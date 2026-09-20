// 通知事件注入点测试：状态转换触发回调、事件类型正确、模型级冷却不触发。
package pool

import (
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// collectNotices 注入通知回调并返回事件收集器（回调在持锁路径上，只做追加）。
func collectNotices(p *Pool) (*[]NoticeEvent, *sync.Mutex) {
	var mu sync.Mutex
	var got []NoticeEvent
	p.SetNotifier(func(ev NoticeEvent) {
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	})
	return &got, &mu
}

func kinds(got *[]NoticeEvent, mu *sync.Mutex) []string {
	mu.Lock()
	defer mu.Unlock()
	out := make([]string, 0, len(*got))
	for _, e := range *got {
		out = append(out, e.Kind)
	}
	return out
}

func TestNoticeOnCoolingAndExhausted(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "甲", Domain: "www.codebuddy.cn"})
	p.SetCreditsDetailed("u1", 500, 120)
	got, mu := collectNotices(p)

	p.Cooldown("u1", CoolSoft, time.Hour, "429 rate limit")
	if k := kinds(got, mu); len(k) != 1 || k[0] != "cooling" {
		t.Fatalf("soft cooldown: want [cooling], got %v", k)
	}
	mu.Lock()
	ev := (*got)[0]
	mu.Unlock()
	if ev.UID != "u1" || ev.Nickname != "甲" || ev.Realm != "cn" || ev.Credits != 500 || ev.Expiring != 120 {
		t.Fatalf("event fields wrong: %+v", ev)
	}
	if ev.Until.IsZero() {
		t.Fatal("cooling event should carry Until")
	}

	p.CooldownUntilTomorrow4AM("u1", "余额不足")
	if k := kinds(got, mu); len(k) != 2 || k[1] != "exhausted" {
		t.Fatalf("hard credit: want cooling+exhausted, got %v", k)
	}
}

func TestNoticeOnSoftRateAndBreakerAndDisable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", Domain: "www.codebuddy.cn"})
	p.SetBreaker(2, 30*time.Minute, 2*time.Hour)
	got, mu := collectNotices(p)

	p.CooldownSoftRate("u1", time.Minute, time.Time{}, "429 rate limit")
	p.NoteError("u1")
	p.NoteError("u1") // 达阈值 → 熔断
	p.Disable("u1", "manual")
	gotKinds := kinds(got, mu)
	want := []string{"cooling", "breaker", "disabled"}
	if len(gotKinds) != len(want) {
		t.Fatalf("want %v, got %v", want, gotKinds)
	}
	for i := range want {
		if gotKinds[i] != want[i] {
			t.Fatalf("want %v, got %v", want, gotKinds)
		}
	}
}

func TestNoticeNotFiredForModelLevelCooldown(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", Domain: "www.codebuddy.cn"})
	got, mu := collectNotices(p)

	// 带上游重置时间的模型级 6004 冷却：账号仍在服务（仅该模型被限），不应发通知。
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(30*time.Minute), "glm-5.3", "6004 model rate limit")
	if k := kinds(got, mu); len(k) != 0 {
		t.Fatalf("model-level cooldown must not notify, got %v", k)
	}
}

func TestAvailabilityInjection(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", Domain: "www.codebuddy.cn"})
	if got := p.Availability("cn"); got != nil {
		t.Fatalf("unset availability should be nil, got %v", got)
	}
	p.SetAvailability(func(realm string) []string { return []string{"u1@" + realm} })
	if got := p.Availability("cn"); len(got) != 1 || got[0] != "u1@cn" {
		t.Fatalf("availability wiring wrong: %v", got)
	}
}

// 在途分摊权重（防风控）：权重按 1/(1+在途数) 阻尼；开关关闭时原样返回。
func TestSpreadInFlightDamping(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a", Domain: "www.codebuddy.cn"})
	ea := p.byUID["a"]

	// 开启（缺省）：在途 0 不衰减；在途 2 → 衰减到 1/3
	base := 100.0
	if got := p.dampWeight(ea, base); got != base {
		t.Fatalf("in-flight 0 should not damp, got %v", got)
	}
	ea.inFlight.Store(2)
	if got := p.dampWeight(ea, base); got > base/3+0.01 {
		t.Fatalf("in-flight 2 should damp to <=1/3, got %v", got)
	}
	// 关闭：原样返回
	p.SetSpreadInFlight(false)
	if got := p.dampWeight(ea, base); got != base {
		t.Fatalf("spread off should not damp, got %v", got)
	}
	p.SetSpreadInFlight(true)
	ea.inFlight.Store(0)
	if got := p.dampWeight(ea, base); got != base {
		t.Fatalf("in-flight 0 should not damp, got %v", got)
	}
}
