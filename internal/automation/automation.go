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
}

// Manager 自动化管理器。
type Manager struct {
	cfg   Config
	store *Store

	mu      sync.Mutex
	spec    scheduler.ScheduleSpec
	running map[scheduler.Kind]string // kind → 当前 run ID（单飞）
	wake    chan struct{}
}

// New 构建管理器（加载历史 + 启动后台落盘）。
func New(cfg Config) *Manager {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	m := &Manager{
		cfg:     cfg,
		store:   newStore(cfg.HistoryFile, cfg.HistoryRuns),
		spec:    cfg.Spec,
		running: map[scheduler.Kind]string{},
		wake:    make(chan struct{}, 1),
	}
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
	// 非阻塞唤醒：循环在 select 里收到后重算 nextWake。
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

// Run 定时循环：算下次触发 → 定时/被热更新唤醒 → 到点并行执行同槽任务。
// 阻塞直到 ctx 取消（与 scheduler.Run 的行为对齐）。
func (m *Manager) Run(ctx context.Context) {
	if !m.cfg.LoopEnabled {
		<-ctx.Done()
		return
	}
	for {
		now := m.now()
		next, kinds := scheduler.NextWake(now, m.Spec())
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
		log.Printf("[automation] %s 已在运行，跳过本次%s触发", kind, trigger)
		return Run{}, false
	}
	outs := m.cfg.Runner.RunKind(ctx, kind)
	return m.finish(run, kind, outs), true
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
		items = append(items, Item{
			UID: o.UID, Nickname: o.Nickname, OK: o.OK, Status: o.Status,
			Message: o.Message, Reward: o.Reward, Credits: o.Credits,
		})
		switch o.Status {
		case "skipped":
			run.Skipped++
		default:
			if o.OK {
				run.OK++
			} else {
				run.Failed++
			}
		}
	}
	// 脚本类任务：合并输出与退出码随运行记录回传（单条 outcome）。
	if len(outs) == 1 && outs[0].UID == "" {
		run.Output = outs[0].Output
		run.ExitCode = outs[0].ExitCode
	}
	if len(items) > maxItemsPerRun {
		items = items[:maxItemsPerRun]
		run.Truncated = true
	}
	run.Items = items
	run.Running = false
	run.FinishedAt = m.now()
	m.store.Upsert(run)

	m.mu.Lock()
	delete(m.running, kind)
	m.mu.Unlock()

	log.Printf("[automation] %s 完成（%s，run=%s）：ok=%d failed=%d skipped=%d",
		kind, run.Trigger, run.ID, run.OK, run.Failed, run.Skipped)
	return run
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
			NextFire: nextFireForKind(now, spec, k),
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

// nextFireForKind 单个任务类型的下次触发时刻：把 spec 收窄到只启用该类，
// 复用 NextWake 的排程语义（单一来源，避免两处实现漂移）。
func nextFireForKind(now time.Time, spec scheduler.ScheduleSpec, k scheduler.Kind) *time.Time {
	single := scheduler.ScheduleSpec{
		CheckinDisabled: true, TravelDisabled: true, ActivityDisabled: true,
		KeepaliveDisabled: true, SchoolDisabled: true, CatDisabled: true,
	}
	switch k {
	case scheduler.KindCheckin:
		single.CheckinDisabled, single.CheckinHours = spec.CheckinDisabled, spec.CheckinHours
	case scheduler.KindTravel:
		single.TravelDisabled, single.TravelHours = spec.TravelDisabled, spec.TravelHours
	case scheduler.KindActivity:
		single.ActivityDisabled, single.ActivityHours = spec.ActivityDisabled, spec.ActivityHours
	case scheduler.KindKeepalive:
		single.KeepaliveDisabled, single.KeepaliveHours = spec.KeepaliveDisabled, spec.KeepaliveHours
	case scheduler.KindSchool:
		single.SchoolDisabled, single.SchoolHours = spec.SchoolDisabled, spec.SchoolHours
	case scheduler.KindCat:
		single.CatDisabled, single.CatHours = spec.CatDisabled, spec.CatHours
	}
	at, kinds := scheduler.NextWake(now, single)
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
