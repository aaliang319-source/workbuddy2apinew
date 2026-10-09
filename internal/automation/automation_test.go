// automation_test.go automation 模块：单飞、历史持久化、热更新、状态、裁剪。
package automation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/scheduler"
)

// fakeRunner 可配置输出与耗时的任务执行 fake。
type fakeRunner struct {
	mu    sync.Mutex
	outs  []scheduler.Outcome
	delay time.Duration
	calls []scheduler.Kind
}

func (f *fakeRunner) RunKind(ctx context.Context, k scheduler.Kind) []scheduler.Outcome {
	f.mu.Lock()
	f.calls = append(f.calls, k)
	d := f.delay
	f.mu.Unlock()
	if d > 0 {
		time.Sleep(d)
	}
	return f.outs
}

func (f *fakeRunner) called(k scheduler.Kind) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == k {
			return true
		}
	}
	return false
}

// newScheduledManager 构建管理器并驱动定时循环，等 checkin 排定出落点后返回。
//
// 面板的 next_fire 读的是循环排定时掷出的落点，所以断言"启用任务有时刻、禁用任务
// 无时刻"必须先把循环跑起来——没跑循环时是「待排定」，不是「没实现」。
func newScheduledManager(t *testing.T, spec scheduler.ScheduleSpec) *Manager {
	t.Helper()
	m := New(Config{
		Runner:      &fakeRunner{},
		Spec:        spec,
		HistoryFile: filepath.Join(t.TempDir(), "a.json"),
		LoopEnabled: true,
	})
	t.Cleanup(m.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go m.Run(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := m.rnd.Scheduled(string(scheduler.KindCheckin)); ok {
			return m
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("3s 内 checkin 未排定出落点")
	return m
}

// testSpec 只有 checkin 启用（9 点），其余禁用（hours 非空保持语义一致）。
func testSpec() scheduler.ScheduleSpec {
	return scheduler.ScheduleSpec{
		CheckinHours:      []int{9},
		TravelHours:       []int{9},
		ActivityHours:     []int{10},
		KeepaliveHours:    []int{22},
		SchoolHours:       []int{12},
		CatHours:          []int{1},
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		SchoolDisabled:    true,
		CatDisabled:       true,
	}
}

func waitFinished(t *testing.T, m *Manager, id string) Run {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if r, ok := m.GetRun(id); ok && !r.Running {
			return r
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("run 未在时限内完成")
	return Run{}
}

func TestManagerTriggerAndHistory(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "automation.json")
	fr := &fakeRunner{outs: []scheduler.Outcome{
		{UID: "u1", Nickname: "一号", OK: true, Status: "ok"},
		{UID: "u2", OK: false, Status: "fail", Message: "boom"},
		{UID: "u3", OK: true, Status: "skipped", Message: "disabled"},
	}}
	m := New(Config{Runner: fr, Spec: testSpec(), HistoryFile: fp, HistoryRuns: 100})
	defer m.Close()

	run, err := m.Trigger(scheduler.KindCheckin)
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if !run.Running || run.ID == "" {
		t.Fatalf("initial run=%+v want running", run)
	}
	r := waitFinished(t, m, run.ID)
	if r.OK != 1 || r.Failed != 1 || r.Skipped != 1 {
		t.Errorf("summary=%+v want 1/1/1", r)
	}
	if len(r.Items) != 3 {
		t.Fatalf("items=%d want 3", len(r.Items))
	}
	if r.Items[0].Nickname != "一号" {
		t.Errorf("item0=%+v", r.Items[0])
	}

	// 持久化：Close 后重新加载同一路径应能读到该记录。
	m.Close()
	st := newStore(fp, 100)
	got, ok := st.Get(run.ID)
	if !ok {
		t.Fatal("reloaded store lost run")
	}
	if got.OK != 1 || got.Failed != 1 {
		t.Errorf("reloaded run=%+v", got)
	}
}

func TestManagerTriggerBusy(t *testing.T) {
	fr := &fakeRunner{outs: []scheduler.Outcome{{UID: "u1", OK: true, Status: "ok"}}, delay: 100 * time.Millisecond}
	m := New(Config{Runner: fr, Spec: testSpec(), HistoryFile: filepath.Join(t.TempDir(), "a.json")})
	defer m.Close()

	r1, err := m.Trigger(scheduler.KindCheckin)
	if err != nil {
		t.Fatalf("first trigger: %v", err)
	}
	if _, err := m.Trigger(scheduler.KindCheckin); !errors.Is(err, ErrBusy) {
		t.Fatalf("second trigger err=%v want ErrBusy", err)
	}
	// 不同类型不互斥（单飞按类型隔离）。
	r2, err := m.Trigger(scheduler.KindTravel)
	if err != nil {
		t.Fatalf("travel trigger: %v", err)
	}
	waitFinished(t, m, r1.ID)
	waitFinished(t, m, r2.ID)
}

func TestManagerUnknownKindRejected(t *testing.T) {
	m := New(Config{Runner: &fakeRunner{}, Spec: testSpec()})
	defer m.Close()
	if _, err := m.Trigger("nope"); err == nil {
		t.Fatal("unknown kind should error")
	}
}

func TestReconfigureValidationAndHotApply(t *testing.T) {
	m := newScheduledManager(t, testSpec())

	// 非法小时 → 400 语义。
	bad := testSpec()
	bad.CheckinHours = []int{25}
	if err := m.Reconfigure(bad); err == nil {
		t.Fatal("hour 25 should be rejected")
	}
	// 启用但空小时 → 拒绝（防"看着开着其实永不跑"）。
	empty := testSpec()
	empty.CheckinHours = nil
	if err := m.Reconfigure(empty); err == nil {
		t.Fatal("enabled kind with empty hours should be rejected")
	}
	// 合法热更新：checkin 9 → 20。
	spec := testSpec()
	spec.CheckinHours = []int{20}
	if err := m.Reconfigure(spec); err != nil {
		t.Fatal(err)
	}
	st := m.Status()
	var found *KindStatus
	for i := range st.Kinds {
		if st.Kinds[i].Kind == string(scheduler.KindCheckin) {
			found = &st.Kinds[i]
		}
	}
	if found == nil || !found.Enabled || found.Hours[0] != 20 {
		t.Fatalf("kind status after apply: %+v", found)
	}
	// 热更新后循环需被唤醒才会重排（并掷新的落点），故轮询等待而不是立即断言。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if at, ok := m.rnd.Scheduled(string(scheduler.KindCheckin)); ok && at.Hour() == 20 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	at, _ := m.rnd.Scheduled(string(scheduler.KindCheckin))
	t.Errorf("热更新后 3s 内未重排到 20 点窗口，落点=%v", at)
}

func TestStatusNextFireDisabledNil(t *testing.T) {
	m := newScheduledManager(t, testSpec())
	st := m.Status()
	for _, k := range st.Kinds {
		switch k.Kind {
		case string(scheduler.KindCheckin):
			if k.NextFire == nil {
				t.Error("checkin enabled → next_fire 必须有值")
			}
		default:
			if k.NextFire != nil {
				t.Errorf("%s disabled → next_fire 应为空", k.Kind)
			}
		}
	}
	// 循环在跑：next_fire 读的是掷出的落点，绝不能落在整点。
	for _, k := range st.Kinds {
		if k.NextFire == nil {
			continue
		}
		if k.NextFire.Minute() == 0 && k.NextFire.Second() == 0 {
			t.Errorf("%s next_fire=%v 落在整点——应已被抖动打散", k.Kind, k.NextFire)
		}
	}
}

// TestScheduleRandomPerRound 每次排定都掷新的随机落点——用户的核心诉求
// （「每次都是随机时间执行，不是你固定时间」）。
//
// 直接驱动排定（m.schedule）而不等真实定时器。断言的是"不同轮次掷出的值不同"，
// 即旧实现那个按 (kind, 日, 小时) 哈希出来的固定偏移已被移除。
func TestScheduleRandomPerRound(t *testing.T) {
	m := New(Config{Runner: &fakeRunner{}, Spec: testSpec(), HistoryFile: filepath.Join(t.TempDir(), "a.json")})
	defer m.Close()
	now := time.Date(2026, 9, 25, 8, 0, 0, 0, time.Local)
	seen := map[time.Time]int{}
	for i := 0; i < 40; i++ {
		if _, kinds := m.schedule(now); len(kinds) == 0 {
			t.Fatal("checkin 启用却排不出任何任务")
		}
		at, ok := m.rnd.Scheduled(string(scheduler.KindCheckin))
		if !ok {
			t.Fatal("排定后应记下落点")
		}
		seen[at]++
	}
	if len(seen) < 2 {
		t.Fatalf("40 轮排定只掷出 %d 个不同落点——抖动仍是固定的", len(seen))
	}
	// 每一轮都不得越出所配小时，也不得落在整点。
	for at := range seen {
		if at.Hour() != 9 {
			t.Errorf("落点 %v 不在所配的 9 点窗口内", at)
		}
		if at.Minute() == 0 && at.Second() == 0 {
			t.Errorf("落点 %v 落在整点", at)
		}
	}
}

// TestNextFireForHoursHourSemantics 配的小时被正确解析（含跨天）：nextFireForHours
// 关闭抖动、结果可预期，因此适合断言"哪个小时"，与"随机落点是多少"分开测。
func TestNextFireForHoursHourSemantics(t *testing.T) {
	now := time.Date(2026, 9, 25, 20, 0, 0, 0, time.Local)
	at := nextFireForHours(now, []int{9, 21}, false)
	if at == nil || !at.Equal(time.Date(2026, 9, 25, 21, 0, 0, 0, time.Local)) {
		t.Errorf("at=%v want 当日 21:00", at)
	}
	at = nextFireForHours(now, []int{9}, false)
	if at == nil || !at.Equal(time.Date(2026, 9, 26, 9, 0, 0, 0, time.Local)) {
		t.Errorf("at=%v want 次日 09:00", at)
	}
	if at := nextFireForHours(now, []int{9}, true); at != nil {
		t.Errorf("禁用的任务不应有时点，got %v", at)
	}
	if at := nextFireForHours(now, nil, false); at != nil {
		t.Errorf("空小时不应有时点，got %v", at)
	}
}

func TestFinishItemCapAndTruncated(t *testing.T) {
	var outs []scheduler.Outcome
	for i := 0; i < 600; i++ {
		outs = append(outs, scheduler.Outcome{UID: "u" + strconv.Itoa(i), OK: true, Status: "ok"})
	}
	fr := &fakeRunner{outs: outs}
	m := New(Config{Runner: fr, Spec: testSpec(), HistoryFile: filepath.Join(t.TempDir(), "a.json")})
	defer m.Close()

	run, err := m.Trigger(scheduler.KindCheckin)
	if err != nil {
		t.Fatal(err)
	}
	r := waitFinished(t, m, run.ID)
	if len(r.Items) != maxItemsPerRun {
		t.Errorf("items=%d want capped at %d", len(r.Items), maxItemsPerRun)
	}
	if !r.Truncated {
		t.Error("truncated 标记缺失")
	}
	// 汇总计数不受裁剪影响（600 全 ok）。
	if r.OK != 600 {
		t.Errorf("ok=%d want 600（计数是完整口径）", r.OK)
	}
}

func TestStoreCorruptFileStartsEmpty(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "automation.json")
	if err := os.WriteFile(fp, []byte("garbage{{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := newStore(fp, 100)
	defer st.Close()
	if got := st.List(10); len(got) != 0 {
		t.Errorf("list=%d want 0（坏文件容忍）", len(got))
	}
	// 坏文件之后仍可正常写入。
	st.Upsert(Run{ID: "x", Kind: "checkin"})
	st.Flush()
	st2 := newStore(fp, 100)
	defer st2.Close()
	if _, ok := st2.Get("x"); !ok {
		t.Error("坏文件后应能正常写入并读回")
	}
}

func TestStoreRingPrune(t *testing.T) {
	st := newStore("", 5) // 纯内存
	defer st.Close()
	for i := 0; i < 12; i++ {
		st.Upsert(Run{ID: "r" + strconv.Itoa(i), Kind: "checkin"})
	}
	if got := st.List(100); len(got) != 5 {
		t.Errorf("list=%d want 5（ring 裁剪）", len(got))
	}
	if _, ok := st.Get("r0"); ok {
		t.Error("最旧的 r0 应被裁掉")
	}
	if _, ok := st.Get("r11"); !ok {
		t.Error("最新的 r11 应保留")
	}
}

func TestScheduledRunRecordsTrigger(t *testing.T) {
	fr := &fakeRunner{outs: []scheduler.Outcome{{UID: "u1", OK: true, Status: "ok"}}}
	// LoopEnabled=false → Run 立即返回（不驱动定时），定时路径用 runSync 直测。
	m := New(Config{Runner: fr, Spec: testSpec(), HistoryFile: filepath.Join(t.TempDir(), "a.json")})
	defer m.Close()
	run, ok := m.runSync(context.Background(), scheduler.KindCheckin, TriggerSchedule)
	if !ok {
		t.Fatal("scheduled run skipped unexpectedly")
	}
	if run.Trigger != TriggerSchedule {
		t.Errorf("trigger=%q want schedule", run.Trigger)
	}
}
