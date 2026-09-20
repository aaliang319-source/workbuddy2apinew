// main.go workbuddy2api 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/automation"
	"workbuddy2api/internal/keys"
	"workbuddy2api/internal/metrics"
	"workbuddy2api/internal/notify"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/redisstore"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		// 配置文件不存在时给一次机会用纯默认 + env
		if os.IsNotExist(err) {
			log.Printf("config %s not found, using defaults+env", *cfgPath)
			cfg, err = Load("")
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	// global realm 路由开关（config global.enabled，缺省 true）：注入 auth 包全局闸。
	// Realm()/IsGlobal() 先过此闸——显式 false 时恒 cn（逃生门：纯 CN 锁定的第一道闸）。
	auth.SetGlobalEnabled(cfg.Global.Enabled)

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)

	p := pool.New(cfg.StateFile)
	defer p.Close() // 进程退出前停后台落盘 goroutine + 最后补一次落盘（FIX-4:goroutine 泄漏）
	p.SetStore(store)
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
	p.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）

	// 多业务 Key 存储（data/keys.json，热生效）。首启文件不存在时把 config api_key
	// 迁移为 default 业务 Key（存量客户端零感知）；此后 keys.json 为唯一权威源。
	keyStore, err := keys.Load(cfg.KeysFile, cfg.APIKey)
	if err != nil {
		log.Fatalf("load keys: %v", err)
	}
	log.Printf("loaded %d api key(s) from %s", len(keyStore.List()), cfg.KeysFile)

	// 请求统计（/v1/stats，server.metrics_enabled 开关，缺省关闭）。
	// nil Tracker = 统计关闭：handler 不记录观测，端点返回 enabled=false 形态。
	var mt *metrics.Tracker
	if cfg.Server.MetricsEnabled {
		mt = metrics.New(cfg.MetricsFile)
		defer mt.Close()
		log.Printf("请求统计已启用（/v1/stats，metrics_file=%s）", cfg.MetricsFile)
	}

	// 熔断器 + 在途上限 + 三因子加权调优（从 config 注入，非正值回退默认）。
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetSoftRateMax(cfg.SoftRateMaxDur) // 软冷却指数退避封顶（soft_rate_max，默认 2h）
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)

	// 邮件通知（可选）：pool 事件回调（额度耗尽/账号路由切换）+ 周期额度扫描。
	// Start/StartScan 需要 ctx，故此处只构建并注入回调，启动放到信号 ctx 创建之后。
	var nt *notify.Notifier
	ncfg, nerr := cfg.BuildNotifyConfig()
	if nerr != nil {
		log.Fatalf("notify config: %v", nerr)
	}
	if ncfg.Enabled {
		nt = notify.New(ncfg, notify.NewSMTPSender(ncfg), func(realm string) []string { return p.Availability(realm) })
		p.SetNotifier(nt.OnPoolEvent)
		p.SetAvailability(p.AvailableUIDsForRealm)
		defer nt.Close()
		log.Printf("邮件通知已启用：扫描时刻 %v（本地时区），收件人 %d 个", cfg.Notify.ScanHours, len(ncfg.To))
		if cfg.NotifyExpiringWin != cfg.ExpiringSoonDur {
			log.Printf("WARN: notify.expiring_days(%s) 与 pool.expiring_soon(%s) 不一致：额度过期告警按前者，选号按后者",
				cfg.NotifyExpiringWin, cfg.ExpiringSoonDur)
		}
		if len(cfg.Notify.ScanHours) == 0 {
			log.Printf("提示：notify.scan_hours 为空，仅实时事件（402/冷却/熔断/禁用）会触发通知")
		}
	}

	// 会话粘性路由（可配关闭）。
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  p.AvailableUIDs,
			// 按模型的可用性口径：绑定号在当前模型被 6004 限额时重分配，
			// 而不是被钉在这个号上反复失败。
			// realm 感知闭包：带前缀模型名按 realm 过滤可用账号（跨 realm 不泄漏，
			// 见 wiring.go）；裸名走 cn（现状零回归）。
			AvailableForModel: realmAwareAvailableForModel(p, cfg.Global.MixRealms),
		})
		sessRouter.LoadFromStore() // 启动时从 Redis 恢复粘性（读操作仅此处）
		sessRouter.StartGC()
		defer sessRouter.StopGC()
	}
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	up := upstream.New()
	// 短 RPC 总时长上限（refresh/checkin/balance/FetchModels），语义不变。
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 聊天 SSE 首字节前（响应头）上限：cfg 已 normalize（缺省回落 timeout_seconds）。
	up.HeaderTimeout = time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second
	if tr, ok := up.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HeaderTimeout
	}
	// 聊天 SSE 流中空闲上限（S3 空闲监控读取）。
	up.IdleTimeout = time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second
	up.SanitizeFingerprints = cfg.Features.SanitizeBlacklistFingerprints
	// 出站 UA（A 段）：非空才做显式覆盖，空 = 默认 WorkBuddy 三段式
	// `WorkBuddy/<client_version> WorkBuddy/<client_version> CLI/<cli_version>`。
	up.UserAgent = cfg.Upstream.UserAgent
	// 版本段（upstream.client_version / cli_version）：空 = 各走内置默认。
	up.ClientVersion = cfg.Upstream.ClientVersion
	up.CliVersion = cfg.Upstream.CliVersion
	// 设备风控头（X-Device-Token）全局兜底 + 文件读取路径；空 = 不注入。
	up.DeviceToken = cfg.Upstream.DeviceToken
	up.DeviceTokenFile = cfg.Upstream.DeviceTokenFile
	// 用量归属头（X-Product/X-IDE-*）+ 客户端 IP 透传开关（见 ChatHeaders / handler）。
	up.ClientName = cfg.Upstream.ClientName
	up.PassthroughIP = cfg.Upstream.PassthroughIP
	// global realm 双域路由（config global 段）：base 空回落内置默认 https://www.workbuddy.ai；
	// GlobalEnabled 与 auth 包开关一致（双保险第二道闸在 upstream.globalOn）。
	up.ChatBaseGlobal = cfg.Global.ChatBase
	up.BillingBaseGlobal = cfg.Global.BillingBase
	up.GlobalEnabled = cfg.Global.Enabled

	sch := scheduler.New(scheduler.Config{
		Pool:                p,
		Upstream:            up,
		CheckinHours:        cfg.Schedule.CheckinHours,
		TravelHours:         cfg.Schedule.TravelHours,
		ActivityHours:       cfg.Schedule.ActivityHours,
		KeepaliveHours:      cfg.Schedule.KeepaliveHours,
		SchoolHours:         cfg.Schedule.SchoolHours,
		CatHours:            cfg.Schedule.CatHours,
		ActivityReportCount: cfg.Schedule.ActivityReportCount,
		ExpiringSoonWindow:  cfg.ExpiringSoonDur,  // 快过期积分优先消耗（issue:积分过期）
		ScriptTimeout:       cfg.ScriptTimeoutDur, // 脚本类任务（school/cat）子进程超时
		CheckinDisabled:     !cfg.Schedule.CheckinEnabled,
		TravelDisabled:      !cfg.Schedule.TravelEnabled,
		ActivityDisabled:    !cfg.Schedule.ActivityEnabled,
		KeepaliveDisabled:   !cfg.Schedule.KeepaliveEnabled,
		SchoolDisabled:      !cfg.Schedule.SchoolEnabled,
		CatDisabled:         !cfg.Schedule.CatEnabled,
	})
	switch {
	case !cfg.Schedule.CheckinEnabled:
		log.Printf("签到已禁用（schedule.checkin_enabled=false）")
	default:
		log.Printf("签到已启用：%v 点（签到 + 余额查询解冻）", cfg.Schedule.CheckinHours)
	}
	switch {
	case !cfg.Schedule.TravelEnabled:
		log.Printf("猫猫旅行已禁用（schedule.travel_enabled=false）")
	default:
		log.Printf("猫猫旅行已启用：%v 点（独立排程：领养 / 派出 / 领奖）", cfg.Schedule.TravelHours)
	}
	switch {
	case !cfg.Schedule.ActivityEnabled:
		log.Printf("活跃上报已禁用（schedule.activity_enabled=false）")
	default:
		log.Printf("活跃上报已启用：%v 点（每号 %d 条，点亮连登 + 补满领猫对话门槛）", cfg.Schedule.ActivityHours, cfg.Schedule.ActivityReportCount)
	}
	if !cfg.Schedule.KeepaliveEnabled {
		log.Printf("token 保活已禁用（schedule.keepalive_enabled=false）")
	} else {
		log.Printf("token 保活已启用：%v 点", cfg.Schedule.KeepaliveHours)
	}
	if !cfg.Schedule.SchoolEnabled {
		log.Printf("开学季任务已禁用（schedule.school_enabled=false）")
	} else {
		log.Printf("开学季任务已启用：%v 点（school_open_day_2026.py ALL --run --yes）", cfg.Schedule.SchoolHours)
	}
	if !cfg.Schedule.CatEnabled {
		log.Printf("夜猫子任务已禁用（schedule.cat_enabled=false）")
	} else {
		log.Printf("夜猫子任务已启用：%v 点（task_runner.py ALL --yes --only black_cat）", cfg.Schedule.CatHours)
	}

	// 自动化功能层：定时循环 + 运行历史持久化 + 手动触发/状态 API。
	// LoopEnabled=false 时回退 scheduler 原循环（kill switch，行为与引入前逐字一致）。
	auto := automation.New(automation.Config{
		Runner:      sch,
		Spec:        sch.Spec(),
		HistoryFile: cfg.Automation.HistoryFile,
		HistoryRuns: cfg.Automation.HistoryRuns,
		LoopEnabled: cfg.Automation.Enabled,
	})
	defer auto.Close()
	log.Printf("自动化已启用（/admin/automation/*，历史=%s，定时循环=%v）",
		cfg.Automation.HistoryFile, cfg.Automation.Enabled)

	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     up,
		APIKey:       cfg.APIKey,
		Keys:         keyStore, // nil 不可能：Load 恒返回非 nil（失败即 Fatal）
		AdminKey:     cfg.APIKey,
		Metrics:      mt,   // nil = 统计关闭
		Automation:   auto, // 定时自动化（/admin/automation/*）
		Session:      sessRouter,
		StickyCount:  sessCount,
		RedisMode:    redisMode,
		SoftCooldown: cfg.SoftRateDur,
		PromptMode:   cfg.Prompt.Mode,
		PromptText:   cfg.PromptText,
		MaxBodyBytes: int64(cfg.Server.MaxBodyMB) << 20, // MB → 字节
		// global realm 开关（handler 侧第三道闸：modelList 据此决定是否列 global 名单）。
		GlobalEnabled: cfg.Global.Enabled,
		// 混合调度（config global.mix_realms）：CN/global 账号同池竞争 + 模型名统一。
		MixRealms: cfg.Global.MixRealms,
		// Anthropic /v1/messages 模型映射（claude-* 等客户端模型名 → 网关模型）。
		AnthropicDefaultModel: cfg.Anthropic.DefaultModel,
		AnthropicModelMap:     cfg.Anthropic.ModelMap,
		// 通知测试发送（管理端点用）；未启用通知时为 nil → 端点返回 400 notify_disabled。
		NotifyTest: notifyTestFn(nt),
		// 模型回退白名单（切模型故障转移）。
		ModelFallback: cfg.ModelFallback,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// 邮件通知的发送 worker 与额度周期扫描（未启用时 nt==nil，整段跳过）。
	if nt != nil {
		nt.Start(ctx)
		nt.StartScan(ctx, p, cfg.Notify.ScanHours)
	}
	if cfg.Automation.Enabled {
		go auto.Run(ctx) // automation 模块驱动定时（排程可热更新）
	} else {
		go sch.Run(ctx) // kill switch：回退 scheduler 原循环（行为与引入前一致）
	}

	// auths 运行期自动发现（根因修复：「已落盘但池里没有」事故两连——面板/脚本
	// 落盘新账号后无人重启网关，而服务只在启动时扫一次 auths）。每 N 秒重扫目录，
	// 仅追加池中不存在的新 uid：不覆盖既有凭证（token 刷新后内存可能比磁盘新）、
	// 不删除消失账号（文件临时缺失不应打断在途请求），删除语义仍由重启时
	// SyncToDir 全量对齐承担。显式 0（server.auth_resync_seconds=0）关闭。
	if cfg.Server.AuthResyncSeconds > 0 {
		interval := time.Duration(cfg.Server.AuthResyncSeconds) * time.Second
		go func() {
			t := time.NewTicker(interval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					fresh, err := auth.LoadDir(cfg.AuthDir)
					if err != nil {
						log.Printf("WARN: [server] auths 自动发现扫描失败: %v", err)
						continue
					}
					if n := p.AddMissing(fresh); n > 0 {
						log.Printf("auths 自动发现: 新增 %d 账号（无需重启）", n)
					}
				}
			}
		}()
		log.Printf("auths 运行期自动发现已启用：每 %ds 扫描 %s", cfg.Server.AuthResyncSeconds, cfg.AuthDir)
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		// ReadTimeout 覆盖整个请求读取（含 body）：防慢速 body 拖死连接。
		// 取值大于 MaxBodyMB 在常规带宽下的上传耗时；聊天请求体上限默认 8MB。
		ReadTimeout: 60 * time.Second,
		// IdleTimeout keep-alive 空闲连接回收：配合 ctx 传播（FIX-2）防连接泄漏堆积。
		// 注意：SSE 流式响应期间连接非空闲，不受此项掐断；不设全局 WriteTimeout
		// （长流式生成合法时长可达数分钟，全局 WriteTimeout 会误杀在途 SSE）。
		IdleTimeout: 120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		p.Flush()    // 信号触发：先落盘再做优雅停机
		auto.Flush() // 自动化运行历史同样补一笔
		if mt != nil {
			mt.Flush() // 统计同样补一笔（与 pool 的 SIGTERM 口径一致）
		}
		if mt != nil {
			mt.Flush() // 统计同样补一笔（与 pool 的 SIGTERM 口径一致）
		}
		// Flush 已把最后一笔状态快照提交给 Redis（fire-and-forget）；store.Close
		// 等 Upstash 在途/排队写排空再关连接——最后一笔镜像必须写完才退出（发现 4）。
		// Noop 的 Close 是空操作；单写上限 5s × 上限 8，Close 内部另有超时兜底。
		if cErr := store.Close(); cErr != nil {
			log.Printf("WARN: [server] redisstore close: %v", cErr)
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if cfg.Global.Enabled {
		log.Printf("global realm 已启用（chat_base=%q billing_base=%q，空=默认 workbuddy.ai）",
			cfg.Global.ChatBase, cfg.Global.BillingBase)
	} else {
		log.Printf("global realm 已禁用（config global.enabled=false，纯 CN）")
	}
	log.Printf("workbuddy2api listening on %s (api_key=%v)", cfg.Listen, cfg.APIKey != "")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}

// notifyTestFn 把可空通知器转成端点用的测试发送闭包（nil → nil，端点据此返回 400）。
func notifyTestFn(nt *notify.Notifier) func() error {
	if nt == nil || !nt.Enabled() {
		return nil
	}
	return nt.SendTest
}
