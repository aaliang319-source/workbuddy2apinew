// 通知（邮件提醒）接口：代理网关 /admin/notify/test。
package api

import "net/http"

// handleNotifyTest POST /api/notify/test：让网关立即发一封测试邮件。
// 写操作受只读模式约束；网关侧通知未启用时返回 400（错误信息原样透给用户）。
func (s *Server) handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.EnsureWritable(); err != nil {
		writeError(w, err)
		return
	}
	if err := s.svc.Gateway().AdminNotifyTest(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "测试邮件已发送，请查收（含垃圾箱）"})
}
