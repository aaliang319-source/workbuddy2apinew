// mix_realms_test.go 混合调度（config global.mix_realms）测试：
//   - 跨域选号：Key 关联优先级在 CN/global 账号间生效（mix 开）；mix 关时维持分池隔离。
//   - 模型名统一：mix 开时 /v1/models 输出去前缀且同名去重。
package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/keys"
	"workbuddy2api/internal/pool"
)

// mixTestPool 构建一个 CN + 一个 global 账号的池（均为 far-future 有效 token）。
func mixTestPool(cnUID, glUID string) (*pool.Pool, *auth.Auth, *auth.Auth) {
	auth.SetGlobalEnabled(true)
	cn := &auth.Auth{UID: cnUID, AccessToken: "at-cn", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	gl := &auth.Auth{UID: glUID, AccessToken: "at-gl", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"}
	p := testPoolWith(cn, gl)
	return p, cn, gl
}

// mixKey 建一个 Key：global 账号优先级 100、CN 账号 1（分层应恒选 global）。
func mixKey(t *testing.T, h *Handler, cnUID, glUID string) string {
	t.Helper()
	k, err := h.cfg.Keys.Create("mix")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if _, err := h.cfg.Keys.Update(k.ID, func(x *keys.Key) error {
		x.Associations = []keys.Association{
			{UID: glUID, Priority: 100, Enabled: true},
			{UID: cnUID, Priority: 1, Enabled: true},
		}
		return nil
	}); err != nil {
		t.Fatalf("update key: %v", err)
	}
	return k.Value
}

// servedUID 返回池中成功计数 > 0 的账号 UID（本测试每个请求只服务一次）。
func servedUID(p *pool.Pool) string {
	for _, st := range p.List() {
		if st.SuccessCount > 0 {
			return st.UID
		}
	}
	return ""
}

func TestMixRealmsCrossRealmPriority(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })

	// mix=false：bare 模型 → 分池隔离，只走 CN（global 虽 prio 100 也不参与）。
	p1, cn1, gl1 := mixTestPool("cn-1", "gl-1")
	st1, err := keys.Load(filepath.Join(t.TempDir(), "keys.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	h1 := NewHandler(Config{Pool: p1, Upstream: up, Keys: st1, AdminKey: "adm", MaxBodyBytes: 1 << 20})
	kv1 := mixKey(t, h1, cn1.UID, gl1.UID)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+kv1)
	h1.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("mix off: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := servedUID(p1); got != cn1.UID {
		t.Fatalf("mix off: want CN account %s (realm isolation), got %q", cn1.UID, got)
	}

	// mix=true：同一配置下 global 账号（prio 100）应被选中——跨域分层生效。
	p2, cn2, gl2 := mixTestPool("cn-2", "gl-2")
	st2, err := keys.Load(filepath.Join(t.TempDir(), "keys.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	h2 := NewHandler(Config{Pool: p2, Upstream: up, Keys: st2, AdminKey: "adm", MaxBodyBytes: 1 << 20, MixRealms: true})
	kv2 := mixKey(t, h2, cn2.UID, gl2.UID)
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`))
	req2.Header.Set("Authorization", "Bearer "+kv2)
	h2.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("mix on: status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	if got := servedUID(p2); got != gl2.UID {
		t.Fatalf("mix on: want global account %s (cross-realm priority), got %q", gl2.UID, got)
	}
}

func TestMixRealmsGlobalPrefixStillStripped(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p, cn, gl := mixTestPool("cn-3", "gl-3")
	st, err := keys.Load(filepath.Join(t.TempDir(), "keys.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: p, Upstream: up, Keys: st, AdminKey: "adm", MaxBodyBytes: 1 << 20, MixRealms: true})
	kv := mixKey(t, h, cn.UID, gl.UID)
	// 老客户端仍带 global: 前缀：mix 下同样走混合池（且出站仍是裸名，不因前缀 400）。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"global:glm-5.3","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+kv)
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := servedUID(p); got != gl.UID {
		t.Fatalf("want global account %s, got %q", gl.UID, got)
	}
}

func TestModelListUnifiedWhenMix(t *testing.T) {
	resetModelsCache()
	emptyUp := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	base := Config{Pool: pool.New(""), Upstream: emptyUp, MaxBodyBytes: 1 << 20}

	// 统一前：CN 带 cn: 前缀、global 带 global: 前缀。
	base.GlobalEnabled = true
	h0 := NewHandler(base)
	prefixed := 0
	for _, e := range h0.modelList() {
		if id, _ := e["id"].(string); strings.HasPrefix(id, "cn:") || strings.HasPrefix(id, "global:") {
			prefixed++
		}
	}
	if prefixed == 0 {
		t.Fatal("non-mix list should carry realm prefixes")
	}

	// 统一后：无前缀 + 同名去重（两域同名模型只出现一次）。
	resetModelsCache()
	h1 := NewHandler(Config{Pool: pool.New(""), Upstream: emptyUp, MaxBodyBytes: 1 << 20, GlobalEnabled: true, MixRealms: true})
	seen := map[string]bool{}
	for _, e := range h1.modelList() {
		id, _ := e["id"].(string)
		if strings.HasPrefix(id, "cn:") || strings.HasPrefix(id, "global:") {
			t.Fatalf("mix list must not carry realm prefix: %s", id)
		}
		if seen[id] {
			t.Fatalf("mix list must dedupe duplicate model id: %s", id)
		}
		seen[id] = true
	}
	if len(seen) == 0 {
		t.Fatal("mix list empty")
	}
}

// TestMixRealms403NegativeCache 混合调度下 global 账号被上游 403（WAF）拒绝时：
// 首条请求换号成功；此后该 (账号, 模型) 进入负缓存，后续请求不再先撞 403（尝试=1）。
func TestMixRealms403NegativeCache(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if strings.Contains(authz, "at-gl") {
			// global 上游 WAF 拦截页（实测形态：403 + HTML）
			return 403, `<!DOCTYPE html><html><title>WAF Block Page</title></html>`, false
		}
		return 200, sseOK, true
	})
	p, cn, gl := mixTestPool("cn-403", "gl-403")
	st, err := keys.Load(filepath.Join(t.TempDir(), "keys.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: p, Upstream: up, Keys: st, AdminKey: "adm", MaxBodyBytes: 1 << 20, MixRealms: true})
	kv := mixKey(t, h, cn.UID, gl.UID)

	body := `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`
	send := func() int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+kv)
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	// 第 1 条：gl 撞 403 → 轮转 cn 成功。
	if code := send(); code != 200 {
		t.Fatalf("request1: want 200, got %d", code)
	}
	counts := func() map[string]int64 {
		m := map[string]int64{}
		for _, s := range p.List() {
			m[s.UID] = s.SuccessCount
		}
		return m
	}
	if counts()[cn.UID] != 1 || counts()[gl.UID] != 0 {
		t.Fatalf("request1 distribution wrong: %v", counts())
	}
	// 负缓存已写入：gl 对该模型进入台账避让。
	var blocked bool
	for _, s := range p.List() {
		if s.UID == gl.UID && len(s.RateLimitedModels) > 0 {
			blocked = true
		}
	}
	if !blocked {
		t.Fatal("global account should have model block entry after 403")
	}

	// 第 2 条：负缓存生效 → 直接 cn 成功，不再先撞 gl（这是修复的核心断言）。
	if code := send(); code != 200 {
		t.Fatalf("request2: want 200, got %d", code)
	}
	if counts()[cn.UID] != 2 || counts()[gl.UID] != 0 {
		t.Fatalf("request2 distribution wrong (gl must stay avoided): %v", counts())
	}
}
