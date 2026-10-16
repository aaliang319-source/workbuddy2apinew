// automation.go 自动化管理器：定时循环 + 手动触发 + 状态 + 排程热更新。
//
// 与 scheduler 的分工：本包决定"何时跑、跑完记什么、对外怎么报告"，scheduler 负责
// "具体怎么跑"。定时循环的排程语义来自纯函数 scheduler.NextWake，因此热更新排程
// 只需替换本包持有的 spec 并唤醒循环，不必重建 Scheduler（Pool/Upstream 全程不变）。
package automation

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/config"
	"workbuddy2api/internal/scheduler"
)

// ErrBusy 同类型任务已在运行（定时与手动、或两次手动撞车）。
var ErrBusy = errors.New("task already running")

// Runner 任务执行面（*scheduler.Scheduler 实现；测试注入 fake）。
type Runner interface {
	RunKind(ctx context.Context, k scheduler.Kind) []scheduler.Outcome
}

// Config 管理器配置。
type Config struct {
	Runner      Runner
	Spec        scheduler.ScheduleSpec // 初始排程（通常来自 config.schedule）
	HistoryFile string                 // data/automation.json；空 = 不落盘
	HistoryRuns int                    // ring 上限，默认 100
	// LoopEnabled false 时 Run 只等待退出（不驱动定时），手动触发/历史/状态仍可用。
	LoopEnabled bool
	Now         func() time.Time // 测试注入时钟；nil = time.Now
	// Rand 抖动源；nil 时自建。生产路径由 cmd/server 传入 scheduler 的实例——
	// 循环必须与执行侧共用同一个实例：掷随机数的是循环，面板读的是循环记下的落点，
	// 两个实例各掷各的，显示就会与真实执行不符（见 scheduler/random.go）。
	Rand *scheduler.Randomizer
}

// Manager 自动化管理器。
type Manager struct {
	cfg   Config
	store *Store

	mu      sync.Mutex
	spec    scheduler.ScheduleSpec
	running map[scheduler.Kind]string // kind → 当前 run ID（单飞）
	wake    chan struct{}
	rnd     *scheduler.Randomizer
}

// New 构建管理器（加载历史 + 启动后台落盘）。
func New(cfg Config) *Manager {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Rand == nil {
		cfg.Rand = scheduler.NewRandomizer(cfg.Spec.JitterSpan()) // 跨度已由 JitterSpan() 归一
	}
	m := &Manager{
		cfg:     cfg,
		store:   newStore(cfg.HistoryFile, cfg.HistoryRuns),
		spec:    cfg.Spec,
		running: map[scheduler.Kind]string{},
		wake:    make(chan struct{}, 1),
		rnd:     cfg.Rand,
	}
	// 启动装配：把注入实例的跨度对齐到本管理器的初始排程（保留落点——此刻还没有）。
	m.rnd.SyncSpan(cfg.Spec)
	return m
}

func (m *Manager) now() time.Time { return m.cfg.Now() }

// Flush 同步落盘历史（网关 SIGTERM 路径调用，与 pool/metrics 同口径）。
func (m *Manager) Flush() { m.store.Flush() }

// Close 停后台落盘并做最后一次历史落盘（幂等）。
func (m *Manager) Close() { m.store.Close() }

// Spec 返回当前排程（副本，调用方可安全读）。
func (m *Manager) Spec() scheduler.ScheduleSpec {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.spec
}

// Reconfigure 热替换排程：校验 → 换内存 spec → 唤醒循环重算下次触发。
//
// 只改内存、绝不写 config.json（网关侧该文件为只读挂载；面板是唯一文件写者，
// 保存文件后再调本接口，避免双写者互相覆盖）。
func (m *Manager) Reconfigure(spec scheduler.ScheduleSpec) error {
	if err := validateSpec(spec); err != nil {
		return err
	}
	m.mu.Lock()
	m.spec = spec
	m.mu.Unlock()
	// 作废全部已排定的落点：用户可能改了小时、也可能改了跨度，旧值一律不再代表
	// "下次执行"。循环被唤醒后立刻重掷补上，面板短暂显示「待排定」。
	m.rnd.ResetForSpec(spec)
	// 非阻塞唤醒：循环在 select 里收到后重算 nextWake（真正掷新的随机落点）。
	select {
	case m.wake <- struct{}{}:
	default:
	}
	return nil
}

// validateSpec 校验排程：小时 0-23（复用 config 的启动期校验，语义一致）；
// 启用状态的任务至少要有 1 个小时点，避免"看起来开着但永远不跑"。
func validateSpec(spec scheduler.ScheduleSpec) error {
	checks := []struct {
		field    string
		sw       string
		hours    []int
		disabled bool
	}{
		{"schedule.checkin_hours", "checkin_enabled", spec.CheckinHours, spec.CheckinDisabled},
		{"schedule.travel_hours", "travel_enabled", spec.TravelHours, spec.TravelDisabled},
		{"schedule.activity_hours", "activity_enabled", spec.ActivityHours, spec.ActivityDisabled},
		{"schedule.keepalive_hours", "keepalive_enabled", spec.KeepaliveHours, spec.KeepaliveDisabled},
		{"schedule.school_hours", "school_enabled", spec.SchoolHours, spec.SchoolDisabled},
		{"schedule.cat_hours", "cat_enabled", spec.CatHours, spec.CatDisabled},
	}
	for _, c := range checks {
		if err := config.CheckHourRange(c.field, c.sw, c.hours); err != nil {
			return err
		}
		if !c.disabled && len(c.hours) == 0 {
			return fmt.Errorf("%s: 任务启用时至少需要一个小时点（或设 schedule.%s=false 关闭）", c.field, c.sw)
		}
	}
	return nil
}

// schedule 排定下一次触发：读当前排程 → 现掷随机落点 → 记入 m.rnd 供面板显示。
//
// **这是生产路径上唯一会掷随机数的地方。** 面板显示「下次执行」时读的是这里掷出的
// 结果（Randomizer.Scheduled），绝不能自己再调一次 NextWake——那会掷出另一个随机值，
// 显示的时刻与真实执行不符。
func (m *Manager) schedule(now time.Time) (time.Time, []scheduler.Kind) {
	return scheduler.NextWake(now, m.Spec(), m.rnd)
}

// Run 定时循环：算下次触发 → 定时/被热更新唤醒 → 到点并行执行同槽任务。
// 阻塞直到 ctx 取消（与 scheduler.Run 的行为对齐）。
func (m *Manager) Run(ctx context.Context) {
	if !m.cfg.LoopEnabled {
		<-ctx.Done()
		return
	}
	for {
		now := m.now()
		// 每次排定现掷随机落点（并记入 m.rnd，供面板显示）。
		next, kinds := m.schedule(now)
		if next.IsZero() {
			// 六类全部禁用：不空转，只等退出或热更新唤醒。
			select {
			case <-ctx.Done():
				return
			case <-m.wake:
				continue
			}
		}
		d := next.Sub(now)
		if d < 0 {
			d = 0
		}
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-m.wake:
			// 排程被热更新：丢弃本次定时，回到循环顶部重算。
			timer.Stop()
			continue
		case <-timer.C:
			// 同槽多类任务并行执行（与 scheduler.runBatch 同口径：等全部收尾）。
			var wg sync.WaitGroup
			for _, k := range kinds {
				wg.Add(1)
				go func(k scheduler.Kind) {
					defer wg.Done()
					m.runSync(ctx, k, TriggerSchedule)
				}(k)
			}
			wg.Wait()
		}
	}
}

// runSync 同步执行一类任务（定时路径）：已在本跑则静默跳过（不重复打上游）。
func (m *Manager) runSync(ctx context.Context, kind scheduler.Kind, trigger string) (Run, bool) {
	run, ok := m.start(kind, trigger)
	if !ok {
		// 到点了但没执行：此前只打一行日志，面板历史里查不到"这个时点为什么没跑"。
		// 落一条 skipped 记录，把门控原因写进历史（详见 recordGateSkip）。
		log.Printf("[automation] %s 已在运行，跳过本次%s触发", kind, trigger)
		m.recordGateSkip(kind, trigger, "同类型任务上一次执行尚未结束，本次触发未执行（不重复请求上游）")
		return Run{}, false
	}
	outs := m.cfg.Runner.RunKind(ctx, kind)
	return m.finish(run, kind, outs), true
}

// recordGateSkip 记录一次"到点了、因为门控没执行"的运行：互斥（上一次还没收尾）等。
// 不做任何实际执行，因此 StartedAt=FinishedAt、计数恒为 1 跳过，面板历史里能直接
// 看到时点与原因，而不是只在网关日志里。手动触发撞车仍走 409（面板已有提示）。
func (m *Manager) recordGateSkip(kind scheduler.Kind, trigger, reason string) {
	now := m.now()
	run := Run{
		ID:        newRunID(kind, now),
		Kind:      string(kind),
		Trigger:   trigger,
		StartedAt: now, FinishedAt: now,
		Items: []Item{{OK: false, Status: scheduler.StatusSkipped, Message: reason}},
	}
	m.summarize(&run)
	m.store.Upsert(run)
}

// Trigger 手动触发：立即返回"运行中"记录（HTTP 202 语义），执行在后台完成。
// 同类型已在运行 → ErrBusy。手动执行使用独立 ctx（不受请求取消/网关停机影响：
// 脚本类任务带真实写副作用，中途取消比跑完更危险）。
func (m *Manager) Trigger(kind scheduler.Kind) (Run, error) {
	if !scheduler.ValidKind(string(kind)) {
		return Run{}, fmt.Errorf("unknown task kind: %s", kind)
	}
	run, ok := m.start(kind, TriggerManual)
	if !ok {
		return Run{}, ErrBusy
	}
	go func() {
		outs := m.cfg.Runner.RunKind(context.Background(), kind)
		m.finish(run, kind, outs)
	}()
	return run, nil
}

// start 占位（单飞）：返回初始 Run（Running=true）并写入历史。
func (m *Manager) start(kind scheduler.Kind, trigger string) (Run, bool) {
	m.mu.Lock()
	if _, busy := m.running[kind]; busy {
		m.mu.Unlock()
		return Run{}, false
	}
	run := Run{
		ID:        newRunID(kind, m.now()),
		Kind:      string(kind),
		Trigger:   trigger,
		StartedAt: m.now(),
		Running:   true,
	}
	m.running[kind] = run.ID
	m.mu.Unlock()

	m.store.Upsert(run)
	log.Printf("[automation] %s 开始（%s，run=%s）", kind, trigger, run.ID)
	return run, true
}

// finish 汇总结果、更新历史、释放单飞占位。
func (m *Manager) finish(run Run, kind scheduler.Kind, outs []scheduler.Outcome) Run {
	items := make([]Item, 0, len(outs))
	for _, o := range outs {
		// 脚本类任务（school/cat）的真实结论在子进程输出的解析结果里：
		// 命令级 Outcome 只带 Output/ExitCode 与解析告警，不再当作一条账号结果
		// （否则"整批一条 ok"会把 1 跳过 + 2 失败 的实情记成 成功 1）。
		children := o.Items
		if len(children) == 0 {
			children = []scheduler.Outcome{o}
		} else if o.ParseWarning != "" {
			run.ParseWarning = o.ParseWarning
		}
		for _, c := range children {
			items = append(items, Item{
				UID: c.UID, Nickname: c.Nickname, OK: c.OK, Status: c.Status,
				Message: c.Message, Reward: c.Reward, Pending: c.Pending, Credits: c.Credits,
			})
		}
	}
	// 脚本类任务：合并输出与退出码随运行记录回传（单命令脚本 = 一条命令级 outcome）。
	if len(outs) == 1 && outs[0].UID == "" {
		run.Output = outs[0].Output
		run.ExitCode = outs[0].ExitCode
		// 解析不出逐账号明细时整批失败才算"整批 Error"（失败原因只剩 exit status N，
		// 必须在详情页显式提示；有明细时失败原因在条目里，不重复刷一条红条）。
		if run.Error == "" && !outs[0].OK && len(outs[0].Items) == 0 {
			run.Error = outs[0].Message
		}
	}
	// 计数用**截断前**的全量条目（口径不变：以前是逐条累加，与 Items 存不存无关）；
	// 只有存进历史的 Items 受 maxItemsPerRun 限制，并用 Truncated 标出"仅显示部分"。
	run.Items = items
	m.summarize(&run)
	if len(items) > maxItemsPerRun {
		run.Items = items[:maxItemsPerRun]
		run.Truncated = true
	}
	run.Running = false
	run.FinishedAt = m.now()
	// 记下本槽已真实执行：执行后循环重排时同槽作废（nextFireKind 跳过 → 直接掷次日），
	// 否则抖动窗口内剩余时间仍在"将来"，同一小时窗口会被重复执行 2~3 次。
	// 槽位按 StartedAt 取（那次触发所属的小时），手动触发与定时执行同口径。
	m.rnd.MarkFired(string(kind), run.StartedAt)
	m.store.Upsert(run)
	// 唤醒循环重算下次触发：手动触发后旧定时器可能还指着本槽的旧落点，
	// 不唤醒的话到点仍会再跑一次。非阻塞（循环可能正忙于执行）。
	select {
	case m.wake <- struct{}{}:
	default:
	}

	m.mu.Lock()
	delete(m.running, kind)
	m.mu.Unlock()

	log.Printf("[automation] %s 完成（%s，run=%s）：ok=%d failed=%d skipped=%d",
		kind, run.Trigger, run.ID, run.OK, run.Failed, run.Skipped)
	return run
}

// summarize 从 Items 派生运行级计数与列表摘要（Run 的计数 / Accounts / Reward* / Reason）。
//
// 口径：账号类任务一条 Item = 一个账号；脚本类任务一条 Item = 一个账号的汇总结论
// （见 scheduler.parseScriptOutput）。unreached（脚本没返回结果的账号）计入 Skipped——
// 它确实是"没执行"，详情页里再按 unreached 单独标出，列表数字与账号数保持自洽。
// Reason 供列表视图一眼看出"这次为什么不对"：失败 > 未执行 > 跳过的首个说明。
// RewardTotal/RewardPendingTotal 滚动汇总收益：列表没有 Items，靠它们才能看出"这轮赚了多少"。
func (m *Manager) summarize(run *Run) {
	run.OK, run.Failed, run.Skipped, run.Accounts, run.Reason = 0, 0, 0, 0, ""
	run.RewardTotal, run.RewardPendingTotal = 0, 0
	for _, it := range run.Items {
		if it.UID != "" {
			run.Accounts++
		}
		run.RewardTotal += it.Reward
		run.RewardPendingTotal += it.Pending
		switch it.Status {
		case scheduler.StatusSkipped, scheduler.StatusUnreached:
			run.Skipped++
		default:
			if it.OK {
				run.OK++
			} else {
				run.Failed++
			}
		}
	}
	for _, want := range []string{scheduler.StatusFail, scheduler.StatusUnreached, scheduler.StatusSkipped} {
		for _, it := range run.Items {
			if it.Status == want && it.Message != "" {
				run.Reason = it.Message
				break
			}
		}
		if run.Reason != "" {
			break
		}
	}
}

// Status 汇总各任务类型状态（供面板总览）。
func (m *Manager) Status() Status {
	m.mu.Lock()
	spec := m.spec
	running := make(map[scheduler.Kind]string, len(m.running))
	for k, id := range m.running {
		running[k] = id
	}
	m.mu.Unlock()

	now := m.now()
	out := Status{Enabled: m.cfg.LoopEnabled, Now: now, Kinds: make([]KindStatus, 0, len(scheduler.AllKinds))}
	for _, k := range scheduler.AllKinds {
		hours, enabled := hoursOf(spec, k)
		ks := KindStatus{
			Kind: string(k), Enabled: enabled, Hours: hours,
			NextFire:      m.nextFireForKind(k),
			JitterMinutes: spec.JitterSpan(), // 0 = 准点（jitter_disabled）
		}
		if id, ok := running[k]; ok {
			ks.Running = true
			ks.CurrentRunID = id
		}
		if last, ok := m.store.LastByKind(string(k)); ok {
			lr := last
			ks.LastRun = &lr
		}
		out.Kinds = append(out.Kinds, ks)
	}
	return out
}

// Runs 最近 limit 条运行记录（新 → 旧，不含明细）。
func (m *Manager) Runs(limit int) []Run { return m.store.List(limit) }

// GetRun 取单条完整记录（与定时循环 Run(ctx) 区分命名）。
func (m *Manager) GetRun(id string) (Run, bool) { return m.store.Get(id) }

// hoursOf 取某类型的小时与启用状态。
func hoursOf(spec scheduler.ScheduleSpec, k scheduler.Kind) ([]int, bool) {
	switch k {
	case scheduler.KindCheckin:
		return spec.CheckinHours, !spec.CheckinDisabled
	case scheduler.KindTravel:
		return spec.TravelHours, !spec.TravelDisabled
	case scheduler.KindActivity:
		return spec.ActivityHours, !spec.ActivityDisabled
	case scheduler.KindKeepalive:
		return spec.KeepaliveHours, !spec.KeepaliveDisabled
	case scheduler.KindSchool:
		return spec.SchoolHours, !spec.SchoolDisabled
	case scheduler.KindCat:
		return spec.CatHours, !spec.CatDisabled
	}
	return nil, false
}

// nextFireForKind 单个任务类型的"下次执行"时刻——**读定时循环排定时记下的落点**。
//
// 为什么不再自己调 NextWake 重算：抖动是每次排定现掷的随机数，重算就是另掷一次，
// 面板显示的时刻会与真实执行不符。回退仅限"还没排定过"（启动瞬间/该任务刚被启用/
// 刚热更新过），此时给出 nil，面板显示「待排定」——**绝不编造一个时刻**。
//
// 对**已禁用**的任务一律返回 nil：循环不再为它排定，但落点记忆里还留着禁用前那一次
// 的值（不会过期，也不该被展示成一个"下次执行"）。只让启用中的任务暴露落点。
func (m *Manager) nextFireForKind(k scheduler.Kind) *time.Time {
	if _, enabled := hoursOf(m.Spec(), k); !enabled {
		return nil
	}
	if at, ok := m.rnd.Scheduled(string(k)); ok {
		return &at
	}
	return nil
}

// nextFireForHours 按"只关心哪个小时"算下次触发——关闭抖动的排程计算，**仅供测试**
// 断言排程语义（小时是否被正确解析、跨天）使用。
//
// 生产路径的「下次执行」一律读循环记下的落点（见 nextFireForKind）：抖动每次现掷，
// 这里重算出来的是另一个随机值，拿去显示就是错的。关掉抖动后它是确定性的，
// 因此适合当"用户配的小时有没有被正确解析"的断言口径。
func nextFireForHours(now time.Time, hours []int, disabled bool) *time.Time {
	if disabled || len(hours) == 0 {
		return nil
	}
	single := scheduler.ScheduleSpec{JitterDisabled: true, CheckinHours: hours}
	at, kinds := scheduler.NextWake(now, single, nil)
	if len(kinds) == 0 || at.IsZero() {
		return nil
	}
	return &at
}

// newRunID 运行 ID：<kind>-<UTC 时间戳>-<序号>，可读且天然按时间排序。
var runSeq uint64

func newRunID(kind scheduler.Kind, now time.Time) string {
	runSeq++
	return fmt.Sprintf("%s-%s-%d", kind, now.UTC().Format("20060102T150405"), runSeq%1000)
}

// ParseKind 把字符串转成 Kind（未知返回 false）。
func ParseKind(s string) (scheduler.Kind, bool) {
	s = strings.TrimSpace(s)
	if !scheduler.ValidKind(s) {
		return "", false
	}
	return scheduler.Kind(s), true
}
