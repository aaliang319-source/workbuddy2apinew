package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// assertJittered 断言 t 落在 hour 点窗口内、且**不在整点**——这正是本次改动的全部目的：
// 落点仍归属配置的小时（可预期），但分秒每次现掷（不可指纹化）。
// wantDay 传入期望的自然日（跨天场景由调用方算好）。
func assertJittered(t *testing.T, label string, at, wantDay time.Time, hour int) {
	t.Helper()
	if at.Day() != wantDay.Day() || at.Month() != wantDay.Month() {
		t.Errorf("%s: next=%v want day %v", label, at, wantDay.Format("2006-01-02"))
		return
	}
	if at.Hour() != hour {
		t.Errorf("%s: next=%v want hour %d（抖动不跨小时）", label, at, hour)
		return
	}
	// 核心不变量：整点 :00 是所有自动化脚本的共同指纹，必须避开。
	if at.Minute() == 0 && at.Second() == 0 {
		t.Errorf("%s: next=%v 落在整点——本次改动的唯一硬性要求就是不整点", label, at)
	}
	// 落点不得越过本小时最后一秒（夹取兜底不得反过来把任务排到下一小时）。
	if at.Minute() > 55 {
		t.Errorf("%s: next=%v 抖动越过了 %d 点窗口上界", label, at, hour)
	}
}

// testRnd 测试用抖动源（默认跨度）。nextFire 现已是 Randomizer 的方法，一次性实例即可。
//
// 注意传的是 Default 而不是 0：NewRandomizer 的 0 表示**准点**（跨度 0），
// "未配置 → 默认跨度"的兜底在 ScheduleSpec.JitterSpan() 那一侧。
func testRnd() *Randomizer { return NewRandomizer(jitterSpanDefault) }

func TestNextFire(t *testing.T) {
	loc := time.Local
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, loc)
	next := testRnd().nextFire(now, []int{9, 21})
	assertJittered(t, "10:00 起算，同日晚些的 21 点", next, now, 21)
	now = time.Date(2026, 7, 27, 22, 0, 0, 0, loc)
	next = testRnd().nextFire(now, []int{9, 21})
	assertJittered(t, "22:00 起算，次日 9 点", next, now.AddDate(0, 0, 1), 9)
	// 09:00 起算：9 点的抖动落点（恒 >09:00）仍在将来，故**当日**触发。
	// 这是与改动前最大的语义差别——旧实现 9 点整已被算作"已过"，要等到次日。
	now = time.Date(2026, 7, 27, 9, 0, 0, 0, loc)
	next = testRnd().nextFire(now, []int{9})
	assertJittered(t, "09:00 起算，9 点窗口尚在当日", next, now, 9)

	// 09:59 起算（已过抖动上界 09:55）：当日 9 点窗口确实过了，滚到次日。
	now = time.Date(2026, 7, 27, 9, 59, 0, 0, loc)
	next = testRnd().nextFire(now, []int{9})
	assertJittered(t, "09:59 起算滚到次日 9 点", next, now.AddDate(0, 0, 1), 9)
}

// TestNextFireJitterNeverOnTheHour 抖动落点永远避开整点（本次改动的硬性要求）。
func TestNextFireJitterNeverOnTheHour(t *testing.T) {
	loc := time.Local
	r := testRnd()
	for day := 1; day <= 28; day++ {
		for _, h := range []int{0, 1, 9, 12, 21, 22, 23} {
			// 从该小时的前一小时起算，保证落在本小时内。
			now := time.Date(2026, 9, day, (h+23)%24, 0, 0, 0, loc)
			at := r.nextFire(now, []int{h})
			if at.Hour() != h {
				t.Fatalf("day=%d h=%d: next=%v 不在目标小时", day, h, at)
			}
			if at.Minute() == 0 && at.Second() == 0 {
				t.Fatalf("day=%d h=%d: next=%v 落在整点", day, h, at)
			}
		}
	}
}

// TestNextFireRandomEveryTime 每次排定重新掷随机数——这是本轮改动的核心诉求
// （用户原话：「每次都是随机时间执行，不是你固定时间」）。
//
// 反过来说：同一起算时刻连掷多次必须**不恒定**。旧实现按 (kind, 日, 小时) 哈希，
// 每天同一时刻执行，只是把指纹从 :00 挪到了 :33，仍然是一条可被关联的规律。
func TestNextFireRandomEveryTime(t *testing.T) {
	loc := time.Local
	now := time.Date(2026, 9, 18, 8, 0, 0, 0, loc)
	r := testRnd()
	seen := map[time.Time]int{}
	for i := 0; i < 60; i++ {
		seen[r.nextFire(now, []int{9})]++
	}
	if len(seen) < 2 {
		t.Fatalf("60 次排定只掷出 %d 个不同落点——抖动仍是固定的", len(seen))
	}
}

// TestNextFireKindsDifferAtSameHour 同一小时上不同任务族错开：签到与旅行都配 9 点，
// 抖动必须各自独立掷，否则同刻并发仍然是最易识别的脚本形态。
func TestNextFireKindsDifferAtSameHour(t *testing.T) {
	loc := time.Local
	now := time.Date(2026, 9, 18, 8, 0, 0, 0, loc)
	r := testRnd()
	checkin := r.nextFire(now, []int{9})
	travel := r.nextFire(now, []int{9})
	if checkin.Equal(travel) {
		t.Errorf("checkin=%v travel=%v 同刻——不同任务族应各自错开", checkin, travel)
	}
}

func TestNextFireMergesSchedules(t *testing.T) {
	now := time.Date(2026, 7, 27, 20, 0, 0, 0, time.Local)
	next := testRnd().nextFire(now, []int{9, 21, 22})
	assertJittered(t, "20:00 起算，最近的 21 点", next, now, 21)
}

// TestNextWakeKeepaliveOnly 签到已过点时按保活时点唤醒。
func TestNextWakeKeepaliveOnly(t *testing.T) {
	s := New(Config{CheckinHours: []int{9}, KeepaliveHours: []int{22},
		TravelDisabled: true, ActivityDisabled: true})
	now := time.Date(2026, 9, 11, 20, 0, 0, 0, time.Local)
	at, kinds := s.nextWake(now)
	assertJittered(t, "20:00 起算，保活 22 点", at, now, 22)
	if len(kinds) != 1 || kinds[0] != taskKeepalive {
		t.Errorf("kinds=%v want [keepalive]", kinds)
	}
}

// TestNextWakeSameInstantFiresAll 同刻多任务合并语义仍需保留：抖动让相撞变得罕见，
// 但语义上"两个任务落到同一时刻就一起执行"必须成立。此处把抖动固定为同一值来构造相撞。
func TestNextWakeSameInstantFiresAll(t *testing.T) {
	// 抖动固定 10 分钟：签到 22 点与保活 22 点必然同刻。
	defer swapRoll(t, 10*time.Minute)()

	s := New(Config{
		CheckinHours:     []int{9, 22},
		TravelHours:      []int{}, // 禁用旅行时点干扰（仅测签到+保活同整点）
		ActivityHours:    []int{}, // 禁用活跃时点干扰
		KeepaliveHours:   []int{22},
		TravelDisabled:   true,
		ActivityDisabled: true,
		SchoolDisabled:   true,
		CatDisabled:      true,
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 21, 30, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 22, 10, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v", at, want)
	}
	if !hasKind(kinds, taskCheckin) || !hasKind(kinds, taskKeepalive) {
		t.Errorf("kinds=%v want checkin+keepalive（同一时刻两任务）", kinds)
	}

	// 22 点过后下一次是次日 09:10，且只含签到（旅行/活跃已禁用）。
	at, kinds = s.nextWake(time.Date(2026, 9, 11, 22, 30, 0, 0, time.Local))
	if want := time.Date(2026, 9, 12, 9, 10, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Errorf("kinds=%v want [checkin]", kinds)
	}
}

// swapRoll 把抖动偏移固定为 d 秒/分钟（固定值 + 跨度 0 让 rollLocked 的 secs<=0
// 分支返回 0，这里直接替换 rollFn 绕开跨度），返回还原函数。
// 配合 `defer swapRoll(t, d)()` 使用——只用于构造"两个任务恰好同刻"的确定性场景。
func swapRoll(t *testing.T, d time.Duration) func() {
	t.Helper()
	orig := rollFn
	rollFn = func(*rand.Rand, time.Duration) time.Duration { return d }
	return func() { rollFn = orig }
}

// TestNextWakeQuasiZeroRandomizerIsPunctual 零值（含 span 0）的 Randomizer 必须在
// NextWake 里**保持准点**，即 r==nil 与 &Randomizer{} 同口径——"零值可用"是
// NextWake 显式承诺的契约（nil 时它就地构造 &Randomizer{}）。
//
// 这条守的是一个已经踩过的坑：NewRandomizer 曾经把"span<=0"也当成"未配置 → 默认 55"，
// 于是 jitter_disabled 构造出来的实例跨度是 55，准点配置静默失效（要等第一次排定
// 经 setSpanFromSpec 才被纠正）。现在默认值只在 ScheduleSpec.JitterSpan() 一处解析。
func TestNextWakeQuasiZeroRandomizerIsPunctual(t *testing.T) {
	now := time.Date(2026, 9, 25, 8, 0, 0, 0, time.Local)
	spec := ScheduleSpec{JitterDisabled: true, CheckinHours: []int{9}, TravelDisabled: true,
		ActivityDisabled: true, KeepaliveDisabled: true, SchoolDisabled: true, CatDisabled: true}
	for i, r := range []*Randomizer{nil, {}, NewRandomizer(0)} {
		at, kinds := NextWake(now, spec, r)
		if len(kinds) != 1 || !at.Equal(time.Date(2026, 9, 25, 9, 0, 0, 0, time.Local)) {
			t.Errorf("case %d: at=%v kinds=%v want 09:00:00 准点", i, at, kinds)
		}
	}
}

// TestNewSchedulerJitterDisabledPunctual 构造期就尊重 jitter_disabled：
// New 出来的调度器跨度必须是 0，不必等第一次排定。
func TestNewSchedulerJitterDisabledPunctual(t *testing.T) {
	if got := New(Config{JitterDisabled: true}).Randomizer().SpanMinutes(); got != 0 {
		t.Errorf("SpanMinutes()=%d want 0（jitter_disabled 必须准点）", got)
	}
	// 未配置（零值）→ 默认 55。
	if got := New(Config{}).Randomizer().SpanMinutes(); got != jitterSpanDefault {
		t.Errorf("SpanMinutes()=%d want %d（未配置回落默认）", got, jitterSpanDefault)
	}
	// 超上限 → 夹到上限。
	if got := New(Config{JitterSpanMinutes: 600}).Randomizer().SpanMinutes(); got != jitterSpanMax {
		t.Errorf("SpanMinutes()=%d want %d（超上限夹取）", got, jitterSpanMax)
	}
}

// TestNextWakeRememberKeepsAllKinds 面板按任务类型读落点，因此每个**启用**的任务族
// 都必须被记下——只记最早的那一个，面板上其余任务就永远显示「待排定」。
func TestNextWakeRememberKeepsAllKinds(t *testing.T) {
	now := time.Date(2026, 9, 25, 8, 0, 0, 0, time.Local)
	spec := ScheduleSpec{
		CheckinHours: []int{9}, TravelHours: []int{9}, ActivityHours: []int{10},
		KeepaliveHours: []int{22}, SchoolHours: []int{12}, CatHours: []int{1},
	}
	r := NewRandomizer(jitterSpanDefault)
	if _, kinds := NextWake(now, spec, r); len(kinds) == 0 {
		t.Fatal("六族全启用却排不出任务")
	}
	for _, k := range AllKinds {
		at, ok := r.Scheduled(string(k))
		if !ok {
			t.Errorf("%s 未被记下落点（面板会一直显示「待排定」）", k)
			continue
		}
		hours, _ := hoursOfSpec(spec, k)
		if !containsInt(hours, at.Hour()) {
			t.Errorf("%s 落点 %v 不在所配小时 %v 内", k, at, hours)
		}
		if at.Minute() == 0 && at.Second() == 0 {
			t.Errorf("%s 落点 %v 落在整点", k, at)
		}
	}
}

// hoursOfSpec 测试用的 spec 小时查表（生产侧对应 automation.hoursOf）。
func hoursOfSpec(spec ScheduleSpec, k Kind) ([]int, bool) {
	switch k {
	case KindCheckin:
		return spec.CheckinHours, !spec.CheckinDisabled
	case KindTravel:
		return spec.TravelHours, !spec.TravelDisabled
	case KindActivity:
		return spec.ActivityHours, !spec.ActivityDisabled
	case KindKeepalive:
		return spec.KeepaliveHours, !spec.KeepaliveDisabled
	case KindSchool:
		return spec.SchoolHours, !spec.SchoolDisabled
	case KindCat:
		return spec.CatHours, !spec.CatDisabled
	}
	return nil, false
}

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// TestNextWakeNothingScheduled 两类任务全空时返回零值，Run 只等退出信号。
func TestNextWakeNothingScheduled(t *testing.T) {
	s := &Scheduler{cfg: Config{}}
	at, kinds := s.nextWake(time.Now())
	if !at.IsZero() || len(kinds) != 0 {
		t.Errorf("at=%v kinds=%v want zero/nil", at, kinds)
	}
}

// TestNextWakeCheckinDisabled 显式禁用签到后，排程里不再有签到时点（保活照常）。
func TestNextWakeCheckinDisabled(t *testing.T) {
	s := New(Config{CheckinDisabled: true, CheckinHours: []int{9, 21}, KeepaliveHours: []int{22},
		TravelDisabled: true, ActivityDisabled: true})
	now := time.Date(2026, 9, 11, 20, 0, 0, 0, time.Local)
	at, kinds := s.nextWake(now)
	assertJittered(t, "签到禁用 → 只剩保活 22 点", at, now, 22)
	if len(kinds) != 1 || kinds[0] != taskKeepalive {
		t.Errorf("kinds=%v want [keepalive]", kinds)
	}
}

// TestNextWakeKeepaliveDisabled 显式禁用保活后，排程里不再有保活时点（签到照常）。
func TestNextWakeKeepaliveDisabled(t *testing.T) {
	s := New(Config{KeepaliveDisabled: true, CheckinHours: []int{9, 21}, KeepaliveHours: []int{22},
		TravelDisabled: true, ActivityDisabled: true})
	now := time.Date(2026, 9, 11, 20, 0, 0, 0, time.Local)
	at, kinds := s.nextWake(now)
	assertJittered(t, "保活禁用 → 只剩签到 21 点", at, now, 21)
	if len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Errorf("kinds=%v want [checkin]", kinds)
	}
}

// TestNextWakeBothDisabledNothingScheduled 六类任务都显式禁用 → 无可唤醒时点。
func TestNextWakeBothDisabledNothingScheduled(t *testing.T) {
	s := New(Config{
		CheckinDisabled:   true,
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		SchoolDisabled:    true,
		CatDisabled:       true,
		CheckinHours:      []int{9, 21},
		KeepaliveHours:    []int{22},
	})
	at, kinds := s.nextWake(time.Now())
	if !at.IsZero() || len(kinds) != 0 {
		t.Errorf("at=%v kinds=%v want zero/nil", at, kinds)
	}
}

// TestRunAllDisabledNoSpinNoCalls 六类任务全禁用：Run 不空转（只等退出信号），
// 且不能触发任何上游请求。
func TestRunAllDisabledNoSpinNoCalls(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "no upstream call expected", 404)
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{
		Pool:              p,
		Upstream:          up,
		CheckinDisabled:   true,
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		SchoolDisabled:    true,
		CatDisabled:       true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	start := time.Now()
	s.Run(ctx) // 阻塞到 ctx 取消为止（无时点可等，不构造 timer）
	elapsed := time.Since(start)

	if calls.Load() != 0 {
		t.Errorf("upstream calls=%d want 0（六类全禁用）", calls.Load())
	}
	if elapsed < 200*time.Millisecond {
		t.Errorf("Run returned after %v, before ctx done（不应提前返回）", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Run took %v（不应空转/忙等）", elapsed)
	}
}

func hasKind(kinds []taskKind, k taskKind) bool {
	for _, v := range kinds {
		if v == k {
			return true
		}
	}
	return false
}

// fakeUpstream 同时模拟 billing 与 refresh。
type fakeUpstream struct {
	checkinCalls   atomic.Int32
	refreshCalls   atomic.Int32
	resourceRemain int64
}

func (f *fakeUpstream) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			f.checkinCalls.Add(1)
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":100,"CycleCapacityRemain":` +
				jsonI64(f.resourceRemain) + `,"CycleCapacityUsed":0}]}}}}`))
		case strings.HasSuffix(r.URL.Path, "/token/refresh"):
			f.refreshCalls.Add(1)
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
}

func jsonI64(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestRunCheckinReenablesCoolingAccount(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	p.Add(a)
	p.Cooldown("u1", pool.CoolHard, time.Hour, "余额不足")

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{
		Pool:           p,
		Upstream:       up,
		CheckinHours:   []int{9, 21},
		KeepaliveHours: []int{22},
	})
	s.RunCheckinNow()
	if f.checkinCalls.Load() != 1 {
		t.Errorf("checkin calls=%d", f.checkinCalls.Load())
	}
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Errorf("account should be reenabled after checkin with credits: %+v", st)
	}
	if st.Credits != 500 {
		t.Errorf("credits=%d want 500", st.Credits)
	}
}

func TestRunKeepaliveRefreshesTokens(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1}
	p.Add(a)

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	s.RunKeepaliveNow()
	if f.refreshCalls.Load() != 1 {
		t.Errorf("refresh calls=%d", f.refreshCalls.Load())
	}
	if a.AccessToken != "new" {
		t.Errorf("token not updated: %s", a.AccessToken)
	}
}

func TestRunKeepaliveSessionDeadDisables(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
	}))
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1}
	p.Add(a)

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	// P0-1：12153 连续 N 次才禁用。前 2 次刷新失败不应杀号（误判防护）。
	s.RunKeepaliveNow()
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("第 1 次 12153 不应禁用: %+v", st)
	}
	s.RunKeepaliveNow()
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("第 2 次 12153 不应禁用: %+v", st)
	}
	// 第 3 次连续 12153 → 禁用。
	s.RunKeepaliveNow()
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("第 3 次连续 12153 应禁用: %+v", st)
	}
	if st.DisabledReason != "12153 session dead" {
		t.Errorf("disabled_reason=%q want 12153 session dead", st.DisabledReason)
	}
}

// TestRunKeepaliveSessionDeadResetBySuccess 两次 12153 后刷新成功 → 计数清零，
// 再来的 12153 从第 1 次重新计（不会因历史失败被继续追杀）。
func TestRunKeepaliveSessionDeadResetBySuccess(t *testing.T) {
	var fails atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fails.Add(1) == 3 { // 第 3 次（本次调度循环的第二轮）刷新成功
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
			return
		}
		w.WriteHeader(401)
		w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
	}))
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1}
	p.Add(a)

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	s.RunKeepaliveNow() // 12153 #1
	s.RunKeepaliveNow() // 12153 #2
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("precondition: 前 2 次不应禁用: %+v", st)
	}
	s.RunKeepaliveNow() // 刷新成功 → 清计数
	// 接下来连续 2 次 12153：从新计数重新算，仍不应禁用（历史计数已清）。
	s.RunKeepaliveNow() // 12153 #1（新计数）
	s.RunKeepaliveNow() // 12153 #2（新计数）
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("刷新成功清计数后连续 2 次 12153 不应禁用: %+v", st)
	}
	s.RunKeepaliveNow() // 12153 #3（新计数）→ 禁用
	if st, _ := p.Status("u1"); !st.Disabled {
		t.Fatalf("新计数第 3 次 12153 应禁用: %+v", st)
	}
}

func TestCheckinErrorDoesNotCrash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`boom`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	// 不应 panic
	s.RunCheckinNow()
	s.RunKeepaliveNow()
	_ = errors.New("unused")
}

// checkinStub 配置化的签到上游：可控制签到响应（ok/already/fail）、刷新是否失败、
// 余额返回值。各分支命中后 atomic 计数，便于并发安全断言。
type checkinStub struct {
	checkinBody    string // /daily-checkin 返回的完整 body（含 code/msg）
	checkinStatus  int    // /daily-checkin HTTP 状态码（0=200）
	refreshFail    bool   // /token/refresh 是否失败（返回 12153 session dead）
	refreshCalls   atomic.Int32
	resourceRemain int64
}

func (s *checkinStub) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			if s.checkinStatus != 0 {
				w.WriteHeader(s.checkinStatus)
			}
			w.Write([]byte(s.checkinBody))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":100,"CycleCapacityRemain":` +
				jsonI64(s.resourceRemain) + `,"CycleCapacityUsed":0}]}}}}`))
		case strings.HasSuffix(r.URL.Path, "/token/refresh"):
			s.refreshCalls.Add(1)
			if s.refreshFail {
				w.WriteHeader(401)
				w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
				return
			}
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
}

// newCheckinS 构造 Pool+Upstream+Scheduler，账号 token 未过期（不触发预刷新）。
func newCheckinS(t *testing.T, stub *checkinStub) (*Scheduler, *pool.Pool) {
	t.Helper()
	srv := stub.server()
	t.Cleanup(srv.Close)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return New(Config{Pool: p, Upstream: up}), p
}

// TestCheckinAllOK 签到成功 → ok、余额回填、credits 指针有值。
func TestCheckinAllOK(t *testing.T) {
	s, p := newCheckinS(t, &checkinStub{
		checkinBody:    `{"code":0,"msg":"ok","data":{}}`,
		resourceRemain: 500,
	})
	out, err := s.CheckinAll()
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(out) != 1 || out[0].Status != CheckinOK {
		t.Fatalf("out=%+v want ok", out)
	}
	if out[0].Credits == nil || *out[0].Credits != 500 {
		t.Errorf("credits=%v want 500", out[0].Credits)
	}
	if st, _ := p.Status("u1"); st.Credits != 500 {
		t.Errorf("pool credits=%d want 500", st.Credits)
	}
}

// TestCheckinAllAlready "今天已签到"记 already、不回填 400 报文到 detail。
func TestCheckinAllAlready(t *testing.T) {
	s, _ := newCheckinS(t, &checkinStub{
		checkinBody:    `{"code":14001,"msg":"今天已签到"}`,
		resourceRemain: 300,
	})
	out, err := s.CheckinAll()
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if out[0].Status != CheckinAlready {
		t.Errorf("status=%q want already", out[0].Status)
	}
	if !strings.Contains(out[0].Detail, "今日已签到") {
		t.Errorf("已签到应回填一句人话结论（不含上游 400 报文），got=%q", out[0].Detail)
	}
	if strings.Contains(out[0].Detail, "{") {
		t.Errorf("detail 不应回填上游报文原文，got=%q", out[0].Detail)
	}
}

// TestCheckinAllFail 签到上游 500 → fail、detail 填报错。
func TestCheckinAllFail(t *testing.T) {
	s, _ := newCheckinS(t, &checkinStub{
		checkinBody:    `boom`,
		checkinStatus:  500,
		resourceRemain: 300,
	})
	out, _ := s.CheckinAll()
	if out[0].Status != CheckinFail {
		t.Errorf("status=%q want fail", out[0].Status)
	}
	if out[0].Detail == "" {
		t.Error("fail 应填 detail")
	}
}

// TestCheckinAllSkipsDisabled 禁用账号记 skipped，不参与签到。
func TestCheckinAllSkipsDisabled(t *testing.T) {
	stub := &checkinStub{checkinBody: `{"code":0,"msg":"ok","data":{}}`, resourceRemain: 100}
	srv := stub.server()
	defer srv.Close()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "dis", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "ok", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Disable("dis", "test")
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	out, _ := s.CheckinAll()
	m := map[string]CheckinStatus{}
	for _, o := range out {
		m[o.UID] = o.Status
	}
	if m["dis"] != CheckinSkipped {
		t.Errorf("dis=%q want skipped", m["dis"])
	}
	if m["ok"] != CheckinOK {
		t.Errorf("ok=%q want ok", m["ok"])
	}
}

// TestCheckinAllSkipsNoCredentials 无 refreshToken 的账号记 skipped(no credentials)。
func TestCheckinAllSkipsNoCredentials(t *testing.T) {
	stub := &checkinStub{checkinBody: `{"code":0,"msg":"ok","data":{}}`, resourceRemain: 100}
	srv := stub.server()
	defer srv.Close()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "notoken", AccessToken: "", RefreshToken: "", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	out, _ := s.CheckinAll()
	if out[0].Status != CheckinSkipped {
		t.Errorf("status=%q want skipped", out[0].Status)
	}
	if !strings.Contains(out[0].Detail, "无有效凭证") {
		t.Errorf("detail=%q want 含「无有效凭证」的说明（面板直接展示这句）", out[0].Detail)
	}
}

// TestCheckinAllRefreshBeforeExpiry token 临近过期 → 签到前先刷新，刷新成功后继续签到。
func TestCheckinAllRefreshBeforeExpiry(t *testing.T) {
	stub := &checkinStub{checkinBody: `{"code":0,"msg":"ok","data":{}}`, resourceRemain: 100}
	srv := stub.server()
	defer srv.Close()
	p := pool.New("")
	// ExpiresAt 5 分钟后过期，落在 checkinRefreshSkew(10min) 窗口内 → 触发预刷新。
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt",
		ExpiresAt: time.Now().Add(5 * time.Minute).Unix()}
	p.Add(a)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	out, _ := s.CheckinAll()
	if stub.refreshCalls.Load() != 1 {
		t.Errorf("refresh calls=%d want 1（过期窗口应预刷新）", stub.refreshCalls.Load())
	}
	if out[0].Status != CheckinOK {
		t.Errorf("status=%q want ok（刷新成功后继续签到）", out[0].Status)
	}
	if a.AccessToken != "new" {
		t.Errorf("token 未刷新: %s", a.AccessToken)
	}
}

// TestCheckinAllRefreshFlakyContinues 刷新抖动失败但 token 未真过期 → 继续签到（不阻断）。
func TestCheckinAllRefreshFlakyContinues(t *testing.T) {
	stub := &checkinStub{
		checkinBody:    `{"code":0,"msg":"ok","data":{}}`,
		refreshFail:    true,
		resourceRemain: 100,
	}
	srv := stub.server()
	defer srv.Close()
	p := pool.New("")
	// token 5 分钟后过期（在窗口内 → 尝试刷新），但 NeedsRefresh(0) 仍 false（未真过期）。
	// 刷新返回 12153 但不是真 session dead 的终态——token 仍有效，继续签到。
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: time.Now().Add(5 * time.Minute).Unix()}
	p.Add(a)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	out, _ := s.CheckinAll()
	if out[0].Status != CheckinOK {
		t.Errorf("status=%q want ok（刷新抖动不应阻断签到）", out[0].Status)
	}
	if stub.refreshCalls.Load() != 1 {
		t.Errorf("refresh calls=%d want 1", stub.refreshCalls.Load())
	}
}

// TestCheckinAllRefreshTrulyExpiredFails 刷新失败且 token 真过期 → 记 fail。
func TestCheckinAllRefreshTrulyExpiredFails(t *testing.T) {
	stub := &checkinStub{
		checkinBody: `{"code":0,"msg":"ok","data":{}}`,
		refreshFail: true,
	}
	srv := stub.server()
	defer srv.Close()
	p := pool.New("")
	// ExpiresAt 已是过去 → NeedsRefresh(0) 为 true（真过期），刷新失败即 fail。
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}
	p.Add(a)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	out, _ := s.CheckinAll()
	if out[0].Status != CheckinFail {
		t.Errorf("status=%q want fail（真过期 + 刷新失败）", out[0].Status)
	}
	if out[0].Detail == "" || !strings.HasPrefix(out[0].Detail, "refresh:") {
		t.Errorf("detail=%q want refresh: 前缀", out[0].Detail)
	}
}

// TestCheckinAllBusy 并发第二次调用返回 ErrBusy（TryLock 串行化）。
func TestCheckinAllBusy(t *testing.T) {
	stub := &checkinStub{checkinBody: `{"code":0,"msg":"ok","data":{}}`, resourceRemain: 100}
	s, _ := newCheckinS(t, stub)
	// 手动持锁模拟一次签到正在执行，再调 CheckinAll 应得 ErrBusy。
	s.checkinMu.Lock()
	defer s.checkinMu.Unlock()
	_, err := s.CheckinAll()
	if !errors.Is(err, ErrBusy) {
		t.Errorf("err=%v want ErrBusy", err)
	}
}

// TestCheckinAllReenablesCoolingAccount 冷却账号签到成功 + 余额恢复 → 解冻。
func TestCheckinAllReenablesCoolingAccount(t *testing.T) {
	stub := &checkinStub{checkinBody: `{"code":0,"msg":"ok","data":{}}`, resourceRemain: 500}
	srv := stub.server()
	defer srv.Close()
	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	p.Add(a)
	p.Cooldown("u1", pool.CoolHard, time.Hour, "余额不足")
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	out, err := s.CheckinAll()
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if out[0].Status != CheckinOK {
		t.Errorf("status=%q want ok", out[0].Status)
	}
	if st, _ := p.Status("u1"); st.Cooling {
		t.Errorf("签到 + 余额恢复应解冻: %+v", st)
	}
}

// TestRunKeepaliveBackfillsRealm 验证 keepalive refresh 成功后，SaveAtomic 落盘文件
// 自动补上 realm 标识：老 global 文件（domain=workbuddy.ai，无 realm 键）→ global，
// 老 CN 文件（空 domain，无 realm 键）→ cn；再次 refresh 不改变已补的标识（幂等）。
func TestRunKeepaliveBackfillsRealm(t *testing.T) {
	cases := []struct {
		name      string
		fixture   string
		filename  string
		wantRealm string
	}{
		{
			name:      "老 global 落盘补 global",
			fixture:   `{"auth":{"accessToken":"old","refreshToken":"rt","expiresAt":1,"domain":"www.workbuddy.ai"},"account":{"uid":"g1"}}`,
			filename:  "workbuddy-g1.json",
			wantRealm: "global",
		},
		{
			name:      "老 CN 空 domain 落盘补 cn",
			fixture:   `{"auth":{"accessToken":"old","refreshToken":"rt","expiresAt":1,"domain":""},"account":{"uid":"c1"}}`,
			filename:  "workbuddy-c1.json",
			wantRealm: "cn",
		},
		{
			name:      "已有 realm 不被覆盖——global domain 显式 cn 保持 cn",
			fixture:   `{"auth":{"accessToken":"old","refreshToken":"rt","expiresAt":1,"domain":"www.workbuddy.ai","realm":"cn"},"account":{"uid":"c2"}}`,
			filename:  "workbuddy-c2.json",
			wantRealm: "cn",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			fp := filepath.Join(dir, c.filename)
			if err := os.WriteFile(fp, []byte(c.fixture), 0o600); err != nil {
				t.Fatal(err)
			}

			a, err := auth.Parse([]byte(c.fixture))
			if err != nil {
				t.Fatal(err)
			}
			a.FilePath = fp

			p := pool.New("")
			p.Add(a)

			f := &fakeUpstream{}
			srv := f.server()
			defer srv.Close()
			up := &upstream.Client{
				HTTP:          srv.Client(),
				ChatBaseCN:    srv.URL,
				BillingBaseCN: srv.URL,
			}
			s := New(Config{Pool: p, Upstream: up})

			s.RunKeepaliveNow()
			if f.refreshCalls.Load() != 1 {
				t.Fatalf("refresh calls=%d", f.refreshCalls.Load())
			}
			raw, err := os.ReadFile(fp)
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			b, err := auth.Parse(raw)
			if err != nil {
				t.Fatalf("reparse: %v", err)
			}
			if b.RealmStored() != c.wantRealm {
				t.Errorf("realm=%q want %q after refresh+save", b.RealmStored(), c.wantRealm)
			}

			// 幂等：已有标识后再跑一轮 refresh+save，值不变
			s.RunKeepaliveNow()
			raw2, err := os.ReadFile(fp)
			if err != nil {
				t.Fatalf("read after second run: %v", err)
			}
			b2, err := auth.Parse(raw2)
			if err != nil {
				t.Fatalf("reparse after second: %v", err)
			}
			if b2.RealmStored() != c.wantRealm {
				t.Errorf("realm=%q want %q after idempotent run", b2.RealmStored(), c.wantRealm)
			}
		})
	}
}
