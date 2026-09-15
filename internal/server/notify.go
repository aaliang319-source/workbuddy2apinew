// 通知（邮件提醒）管理端点：发送测试邮件以验证 SMTP 配置。
package server

import (
	"net/http"
)

// adminNotifyTest POST /admin/notify/test
// 通知未启用（NotifyTest==nil）→ 400 notify_disabled；发送失败 → 500 notify_failed。
// 同步发送：面板点一下即可知道配置对不对（不经队列、不受节流限制）。
func (h *Handler) adminNotifyTest(w http.ResponseWriter, r *http.Request) {
	if h.cfg.NotifyTest == nil {
		writeOpenAIError(w, http.StatusBadRequest, "notify_disabled",
			"notify disabled: 请先在网关配置中启用通知并填写 SMTP（smtp_host/smtp_from/smtp_to），保存后重启网关")
		return
	}
	if err := h.cfg.NotifyTest(); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "notify_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "测试邮件已发送，请查收（含垃圾箱）"})
}
