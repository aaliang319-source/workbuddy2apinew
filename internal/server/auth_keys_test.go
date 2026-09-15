// 鉴权层回归测试：多 Key 模式下 Bearer/X-Api-Key 双头回落与禁用/未关联语义。
package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/keys"
	"workbuddy2api/internal/pool"
)

// newTestHandler 构建多 Key 模式 handler：管理密钥 = legacy-admin-key（已迁移为 default）。
func newTestHandler(t *testing.T) *Handler {
	t.Helper()
	st, err := keys.Load(filepath.Join(t.TempDir(), "keys.json"), "legacy-admin-key")
	if err != nil {
		t.Fatalf("load keys: %v", err)
	}
	return NewHandler(Config{
		Pool:     pool.New(""),
		Upstream: nil,
		APIKey:   "legacy-admin-key",
		Keys:     st,
		AdminKey: "legacy-admin-key",
	})
}

// seedKey 新建业务 Key 并写入关联，返回 (id, value)。
func seedKey(t *testing.T, h *Handler, name string, assocs []keys.Association) (string, string) {
	t.Helper()
	k, err := h.cfg.Keys.Create(name)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if _, err := h.cfg.Keys.Update(k.ID, func(x *keys.Key) error { x.Associations = assocs; return nil }); err != nil {
		t.Fatalf("update key: %v", err)
	}
	return k.ID, k.Value
}

func probe(h *Handler, headers map[string]string, path string) (*httptest.ResponseRecorder, *http.Request) {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, req
}

func TestMultiKeyAuthBearerPriority(t *testing.T) {
	h := newTestHandler(t)
	_, val := seedKey(t, h, "k", []keys.Association{{UID: "u1", Priority: 1, Enabled: true}})

	// 业务 Key 经 Bearer 头命中（回归：曾因回落比较覆盖 bearer 而误 401）。
	rec, _ := probe(h, map[string]string{"Authorization": "Bearer " + val}, "/v1/models")
	if rec.Code != http.StatusOK {
		t.Fatalf("bearer business key: want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	// X-Api-Key 头同命中。
	rec, _ = probe(h, map[string]string{"X-Api-Key": val}, "/v1/models")
	if rec.Code != http.StatusOK {
		t.Fatalf("x-api-key business key: want 200, got %d", rec.Code)
	}
	// bearer 与管理密钥不同且非法 → 仍 401。
	rec, _ = probe(h, map[string]string{"Authorization": "Bearer nope"}, "/v1/models")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad bearer: want 401, got %d", rec.Code)
	}
}

func TestMultiKeyAuthDisabledAndUnprovisioned(t *testing.T) {
	h := newTestHandler(t)
	id, val := seedKey(t, h, "k", []keys.Association{{UID: "u1", Priority: 1, Enabled: true}})

	// 禁用 → 401 key_disabled。
	if _, err := h.cfg.Keys.Update(id, func(x *keys.Key) error { x.Enabled = false; return nil }); err != nil {
		t.Fatal(err)
	}
	rec, _ := probe(h, map[string]string{"Authorization": "Bearer " + val}, "/v1/models")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled key: want 401, got %d", rec.Code)
	}

	// 启用但清空关联 → 403 key_not_provisioned（新建 Key 不允许通配，必须先关联）。
	if _, err := h.cfg.Keys.Update(id, func(x *keys.Key) error { x.Enabled = true; x.Associations = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	rec, _ = probe(h, map[string]string{"Authorization": "Bearer " + val}, "/v1/models")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unprovisioned key: want 403, got %d", rec.Code)
	}
}

func TestMigratedWildcardKeyNeedsNoAssociation(t *testing.T) {
	h := newTestHandler(t)
	// 迁移生成的 default Key（Wildcard）未关联账号也可通过鉴权（存量客户端零感知）。
	rec, _ := probe(h, map[string]string{"Authorization": "Bearer legacy-admin-key"}, "/v1/models")
	if rec.Code != http.StatusOK {
		t.Fatalf("migrated wildcard key: want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	ks := h.cfg.Keys.List()
	if len(ks) != 1 || !ks[0].Wildcard {
		t.Fatalf("migrated key must be wildcard: %+v", ks)
	}
}

func TestAdminAuthRejectsBusinessKey(t *testing.T) {
	h := newTestHandler(t)
	_, val := seedKey(t, h, "k", []keys.Association{{UID: "u1", Priority: 1, Enabled: true}})

	// 业务 Key 访问管理端 → 401。
	rec, _ := probe(h, map[string]string{"Authorization": "Bearer " + val}, "/admin/keys")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("business key on admin: want 401, got %d", rec.Code)
	}
	// 管理密钥 → 200。
	rec, _ = probe(h, map[string]string{"Authorization": "Bearer legacy-admin-key"}, "/admin/keys")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin key on admin: want 200, got %d", rec.Code)
	}
}
