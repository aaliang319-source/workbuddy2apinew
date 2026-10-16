// Package scheduler 定时任务：签到 / 活跃上报 / 猫猫旅行 / token keepalive / 开学季 / 夜猫子 六类独立排程。
// 签到成功后重新查余额，余额 > 0 的冷却账号自动解冻。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// Config 调度器依赖。
//
// 任务开关用「禁用」命名而非「启用」：零值 Config 即六类任务都启用（hours 回落默认），
// 与引入开关前的行为逐字一致（老调用方/老测试无需改动）。
type Config struct {
	Pool           *pool.Pool
	Upstream       *upstream.Client
	CheckinHours   []int // 默认 [9, 21]
	TravelHours    []int // 默认 [9,21]：一趟派出 + 一趟领奖闭环
	ActivityHours  []int // 默认 [10]
	KeepaliveHours []int // 默认 [22]
	SchoolHours    []int // 默认 [12]：开学季任务（迁移自系统 crontab）
	CatHours       []int // 默认 [1]：夜猫子任务（迁移自系统 crontab）
	// ActivityReportCount 每号每次活跃上报的条数：领猫前置需 5 次对话，
	// 默认 5 条同一 conversationId 内多轮上报把 chat_5 刷满；0/缺省=1 兼容旧行为。
	ActivityReportCount int

	// JitterSpanMinutes 每个小时窗口内的随机抖动跨度（分钟）；<=0 走默认 55，上限 55。
	// 触发时刻 = 所配小时 + [1s, 跨度] 内随机偏移，**整点永不被选中**。
	// JitterDisabled 明确要求准点执行（跨度归 0）。
	JitterSpanMinutes int
	JitterDisabled    bool

	// ScriptTimeout 脚本类任务（school/cat）子进程超时，<=0 回落 10m。
	ScriptTimeout time.Duration

	// ExpiringSoonWindow 快过期积分窗口：签到查余额时，把到期时间 <= now+window 的
	// 套餐余额标记为"快过期"（pool 据此优先消耗，见 entry.creditsExpiring）。
	// <=0 时禁用分桶（全部归长期，行为与引入前一致）。默认建议 7*24h。
	ExpiringSoonWindow time.Duration

	// CheckinDisabled 显式关闭签到排程（对应 config 的 schedule.checkin_enabled=false）。
	// 禁用后不再有任何签到时点。旅行不再搭签到便车（已剥离为独立排程）。
	CheckinDisabled bool
	// TravelDisabled 显式关闭猫猫旅行排程（schedule.travel_enabled=false）。
	TravelDisabled bool
	// ActivityDisabled 显式关闭活跃上报排程（schedule.activity_enabled=false）。
	ActivityDisabled bool
	// KeepaliveDisabled 显式关闭 token 保活排程（schedule.keepalive_enabled=false）。
	KeepaliveDisabled bool
	// SchoolDisabled 显式关闭开学季任务排程（schedule.school_enabled=false）。
	SchoolDisabled bool
	// CatDisabled 显式关闭夜猫子任务排程（schedule.cat_enabled=false）。
	CatDisabled bool
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config

	// mu/adoptTried 领养当日失败记录：uid → 自然日（CST）。门槛未达的账号当日不再重试，
	// 避免同日多趟对上游重试轰炸；进程重启即清零（无需持久化）。
	mu         sync.Mutex
	adoptTried map[string]string

	// checkinMu 串行化签到：定时入口与手动触发互斥，避免同一时刻重复打上游签到接口。
	checkinMu sync.Mutex

	// rnd 抖动源与落点记忆：Run 循环排定时现掷随机数并记下，面板读记忆值显示
	// 「下次执行」（不能自己重算，否则显示与真实执行不符）。见 random.go。
	rnd *Randomizer
}

// Randomizer 返回本调度器的抖动源与落点记忆（automation 用它给面板提供
// 「下次执行」，而不是自己重算一遍）。
func (s *Scheduler) Randomizer() *Randomizer { return s.rnd }

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9, 21}
	}
	if len(cfg.TravelHours) == 0 {
		cfg.TravelHours = []int{9, 21}
	}
	if len(cfg.ActivityHours) == 0 {
		cfg.ActivityHours = []int{10}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	if len(cfg.SchoolHours) == 0 {
		cfg.SchoolHours = []int{12}
	}
	if len(cfg.CatHours) == 0 {
		cfg.CatHours = []int{1}
	}
	// 0/缺省 = 1 条（兼容旧行为：每号每天 1 条上报点亮连登）。
	if cfg.ActivityReportCount <= 0 {
		cfg.ActivityReportCount = 1
	}
	if cfg.ScriptTimeout <= 0 {
		cfg.ScriptTimeout = scriptTimeoutDefault
	}
	// 跨度由 Spec().JitterSpan() 归一（未配置 → 默认 55，JitterDisabled → 0），
	// 避免同一份默认值在多处重复解析。
	return &Scheduler{
		cfg:        cfg,
		adoptTried: make(map[string]string),
		rnd:        NewRandomizer(cfg.Spec().JitterSpan()),
	}
}

// checkinRefreshSkew 签到前判定"token 是否临近过期"的时间窗口（10 分钟）。
// 长时间停机/容器长期停跑后 access token 往往已过期，不先刷新则签到必然 401 白跑。
const checkinRefreshSkew = 10 * time.Minute

// CheckinStatus 单账号签到结果状态。
type CheckinStatus string

const (
	CheckinOK      CheckinStatus = "ok"      // 签到成功
	CheckinAlready CheckinStatus = "already" // 上游判定今天已签到（幂等重复，视为正常）
	CheckinFail    CheckinStatus = "fail"    // 刷新 token / 签到 / 余额查询失败
	CheckinSkipped CheckinStatus = "skipped" // 禁用账号或无有效凭证，未参与
)

// CheckinOutcome 单账号签到结果（供手动签到回执与日志汇总）。
type CheckinOutcome struct {
	UID      string        `json:"uid"`
	Nickname string        `json:"nickname,omitempty"`
	Status   CheckinStatus `json:"status"`
	Credits  *int64        `json:"credits,omitempty"` // 签到后余额（余额查询成功才有值）
	Detail   string        `json:"detail,omitempty"`  // 失败/跳过原因（"已签到"不填）
}

// ErrBusy 已有一次签到正在执行（手动入口与定时撞车）。
var ErrBusy = errors.New("checkin already running")

// newOpaqueID 生成 32 位 hex 的不透明标识（conversationId 等）。
// 与 internal/session.NewMessageID 同口径：crypto/rand，失败回落 math/rand/v2。
func newOpaqueID() string { return session.NewMessageID() }

// 夜猫任务的时段约束（CST）：script 侧 scripts/task_runner.py 的 within_night_window()
// 硬编码 [23:00, 次日 08:00)，出窗即打印 "非夜猫窗口(23-08 CST)，skip pending" 白跑一趟。
//
// 这里**不需要**为夜猫单独收窄抖动：偏移恒落在 (0, 55] 分钟内且不跨小时，故 h 点
// 触发落在 [h:00:01, h:55:00]，窗口内的整点（23 与 0..7）抖动后仍在窗内
// （23:00:01..23:55 与 7:00:01..7:55 均未越界）；而窗外的整点本来就已经出窗。
// 换来的约束是：**一旦放宽抖动跨度到 >=60 分钟，或把窗口边界改成非整点，
// 必须回头复核夜猫窗口**（TestNextFireCatStaysInNightWindow 守着这条不变量）。
const (
	catWindowStartHour = 23
	catWindowEndHour   = 8
)

// nextFire 返回 now 之后最近的一次触发时刻；hours 为本地小时（0-23）。
// 无 kind（不感知已执行槽位）的一次性计算路径，测试与旧调用方使用。
func (r *Randomizer) nextFire(now time.Time, hours []int) time.Time {
	return r.nextFireKind("", now, hours)
}

// nextFireKind 同 nextFire，但感知「该任务类型已真实执行过的小时槽」：
// kind 非空且该槽已执行（MarkFired 记下）时，当天该槽作废、直接掷次日。
//
// 为什么必须去重：小时是**窗口**（hour:00:01 ~ hour:55 随机落点），执行后循环
// 重排时窗口内剩余时间仍在"将来"，nextFire 会重掷出同槽的另一个落点——同一
// 小时窗口被重复执行 2~3 次（跨度 55 分钟时），历史里表现为"同一时间点三条
// 相同记录"。执行后记下槽位，同槽跳过，下一个取值自然落次日同一小时。
func (r *Randomizer) nextFireKind(kind string, now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		// 已执行的小时槽：当天作废，直接掷次日（次日必在将来）。
		if kind != "" && r.firedSlot(kind, now, h) {
			t := r.jitteredAt(now.AddDate(0, 0, 1), h)
			if earliest.IsZero() || t.Before(earliest) {
				earliest = t
			}
			continue
		}
		// 先连掷 3 次找一个"今天仍在将来"的落点。
		t := time.Time{}
		for i := 0; i < 3; i++ {
			cand := r.jitteredAt(now, h)
			if cand.After(now) {
				t = cand
				break
			}
			t = cand // 兜底：保留最后一个候选
		}
		if !t.After(now) {
			// 今天这个小时确实过了 → 改掷次日同一小时（次日必在将来）。
			t = r.jitteredAt(now.AddDate(0, 0, 1), h)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// taskKind 调度任务类型。
type taskKind int

const (
	taskCheckin taskKind = iota
	taskTravel
	taskActivity
	taskKeepalive
	taskSchool
	taskCat
)

// nextWake 返回 now 之后最近的唤醒时刻与需执行的任务。
// 排程语义唯一来源于 NextWake（outcome.go）；此处只把导出 Kind 映射回内部 taskKind。
// 抖动源用 s.rnd：Run 循环排定时现掷随机数并记下落点，供面板显示。
func (s *Scheduler) nextWake(now time.Time) (time.Time, []taskKind) {
	at, kinds := NextWake(now, s.cfg.Spec(), s.rnd)
	if len(kinds) == 0 {
		return time.Time{}, nil
	}
	out := make([]taskKind, 0, len(kinds))
	for _, k := range kinds {
		if tk, ok := kindToTask(k); ok {
			out = append(out, tk)
		}
	}
	return at, out
}

// Run 主循环，阻塞直到 ctx 取消。
func (s *Scheduler) Run(ctx context.Context) {
	for {
		next, kinds := s.nextWake(time.Now())
		if next.IsZero() {
			// 六类任务全部禁用：不空转，只等退出信号。
			<-ctx.Done()
			return
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			// 到点任务在排程时确定（不依赖唤醒时刻的小时数），迟到唤醒也不会漏跑。
			// 唤醒时全部并行派发：每类一个 goroutine，慢任务族（如活跃上报
			// 54 号 × 5 条 ≈ 7-8 分钟睡眠）不再阻塞同槽其他任务族；返回前
			// 等全部任务收尾（下一轮 nextWake 照旧从"现在"起算，多轮重叠
			// 的风险与串行版相同——nextWake 只挑现在之后的时点）。
			s.runBatch(ctx, kinds)
		}
	}
}

// runBatch 并行派发一批任务（同一唤醒时刻的多类任务），等全部完成返回。
// 供 Run 主循环与测试使用；ctx 取消时由各任务内部的 sleepCtx 快速收尾。
func (s *Scheduler) runBatch(ctx context.Context, kinds []taskKind) {
	var wg sync.WaitGroup
	for _, k := range kinds {
		wg.Add(1)
		go func(k taskKind) {
			defer wg.Done()
			s.dispatch(ctx, k)
		}(k)
	}
	wg.Wait()
}

// dispatch 按任务类型分发到对应执行函数并返回逐账号结果。脚本类（school/cat）
// 失败只记 WARN、不影响其余任务继续执行（与现有各任务"单账号失败不阻断遍历"同口径）。
// ctx 传导给遍历与脚本子进程（取消时立即放弃剩余账号/杀子进程）。
func (s *Scheduler) dispatch(ctx context.Context, k taskKind) []Outcome {
	return s.runKindCtx(ctx, taskToKind(k))
}

// RunCheckinCtx 带 ctx 的立即签到（手动触发与定时循环统一入口）。
// ctx 作用于账号间限速等待：取消后不再对剩余账号发起上游调用（上游调用本身不可取消）。
func (s *Scheduler) RunCheckinCtx(ctx context.Context) []Outcome {
	ocs, err := s.checkinAll(ctx)
	if err != nil {
		log.Printf("scheduled checkin skipped: %v", err)
		return nil
	}
	out := make([]Outcome, 0, len(ocs))
	for _, oc := range ocs {
		out = append(out, outcomeFromCheckin(oc))
	}
	return out
}

// RunCheckinNow 定时/无 ctx 入口触发的立即签到：逐账号结果由 CheckinAll 记日志，
// 此处只兜住"撞车跳过"。返回值供 automation 记录运行历史（既有调用点可丢弃）。
func (s *Scheduler) RunCheckinNow() []Outcome {
	return s.RunCheckinCtx(context.Background())
}

// CheckinAll 全量签到：按需刷新 token → daily-checkin → 查余额 → 解冻冷却账号。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
// 同一时刻只允许一次签到在跑，重复调用返回 ErrBusy（防止手动触发与定时撞车重复打上游）。
//
// session dead 走 Pool.NoteSessionDead 的**连续计数**语义（与 keepalive 一致）：
// 一次刷新失败不再立即杀号，连续 sessionDeadThreshold 次才禁用，刷新成功清计数。
func (s *Scheduler) CheckinAll() ([]CheckinOutcome, error) {
	return s.checkinAll(context.Background())
}

// checkinAll 签到遍历主体；ctx 仅供账号间限速的取消（上游调用本身不可取消，
// 与 RunCheckinCtx 的既有说明一致）。
func (s *Scheduler) checkinAll(ctx context.Context) ([]CheckinOutcome, error) {
	if !s.checkinMu.TryLock() {
		return nil, ErrBusy
	}
	defer s.checkinMu.Unlock()

	statuses := s.cfg.Pool.List()
	out := make([]CheckinOutcome, 0, len(statuses))
	var okN, alreadyN, failN, skipN int
	// first 标记"是否已有一个账号真正打过上游"：只有真正发起调用的账号之间才需要
	// 限速，被跳过（禁用/无凭证/global）的账号不占用节拍。
	first := true
	for _, st := range statuses {
		oc := CheckinOutcome{UID: st.UID, Nickname: st.Nickname}
		if st.Disabled {
			oc.Status, oc.Detail = CheckinSkipped, "账号已在面板禁用，未参与本次签到"
			skipN++
			out = append(out, oc)
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			oc.Status, oc.Detail = CheckinSkipped, "无有效凭证（缺少 refresh token），未参与本次签到"
			skipN++
			out = append(out, oc)
			continue
		}
		// D4 门控：realm=global 账号无签到体系/任务中心，直接跳过（不发起任何上游调用，避免风控）。
		// 经 auth.Realm() 统一判定：逃生门（global.enabled=false）下 global 账号被降级为 cn、
		// 按 CN 处理——这是 D5 逃生门的刻意语义（纯 CN 部署锁死一切 global），与引用处一致。
		if a.IsGlobal() {
			oc.Status, oc.Detail = CheckinSkipped, "global realm 账号不参与 CN 域签到任务"
			skipN++
			out = append(out, oc)
			continue
		}
		// 账号间限速（抖动）：真正要发起上游调用的账号之间才等待，跳过的账号不占节拍。
		if !first {
			if !pacedSleep(ctx, checkinAccountDelay) {
				return out, nil // ctx 取消：剩余账号下一轮再签
			}
		}
		first = false
		// 停机跨过 token 有效期（关机过夜/容器长期停跑）时先补一次刷新，否则签到必然 401 白跑。
		if a.NeedsRefresh(checkinRefreshSkew) {
			if err := s.cfg.Upstream.RefreshToken(a); err != nil {
				log.Printf("checkin %s refresh: %v", logfmt.UID8(st.UID), err)
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					if s.cfg.Pool.NoteSessionDead(st.UID) {
						log.Printf("WARN: checkin %s: 连续 %d 次 12153 session dead — 禁用", logfmt.UID8(st.UID), pool.SessionDeadThreshold())
					}
				}
				// 刷新只是"提前补票"：token 若仍有效，继续照常签到（否则刷新接口抖动
				// 会让本可成功的签到被白白跳过）；真正过期才判定失败。
				if a.NeedsRefresh(0) {
					oc.Status, oc.Detail = CheckinFail, "refresh: "+err.Error()
					failN++
					out = append(out, oc)
					continue
				}
			} else {
				a.BackfillRealm() // 老 auth 空 realm → 落盘前补标识（幂等：已有不动）
				if err := a.SaveAtomic(); err != nil {
					// 刷新成功但落盘失败：重启会用旧 token，必须暴露。
					log.Printf("checkin %s save: %v", logfmt.UID8(st.UID), err)
				}
			}
		}
		// 签到返回错误（含"今天已签到"）也继续查余额：余额恢复即可解冻账号。
		if err := s.cfg.Upstream.DailyCheckin(a); err != nil {
			if upstream.IsAlreadyCheckin(err) {
				// "今天已签到"是幂等成功，不是错误：只写一句人话结论，
				// 不把上游 400 报文原样塞进 detail（那会被误读成签到失败）。
				oc.Status = CheckinAlready
				oc.Detail = "上游判定今日已签到（幂等成功），无需重复"
			} else {
				oc.Status = CheckinFail
				oc.Detail = err.Error()
				log.Printf("checkin %s: %v", logfmt.UID8(st.UID), err)
			}
		} else {
			// 签到成功：清掉刷新阶段可能残留的告警（token 刷新失败但原 token 仍有效时
			// 会写入 Detail）。不清的话详情页会出现「状态=成功、说明=refresh: xxx」
			// 这种自相矛盾的行；刷新告警已由上面的 log.Printf 留痕。
			oc.Status = CheckinOK
			oc.Detail = ""
		}
		// 分桶查余额：快过期窗口内的积分单独标记，pool 优先消耗（issue:积分过期）。
		// ExpiringSoonWindow<=0 时退化为纯总量（与引入前一致）。
		remain, buckets, err := s.cfg.Upstream.UserResourceDetailed(a, s.cfg.ExpiringSoonWindow)
		if err != nil {
			log.Printf("user-resource %s: %v", logfmt.UID8(st.UID), err)
			// Status 即将被改写成 fail，而 Detail 里可能还留着签到自己的结论
			// （如"幂等成功"）。直接 append 会拼出「幂等成功 ... http 500」这种
			// 自相矛盾的一行，详情页里分不清是签到挂了还是查余额挂了。
			// 先把签到结论固化成独立短语，再接查余额错误，两步责任分明。
			oc.Detail = joinDetail("签到: "+checkinOutcomeText(oc.Status, oc.Detail), "查余额失败: "+err.Error())
			oc.Status = CheckinFail
			failN++
			out = append(out, oc)
			continue
		}
		s.cfg.Pool.ReenableIfCredits(st.UID, remain)
		s.cfg.Pool.SetCreditsDetailed(st.UID, remain, buckets.Expiring)
		oc.Credits = &remain
		switch oc.Status {
		case CheckinOK:
			okN++
		case CheckinAlready:
			alreadyN++
		default:
			failN++
		}
		out = append(out, oc)
	}
	log.Printf("checkin done: total=%d ok=%d already=%d fail=%d skipped=%d",
		len(statuses), okN, alreadyN, failN, skipN)
	return out, nil
}

// joinDetail 拼接多段原因，避免后一段覆盖前一段的失败信息。
func joinDetail(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "; " + add
}

// checkinOutcomeText 把签到自身的结论压成一句可读短语。
//
// 用于"签到之后又失败"的场景（典型是查余额 500）：此时 Status 会被改写成 fail，
// 签到到底成没成功只能靠 Detail 交代。返回「成功」/「今日已签到（幂等）」/
// 「失败（<原因>）」，让详情页每一行的状态与说明不再互相打架。
func checkinOutcomeText(st CheckinStatus, detail string) string {
	switch st {
	case CheckinOK:
		return "成功"
	case CheckinAlready:
		return "今日已签到（幂等）"
	case CheckinFail:
		if detail != "" {
			return "失败（" + detail + "）"
		}
		return "失败"
	}
	if detail != "" {
		return detail
	}
	return string(st)
}

// RunActivityNow 立即对池内所有可用账号执行对话活跃上报。
// 禁用账号跳过；无 AccessToken 的跳过；账号间限速 activityAccountDelay。
// CN 与 global 账号**都上报**（PR #45 实测国际版 /v2/report 可用）；单账号失败
// 只记 WARN 不影响遍历。
//
// 每号上报 N 条（ActivityReportCount，默认 5）：N 条共用同一 conversationId
// （wb2api-<ms>），模拟同一会话内 N 轮对话——这是领养猫（buddy/first）对话量
// 门槛的实测刷法（chat_5 前置需 5 次对话）。requestId 各条独立（同会话多轮）。
// 账号内 N 条之间间隔 activityReportGap（1.5s）避免秒发触发风控。
//
// 0/缺省 ActivityReportCount = 1 条，兼容旧行为（仅点亮连登 + 解锁 first_buddy）。
//
// 上报成功后：① streak 自检（回读连登，发现「200 但静默丢弃」）；
// ② 无猫账号立即重试领养（travelAdoptForce）——对话量刚补满的新状态，不算重试，
// 豁免 adoptTriedToday 当日防抖（旅行排程 09 点已领养过且 skip，10 点上报补满后
// 不能依赖下一轮旅行领养，就地闭环）。
// RunActivityNow 立即对池内所有可用账号执行对话活跃上报（无 ctx 的外部入口：
// cmd/activity 一次性触发、测试）。内部走 runActivity，取背景 ctx（不可取消，
// 语义与引入前 time.Sleep 版一致）。
func (s *Scheduler) RunActivityNow() []Outcome {
	return s.runActivity(context.Background())
}

// runActivity 活跃上报遍历，随 ctx 取消立即退出。返回逐账号结果：
// N 条全发满 → ok（message 带 reports 与 streak 自检结论）；部分/失败 → fail。
func (s *Scheduler) runActivity(ctx context.Context) []Outcome {
	out := []Outcome{}
	count := s.cfg.ActivityReportCount
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessToken == "" {
			continue
		}
		// global 账号同样上报（PR #45 实测国际版 /v2/report 在 workbuddy.ai 上 code=0 OK，
		// 点亮连登）；realmBase 路由/头由 upstream.billingJSON/BillingHeaders 按 realm 切。
		// 单账号失败只记 WARN 不影响遍历（下方 report err → break 该号 → continue 下号）。
		if !first {
			if !pacedSleep(ctx, activityAccountDelay) {
				return out // 优雅停机：不等限速睡满，剩余账号下轮再报
			}
		}
		first = false
		// N 条共用同一 conversationId（同会话），requestId 各自独立（每条一个）。
		// 用不可预测的 32 hex：旧实现是 wb2api-<UnixMilli>，既把固定品牌前缀甩在上游
		// 事件里，又把"这个号几点几分报的"直接编码进 ID——两者都是白送的风控特征。
		cid := newOpaqueID()
		ok := 0
		for i := 1; i <= count; i++ {
			rid := fmt.Sprintf("%s-r%d", cid, i)
			if err := s.cfg.Upstream.ReportChatActivity(a, cid, rid); err != nil {
				log.Printf("activity %s: report %d/%d: %v", logfmt.UID8(a.UID), i, count, err)
				break // 本号上报失败：不再续发，streak 自检无意义
			}
			log.Printf("activity %s: report %d/%d ok", logfmt.UID8(a.UID), i, count)
			ok++
			if i < count {
				// 账号内 5 条之间间隔（抖动），避免秒发风控；取消时立即放弃本号剩余条数。
				if !pacedSleep(ctx, activityReportGap) {
					return out
				}
			}
		}
		if ok < count {
			out = append(out, Outcome{
				UID: st.UID, Nickname: st.Nickname, OK: false, Status: "fail",
				Message: fmt.Sprintf("reports=%d/%d (aborted early)", ok, count),
			})
			continue // N 条未发满：streak 自检与领养均无意义，下个账号
		}
		suspicious := s.checkActivityStreak(a) // N 条全发满 → 回读 streak 自检（只留结论行）
		s.travelAdoptForce(a)                  // 无猫账号对话量刚补满 → 立即重试领养（豁免防抖）
		msg := fmt.Sprintf("reports=%d/%d", ok, count)
		if suspicious {
			// 上报 200 但 streak 可疑（静默丢弃/回读失败）：仍记 ok（上报本身成功），
			// 但把可疑信号带进历史，便于面板一眼看出。
			msg += " streak_check=suspicious"
		}
		out = append(out, Outcome{UID: st.UID, Nickname: st.Nickname, OK: true, Status: "ok", Message: msg})
	}
	return out
}

// checkActivityStreak 上报成功后回读连登天数（只读 oracle，发现静默失败）。
// 背景：REPORT-active-map.md §2 实测「上报 200 但静默丢弃」（缺 userId 时 progress 不动），
// 上报 200 ≠ streak 计分——需要回读验证闭环。
// 异常检测口径：days==0 → warn（report OK but streak.days=0 (silent drop?)）；
// GET 失败 → warn 但不影响主流程（上报本身已成功，按天幂等，不做重试）。
// 日志每号一行、一眼可 grep：`activity %s: streak days=%d`（成功也打，方便对账）。
// 返回 true 表示「上报 OK 但 streak 可疑」（days==0 或回读失败），供测试断言。
func (s *Scheduler) checkActivityStreak(a *auth.Auth) bool {
	days, err := s.cfg.Upstream.GrowthStreak(a)
	if err != nil {
		log.Printf("WARN: activity %s: streak check failed (report OK): %v", logfmt.UID8(a.UID), err)
		return true
	}
	if days == 0 {
		log.Printf("WARN: activity %s: report OK but streak.days=0 (silent drop?)", logfmt.UID8(a.UID))
		return true
	}
	log.Printf("activity %s: streak days=%d", logfmt.UID8(a.UID), days)
	return false
}

// RunKeepaliveNow 立即对所有账号刷新 token；session 死亡的自动禁用。
// 12153 禁用走 Pool.NoteSessionDead 的**连续计数**语义：一次刷新失败不再立即杀号，
// 连续 sessionDeadThreshold 次（3 次）才禁用（P0-1：13 个 disabled 号全是历史误判）。
// 刷新成功 → ClearSessionDead 清计数（错误判定的账号有复活路径）。
func (s *Scheduler) RunKeepaliveNow() []Outcome {
	return s.RunKeepaliveCtx(context.Background())
}

// RunKeepaliveCtx 带 ctx 的 token 保活（当前遍历不检查 ctx，签名保留供后续接入）。
// 返回逐账号结果：刷新成功 ok；12153 session dead 且达阈值禁用 → fail(disabled)。
func (s *Scheduler) RunKeepaliveCtx(ctx context.Context) []Outcome {
	_ = ctx // 遍历为纯上游串行调用，暂无取消点
	out := []Outcome{}
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			out = append(out, Outcome{UID: st.UID, Nickname: st.Nickname, OK: false, Status: StatusSkipped,
				Message: "账号已在面板禁用，未参与保活刷新"})
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			out = append(out, Outcome{UID: st.UID, Nickname: st.Nickname, OK: false, Status: StatusSkipped,
				Message: "无有效凭证（缺少 refresh token），未参与保活刷新"})
			continue
		}
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("keepalive %s: %v", logfmt.UID8(st.UID), err)
			msg := err.Error()
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				if s.cfg.Pool.NoteSessionDead(st.UID) {
					log.Printf("WARN: keepalive %s: 连续 %d 次 12153 session dead — 禁用", logfmt.UID8(st.UID), pool.SessionDeadThreshold())
					msg = "session dead (disabled)"
				}
			}
			out = append(out, Outcome{UID: st.UID, Nickname: st.Nickname, OK: false, Status: "fail", Message: msg})
			continue
		}
		s.cfg.Pool.ClearSessionDead(st.UID) // 刷新成功清误判计数，失败不该累计
		a.BackfillRealm()                   // 老 auth 空 realm → 落盘前补标识（幂等：已有不动）
		if err := a.SaveAtomic(); err != nil {
			log.Printf("keepalive %s save: %v", logfmt.UID8(st.UID), err)
		}
		out = append(out, Outcome{UID: st.UID, Nickname: st.Nickname, OK: true, Status: "ok"})
	}
	return out
}
