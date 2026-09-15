// 管理端 API：/admin/keys（多业务 Key CRUD + 关联管理）、/admin/accounts（账号禁用/启用）。
// 鉴权：withAdminAuth（仅管理密钥 AdminKey/APIKey；业务 Key 不可访问）。
// 多 Key 数据存 keys.Store（data/keys.json），变更即落盘热生效，无需重启。
package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"workbuddy2api/internal/keys"
)

// registerAdmin 注册管理端路由（NewHandler 调用一次）。
func (h *Handler) registerAdmin() {
	h.mux.HandleFunc("GET /admin/keys", h.withAdminAuth(h.adminListKeys))
	h.mux.HandleFunc("POST /admin/keys", h.withAdminAuth(h.adminCreateKey))
	h.mux.HandleFunc("GET /admin/keys/{id}", h.withAdminAuth(h.adminGetKey))
	h.mux.HandleFunc("PUT /admin/keys/{id}", h.withAdminAuth(h.adminUpdateKey))
	h.mux.HandleFunc("DELETE /admin/keys/{id}", h.withAdminAuth(h.adminDeleteKey))
	h.mux.HandleFunc("POST /admin/keys/{id}/regenerate", h.withAdminAuth(h.adminRegenerateKey))
	h.mux.HandleFunc("GET /admin/accounts", h.withAdminAuth(h.adminListAccounts))
	h.mux.HandleFunc("POST /admin/accounts/{uid}/disable", h.withAdminAuth(h.adminDisableAccount))
	h.mux.HandleFunc("POST /admin/accounts/{uid}/enable", h.withAdminAuth(h.adminEnableAccount))
	// 通知：发送测试邮件（验证 SMTP 配置）
	h.mux.HandleFunc("POST /admin/notify/test", h.withAdminAuth(h.adminNotifyTest))
}

// keysReady 管理端依赖 keys.Store；旧单 Key 模式（未注入）下管理端不可用。
func (h *Handler) keysReady(w http.ResponseWriter) bool {
	if h.cfg.Keys == nil {
		writeOpenAIError(w, http.StatusBadRequest, "keys_disabled", "multi-key mode not enabled")
		return false
	}
	return true
}

// adminListKeys GET /admin/keys：全部 Key（含完整 value，面板需生成 cc-switch 深链）。
func (h *Handler) adminListKeys(w http.ResponseWriter, r *http.Request) {
	if !h.keysReady(w) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": h.cfg.Keys.List()})
}

// adminCreateKey POST /admin/keys  {"name": "..."} → Key
func (h *Handler) adminCreateKey(w http.ResponseWriter, r *http.Request) {
	if !h.keysReady(w) {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	k, err := h.cfg.Keys.Create(strings.TrimSpace(body.Name))
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "create_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, k)
}

// adminGetKey GET /admin/keys/{id}
func (h *Handler) adminGetKey(w http.ResponseWriter, r *http.Request) {
	if !h.keysReady(w) {
		return
	}
	k, ok := h.cfg.Keys.Get(r.PathValue("id"))
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "key_not_found", "key not found")
		return
	}
	writeJSON(w, http.StatusOK, k)
}

// adminUpdateKey PUT /admin/keys/{id}
// {"name"?: string, "enabled"?: bool, "associations"?: [{uid,priority,enabled}]}
// 全量替换关联（面板编辑提交完整列表；增量在前端拼装）。
func (h *Handler) adminUpdateKey(w http.ResponseWriter, r *http.Request) {
	if !h.keysReady(w) {
		return
	}
	var body struct {
		Name         *string             `json:"name"`
		Enabled      *bool               `json:"enabled"`
		Associations *[]keys.Association `json:"associations"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	k, err := h.cfg.Keys.Update(r.PathValue("id"), func(x *keys.Key) error {
		if body.Name != nil {
			x.Name = strings.TrimSpace(*body.Name)
		}
		if body.Enabled != nil {
			x.Enabled = *body.Enabled
		}
		if body.Associations != nil {
			x.Associations = *body.Associations
		}
		return nil
	})
	if err == keys.ErrNotFound {
		writeOpenAIError(w, http.StatusNotFound, "key_not_found", "key not found")
		return
	}
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "update_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, k)
}

// adminDeleteKey DELETE /admin/keys/{id}
func (h *Handler) adminDeleteKey(w http.ResponseWriter, r *http.Request) {
	if !h.keysReady(w) {
		return
	}
	err := h.cfg.Keys.Delete(r.PathValue("id"))
	if err == keys.ErrNotFound {
		writeOpenAIError(w, http.StatusNotFound, "key_not_found", "key not found")
		return
	}
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "delete_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// adminRegenerateKey POST /admin/keys/{id}/regenerate：重置 value（旧值立即失效）。
func (h *Handler) adminRegenerateKey(w http.ResponseWriter, r *http.Request) {
	if !h.keysReady(w) {
		return
	}
	k, err := h.cfg.Keys.Regenerate(r.PathValue("id"))
	if err == keys.ErrNotFound {
		writeOpenAIError(w, http.StatusNotFound, "key_not_found", "key not found")
		return
	}
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "regenerate_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, k)
}

// adminListAccounts GET /admin/accounts：全部账号状态（pool.List，含禁用/冷却/余额）。
func (h *Handler) adminListAccounts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"accounts": h.cfg.Pool.List()})
}

// adminDisableAccount POST /admin/accounts/{uid}/disable {"reason"?: string}
// 禁用后该账号立即不可被任何 Key 选中，状态持久化 state.json（重启保持）。
func (h *Handler) adminDisableAccount(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // body 可选
	reason := body.Reason
	if reason == "" {
		reason = "manual disable"
	}
	h.cfg.Pool.Disable(uid, reason)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "disabled": true})
}

// adminEnableAccount POST /admin/accounts/{uid}/enable：复活禁用账号。
func (h *Handler) adminEnableAccount(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	h.cfg.Pool.ReviveDisabled(uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "disabled": false})
}
