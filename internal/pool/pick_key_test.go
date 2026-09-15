// PickForKey 分层选号测试：scope 过滤、优先级分层与降级、兜底限定关联子集。
package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// mkScope 便捷构造。
func mkScope(m map[string]int) *KeyScope { return &KeyScope{Allowed: m} }

func TestPickForKeyExcludesUnassociated(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 100)
	scope := mkScope(map[string]int{"u1": 10})
	for i := 0; i < 50; i++ {
		got := p.PickForKey(nil, "", "", scope)
		if got == nil || got.UID != "u1" {
			t.Fatalf("pick=%v want u1 only (scope excludes u2)", got)
		}
	}
}

func TestPickForKeyTierPriority(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "high"})
	p.Add(&auth.Auth{UID: "low"})
	p.SetCredits("high", 100)
	p.SetCredits("low", 100)
	scope := mkScope(map[string]int{"high": 100, "low": 1})
	// 两层都健康：只用高层。
	for i := 0; i < 50; i++ {
		if got := p.PickForKey(nil, "", "", scope); got.UID != "high" {
			t.Fatalf("pick=%v want high (higher tier)", got)
		}
	}
}

func TestPickForKeyFallsToLowerTierWhenTopCooling(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "high"})
	p.Add(&auth.Auth{UID: "low"})
	p.SetCredits("high", 100)
	p.SetCredits("low", 100)
	p.Cooldown("high", CoolSoft, time.Hour, "429")
	scope := mkScope(map[string]int{"high": 100, "low": 1})
	// 高层冷却：降级到低层（不返回 nil、不回高层）。
	if got := p.PickForKey(nil, "", "", scope); got == nil || got.UID != "low" {
		t.Fatalf("pick=%v want low (tier fallback)", got)
	}
}

func TestPickForKeyScopedCooldownFallback(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	// u1：scope 内、软冷却；u2：scope 外、同样冷却。
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.Cooldown("u1", CoolSoft, time.Minute, "429")
	p.Cooldown("u2", CoolSoft, 30*time.Second, "429") // 更早到期但不在 scope
	scope := mkScope(map[string]int{"u1": 10})
	// 兜底必须限定 scope 内：u2 到期更早也不可选。
	if got := p.PickForKey(nil, "", "", scope); got == nil || got.UID != "u1" {
		t.Fatalf("pick=%v want u1 (scoped fallback)", got)
	}
	// 全 scope 外冷却 → nil。
	if got := p.PickForKey(nil, "", "", mkScope(map[string]int{"u3": 1})); got != nil {
		t.Fatalf("pick=%v want nil (no scoped candidate)", got)
	}
}

func TestPickForKeyDisabledExcluded(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.Disable("u1", "manual")
	scope := mkScope(map[string]int{"u1": 100, "u2": 1})
	// u1 禁用：正常选号走 u2；禁用号不参与兜底。
	if got := p.PickForKey(nil, "", "", scope); got == nil || got.UID != "u2" {
		t.Fatalf("pick=%v want u2 (disabled excluded)", got)
	}
}

func TestPickForKeyNilScopeEqualsPick(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 100)
	// nil scope 与空 scope 都退化为普通选号（全池可达）。
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		seen[p.PickForKey(nil, "", "", nil).UID] = true
		seen[p.PickForKey(nil, "", "", mkScope(map[string]int{})).UID] = true
	}
	if !seen["u1"] || !seen["u2"] {
		t.Fatalf("empty/nil scope must reach full pool: %v", seen)
	}
}

func TestPickForKeyTieringIgnoresLowerTierEvenIfHigherCredits(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "rich-low"})
	p.Add(&auth.Auth{UID: "poor-high"})
	p.SetCredits("rich-low", 50000)
	p.SetCredits("poor-high", 10)
	scope := mkScope(map[string]int{"rich-low": 1, "poor-high": 50})
	// 优先级压过 credits 权重：低积分但高层级恒被选。
	for i := 0; i < 50; i++ {
		if got := p.PickForKey(nil, "", "", scope); got.UID != "poor-high" {
			t.Fatalf("pick=%v want poor-high (priority beats credits)", got)
		}
	}
}

// TestStickyScopedYieldsToHigherTier 粘性绑定不得压过更高优先级层：
// 绑定号 prio 低、且高层有可用号 → 返回 nil（handler 解绑后按层重选）。
func TestStickyScopedYieldsToHigherTier(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "low"})
	p.Add(&auth.Auth{UID: "high"})
	scope := mkScope(map[string]int{"high": 10, "low": 1})
	// 高层可用 → 低层粘性命中应失效。
	if got := p.PickByUIDForModelScoped("low", "", "", scope); got != nil {
		t.Fatalf("sticky low with healthy high tier: want nil, got %v", got)
	}
	// 高层冷却 → 低层绑定继续有效（高层不可用时不违反优先级）。
	p.Cooldown("high", CoolSoft, time.Hour, "429")
	if got := p.PickByUIDForModelScoped("low", "", "", scope); got == nil || got.UID != "low" {
		t.Fatalf("sticky low with cooling high tier: want low, got %v", got)
	}
	// 绑定号即高层 → 正常命中。
	if got := p.PickByUIDForModelScoped("high", "", "", scope); got != nil {
		t.Fatalf("cooling high bound must be nil (unhealthy), got %v", got)
	}
}

// TestStickyScopedTopTierHits 绑定号本身在最高可用层 → 照常命中（保留粘性收益）。
func TestStickyScopedTopTierHits(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	scope := mkScope(map[string]int{"a": 10, "b": 10})
	if got := p.PickByUIDForModelScoped("a", "", "", scope); got == nil || got.UID != "a" {
		t.Fatalf("same-tier sticky should hit, got %v", got)
	}
}

// TestStickyScopedRejectsOutsideScope 绑定号不在 Key 关联集内 → nil（跨 Key 不泄漏）。
func TestStickyScopedRejectsOutsideScope(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "mine"})
	p.Add(&auth.Auth{UID: "other"})
	scope := mkScope(map[string]int{"mine": 1})
	if got := p.PickByUIDForModelScoped("other", "", "", scope); got != nil {
		t.Fatalf("sticky outside scope: want nil, got %v", got)
	}
}

// TestStickyScopedNilScopeDegrades 通配/旧模式（scope nil 或空）→ 退化为普通粘性。
func TestStickyScopedNilScopeDegrades(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	if got := p.PickByUIDForModelScoped("u1", "", "", nil); got == nil || got.UID != "u1" {
		t.Fatalf("nil scope sticky: want u1, got %v", got)
	}
	if got := p.PickByUIDForModelScoped("u1", "", "", mkScope(map[string]int{})); got == nil || got.UID != "u1" {
		t.Fatalf("empty scope sticky: want u1, got %v", got)
	}
}

// TestScopedPickAlwaysUsesTopTierAcrossRepeats 多层多账号：连续选号恒在最高可用层。
func TestScopedPickAlwaysUsesTopTierAcrossRepeats(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	for _, uid := range []string{"h1", "h2", "m1", "l1"} {
		p.Add(&auth.Auth{UID: uid})
		p.SetCredits(uid, 100)
	}
	// 低层积分远高，验证优先级压过 credits 权重。
	p.SetCredits("l1", 100000)
	scope := mkScope(map[string]int{"h1": 10, "h2": 10, "m1": 5, "l1": 1})
	seen := map[string]int{}
	for i := 0; i < 100; i++ {
		got := p.PickForKey(nil, "", "", scope)
		if got == nil {
			t.Fatalf("nil pick at %d", i)
		}
		seen[got.UID]++
	}
	if seen["m1"] != 0 || seen["l1"] != 0 {
		t.Fatalf("lower tiers must starve while top tier healthy: %v", seen)
	}
	if seen["h1"] == 0 || seen["h2"] == 0 {
		t.Fatalf("top tier should be spread across its accounts: %v", seen)
	}
}
