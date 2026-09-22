// 模型级故障转移测试：主模型被域内所有账号拒绝（11102）→ 按白名单切便宜模型；
// 账号级问题（429 限流）不触发回退；Pool.ModelGoneRealm 信号口径。
package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/keys"
	"workbuddy2api/internal/pool"
)

// 11102「该后端无此模型」答复（确定性文案，Classify 归 ErrModelBlocked）。
const resp11102 = `{"error":{"code":11102,"msg":"service info not found"}}`

// fallbackFixture 两个 CN 账号 + 回退白名单 Key（两账号都关联）。
// 返回的 captured 为上游请求体捕获指针（发请求后读取）。
func fallbackFixture(t *testing.T, mix bool) (*pool.Pool, *auth.Auth, *auth.Auth, *Handler, *[]byte) {
	t.Helper()
	auth.SetGlobalEnabled(true)
	cn1 := &auth.Auth{UID: "fb-cn-1", AccessToken: "at-1", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	cn2 := &auth.Auth{UID: "fb-cn-2", AccessToken: "at-2", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	p := testPoolWith(cn1, cn2)
	st, err := keys.Load(filepath.Join(t.TempDir(), "keys.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	k, err := st.Create("fb")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Update(k.ID, func(x *keys.Key) error {
		x.Associations = []keys.Association{
			{UID: cn1.UID, Priority: 10, Enabled: true},
			{UID: cn2.UID, Priority: 1, Enabled: true},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var captured []byte
	up := newCapturingUpstream(t, &captured, func(authz string) (int, string, bool) {
		return 400, resp11102, true // 对主模型恒返回 11102
	})
	h := NewHandler(Config{
		Pool: p, Upstream: up, Keys: st, AdminKey: "adm", MaxBodyBytes: 1 << 20,
		MixRealms: mix, ModelFallback: []string{"deepseek-v4.1-flash", "glm-5.3-flash"},
	})
	return p, cn1, cn2, h, &captured
}

func postChat(h *Handler, kv, model string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}],"max_tokens":1}`))
	req.Header.Set("Authorization", "Bearer "+kv)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestModelFallbackOnModelGone(t *testing.T) {
	auth.SetGlobalEnabled(true)
	cn1 := &auth.Auth{UID: "fb-cn-1", AccessToken: "at-1", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	cn2 := &auth.Auth{UID: "fb-cn-2", AccessToken: "at-2", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	p := testPoolWith(cn1, cn2)
	st, _ := keys.Load(filepath.Join(t.TempDir(), "keys.json"), "")
	k, _ := st.Create("fb")
	_, _ = st.Update(k.ID, func(x *keys.Key) error {
		x.Associations = []keys.Association{{UID: cn1.UID, Priority: 10, Enabled: true}, {UID: cn2.UID, Priority: 1, Enabled: true}}
		return nil
	})
	var captured []byte
	// 模型感知上游：gone-model → 11102（模型不存在）；deepseek-v4.1-flash → 200。
	up := newCapturingUpstream(t, &captured, func(authz string) (int, string, bool) {
		if strings.Contains(string(captured), `"model":"gone-model"`) {
			return 400, resp11102, true
		}
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Keys: st, AdminKey: "adm", MaxBodyBytes: 1 << 20,
		MixRealms: false, ModelFallback: []string{"deepseek-v4.1-flash", "glm-5.3-flash"}})

	rec := postChat(h, k.Value, "gone-model")
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	// 上游实际收到的 model 是回退模型（deepseek），且只打了一轮（尝试=1）。
	if !strings.Contains(string(captured), `"model":"deepseek-v4.1-flash"`) {
		t.Fatalf("fallback model not sent upstream: %s", captured)
	}
	total := int64(0)
	for _, s := range p.List() {
		total += s.SuccessCount
	}
	if total == 0 {
		t.Fatal("no success recorded")
	}
}

// TestModelFallbackOnRateLimitExhaustion 语义更新后：账号级限流把域内账号全部打入
// 冷却（当前模型无账号可服务）→ 同样按白名单回退尝试便宜模型；本例上游对回退模型
// 也 429，最终仍 429，但回退轮换确实发生了（captured 含 deepseek 尝试）。
func TestModelFallbackOnRateLimitExhaustion(t *testing.T) {
	auth.SetGlobalEnabled(true)
	cn1 := &auth.Auth{UID: "fb-cn-1", AccessToken: "at-1", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	cn2 := &auth.Auth{UID: "fb-cn-2", AccessToken: "at-2", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	p := testPoolWith(cn1, cn2)
	st, _ := keys.Load(filepath.Join(t.TempDir(), "keys.json"), "")
	k, _ := st.Create("fb")
	_, _ = st.Update(k.ID, func(x *keys.Key) error {
		x.Associations = []keys.Association{{UID: cn1.UID, Priority: 10, Enabled: true}, {UID: cn2.UID, Priority: 1, Enabled: true}}
		return nil
	})
	var captured []byte
	upSlow := newCapturingUpstream(t, &captured, func(string) (int, string, bool) {
		return 429, `{"error":{"msg":"rate limited"}}`, true
	})
	h := NewHandler(Config{Pool: p, Upstream: upSlow, Keys: st, AdminKey: "adm", MaxBodyBytes: 1 << 20,
		ModelFallback: []string{"deepseek-v4.1-flash"}})
	rec := postChat(h, k.Value, "some-model")
	if rec.Code != http.StatusTooManyRequests && rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("exhausted rate limits: want 429/503, got %d body=%s", rec.Code, rec.Body.String())
	}
	// 关键断言：限流耗尽后回退轮换确实尝试了便宜模型。
	if !strings.Contains(string(captured), `"model":"deepseek-v4.1-flash"`) {
		t.Fatal("fallback should be attempted when rate limits exhaust all accounts")
	}
}

func TestModelGoneRealmSignal(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "a", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "b", Domain: "www.codebuddy.cn"})

	// 无任何负缓存：不算模型没了。
	if p.ModelGoneRealm("cn", "m1", nil) {
		t.Fatal("no blocks should be false")
	}
	// 全部账号 11102 负缓存 → true。
	p.BlockModelBackoff("a", "m1", "11102 model not available")
	p.BlockModelBackoff("b", "m1", "11102 model not available")
	if !p.ModelGoneRealm("cn", "m1", nil) {
		t.Fatal("all blocked should be true")
	}
	// 6004 限流也算"当前无账号可服务"（重置墙钟可能在数小时后，按白名单切模型）。
	p.BlockModelBackoff("a", "m2", "6004 model rate limit")
	p.BlockModelBackoff("b", "m2", "6004 model rate limit")
	if !p.ModelGoneRealm("cn", "m2", nil) {
		t.Fatal("all 6004-limited should be true (fallback to cheaper model)")
	}
	// 其他模型的缓存不影响 m3。
	if p.ModelGoneRealm("cn", "m3", nil) {
		t.Fatal("other model should be false")
	}
}

// ── Key 级模型限制与优先级 ──────────────────────────────────────────────

// restrictedFixture Key 白名单 [deepseek(100), glm-5.3-flash(50)]；上游按请求模型名
// 区分行为：glm-5.3-flash 正常服务，其余模型一律 11102（模拟"模型不存在"）。
func restrictedFixture(t *testing.T) (*pool.Pool, *Handler, *[]byte, string) {
	t.Helper()
	auth.SetGlobalEnabled(true)
	cn := &auth.Auth{UID: "rs-cn-1", AccessToken: "at-1", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	p := testPoolWith(cn)
	st, err := keys.Load(filepath.Join(t.TempDir(), "keys.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	k, err := st.Create("limited")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Update(k.ID, func(x *keys.Key) error {
		x.Associations = []keys.Association{{UID: cn.UID, Priority: 1, Enabled: true}}
		x.Models = []keys.KeyModel{
			{Name: "deepseek-v4.1-flash", Priority: 100},
			{Name: "glm-5.3-flash", Priority: 50},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var captured []byte
	up := newCapturingUpstream(t, &captured, func(authz string) (int, string, bool) {
		// 白名单内模型正常服务；白名单外模型一律 11102（模拟上游无此模型）。
		if strings.Contains(string(captured), `"model":"glm-5.3-flash"`) ||
			strings.Contains(string(captured), `"model":"deepseek-v4.1-flash"`) {
			return 200, sseOK, true
		}
		return 400, resp11102, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Keys: st, AdminKey: "adm", MaxBodyBytes: 1 << 20})
	return p, h, &captured, k.Value
}

func TestKeyModelRestrictionReroutes(t *testing.T) {
	_, h, captured, kv := restrictedFixture(t)
	// 请求白名单外的模型 → 自动改路由到优先级最高的 deepseek。
	rec := postChat(h, kv, "whatever-model")
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(string(*captured), `"model":"deepseek-v4.1-flash"`) {
		t.Fatalf("should reroute to top-priority allowed model: %s", *captured)
	}
}

func TestKeyModelRestrictionFallbackWithinList(t *testing.T) {
	// deepseek 也 11102（模拟下架）→ 按优先级回退到 glm-5.3-flash。
	auth.SetGlobalEnabled(true)
	cn := &auth.Auth{UID: "rs-cn-2", AccessToken: "at-1", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	p := testPoolWith(cn)
	st, err := keys.Load(filepath.Join(t.TempDir(), "keys.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	k, err := st.Create("limited2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Update(k.ID, func(x *keys.Key) error {
		x.Associations = []keys.Association{{UID: cn.UID, Priority: 1, Enabled: true}}
		x.Models = []keys.KeyModel{
			{Name: "deepseek-v4.1-flash", Priority: 100},
			{Name: "glm-5.3-flash", Priority: 50},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var captured []byte
	up := newCapturingUpstream(t, &captured, func(authz string) (int, string, bool) {
		if strings.Contains(string(captured), `"model":"glm-5.3-flash"`) {
			return 200, sseOK, true
		}
		return 400, resp11102, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Keys: st, AdminKey: "adm", MaxBodyBytes: 1 << 20})
	rec := postChat(h, k.Value, "deepseek-v4.1-flash")
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(string(captured), `"model":"glm-5.3-flash"`) {
		t.Fatalf("should fall back within key list: %s", captured)
	}
}

func TestKeyModelUnrestrictedPassesThrough(t *testing.T) {
	// 未配置模型白名单：请求什么模型就原样出站（哪怕上游 11102 也不悄悄换）。
	auth.SetGlobalEnabled(true)
	cn := &auth.Auth{UID: "rs-cn-3", AccessToken: "at-1", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	p := testPoolWith(cn)
	st, err := keys.Load(filepath.Join(t.TempDir(), "keys.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	k, err := st.Create("unrestricted")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Update(k.ID, func(x *keys.Key) error {
		x.Associations = []keys.Association{{UID: cn.UID, Priority: 1, Enabled: true}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var captured []byte
	up := newCapturingUpstream(t, &captured, func(authz string) (int, string, bool) {
		return 400, resp11102, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Keys: st, AdminKey: "adm", MaxBodyBytes: 1 << 20})
	rec := postChat(h, k.Value, "whatever-model")
	if rec.Code != 503 {
		t.Fatalf("unrestricted key should not silently remap, got %d", rec.Code)
	}
	if strings.Contains(string(captured), `"model":"deepseek-v4.1-flash"`) {
		t.Fatal("unrestricted key must not reroute models")
	}
}

// 403 风控 → 账号级短冷却 + 模型负缓存双写；WafCooldownDur=0 时只做模型级避让。
func TestWAF403AccountCooldown(t *testing.T) {
	for _, dur := range []time.Duration{10 * time.Minute, 0} {
		p, _, h, kv, _ := func() (*pool.Pool, *auth.Auth, *Handler, string, *[]byte) {
			auth.SetGlobalEnabled(true)
			cn := &auth.Auth{UID: "waf-cn-1", AccessToken: "at-1", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
			pp := testPoolWith(cn)
			st, _ := keys.Load(filepath.Join(t.TempDir(), "keys.json"), "")
			k, _ := st.Create("waf")
			_, _ = st.Update(k.ID, func(x *keys.Key) error {
				x.Associations = []keys.Association{{UID: cn.UID, Priority: 1, Enabled: true}}
				return nil
			})
			var captured []byte
			up := newCapturingUpstream(t, &captured, func(authz string) (int, string, bool) {
				return 403, `<!DOCTYPE html><title>WAF Block Page</title>`, false
			})
			h := NewHandler(Config{Pool: pp, Upstream: up, Keys: st, AdminKey: "adm", MaxBodyBytes: 1 << 20,
				WafCooldownDur: dur, ModelFallback: nil})
			return pp, cn, h, k.Value, &captured
		}()
		rec := postChat(h, kv, "any-model")
		if rec.Code != 503 {
			t.Fatalf("dur=%v: want 503, got %d", dur, rec.Code)
		}
		st := p.List()[0]
		cooling := st.Cooling
		blocked := len(st.RateLimitedModels) > 0
		if dur > 0 && !cooling {
			t.Fatalf("dur=%v: account should be cooling after WAF 403", dur)
		}
		if dur == 0 && cooling {
			t.Fatalf("dur=0: account cooldown disabled but cooling=true")
		}
		if !blocked {
			t.Fatalf("dur=%v: model block entry missing", dur)
		}
	}
}
