// API Key 管理接口（面板侧代理网关 /admin/keys + 账号禁用/启用）。
// 写操作受只读模式约束（ReadOnly）。
package api

import (
	"net/http"

	"workbuddy2api-gui/internal/gateway"
)

func (s *Server) handleKeysList(w http.ResponseWriter, r *http.Request) {
	keys, err := s.svc.Gateway().AdminKeys(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

func (s *Server) handleKeysCreate(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.EnsureWritable(); err != nil {
		writeError(w, err)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	k, err := s.svc.Gateway().AdminCreateKey(r.Context(), req.Name)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, k)
}

func (s *Server) handleKeysUpdate(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.EnsureWritable(); err != nil {
		writeError(w, err)
		return
	}
	var patch gateway.AdminKeyPatch
	if !decodeJSON(w, r, &patch) {
		return
	}
	k, err := s.svc.Gateway().AdminUpdateKey(r.Context(), r.PathValue("id"), patch)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, k)
}

func (s *Server) handleKeysDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.EnsureWritable(); err != nil {
		writeError(w, err)
		return
	}
	if err := s.svc.Gateway().AdminDeleteKey(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleKeysRegenerate(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.EnsureWritable(); err != nil {
		writeError(w, err)
		return
	}
	k, err := s.svc.Gateway().AdminRegenerateKey(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, k)
}

// handleAccountToggle POST /api/accounts/{uid}/toggle {"disabled": bool}
// 禁用走网关 /admin/accounts/{uid}/disable（即时生效 + 网关侧持久化，无需重启）。
func (s *Server) handleAccountToggle(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.EnsureWritable(); err != nil {
		writeError(w, err)
		return
	}
	var req struct {
		Disabled bool   `json:"disabled"`
		Reason   string `json:"reason"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !req.Disabled && req.Reason == "" {
		req.Reason = "manual disable"
	}
	if err := s.svc.Gateway().AdminAccountToggle(r.Context(), r.PathValue("uid"), req.Disabled, req.Reason); err != nil {
		writeError(w, err)
		return
	}
	msg := "账号已启用"
	if req.Disabled {
		msg = "账号已禁用（立即生效，重启保持）"
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": msg, "disabled": req.Disabled})
}
