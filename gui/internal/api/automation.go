// automation.go 自动化面板 API：代理网关 /admin/automation/*，并在保存排程时
// 先写 config.json（面板是唯一文件写者）再调网关热生效。
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"workbuddy2api-gui/internal/gateway"
)

// handleAutomationStatus GET /api/automation/status
func (s *Server) handleAutomationStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.svc.Gateway().AutomationStatus(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// handleAutomationRuns GET /api/automation/runs?limit=20
func (s *Server) handleAutomationRuns(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	runs, err := s.svc.Gateway().AutomationRuns(r.Context(), limit)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

// handleAutomationRun GET /api/automation/runs/{id}
func (s *Server) handleAutomationRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.svc.Gateway().AutomationRun(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// handleAutomationTrigger POST /api/automation/{kind}/run
// 触发会真实打上游（脚本类还有写副作用），因此与其它写操作一样受 read_only 约束。
func (s *Server) handleAutomationTrigger(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.EnsureWritable(); err != nil {
		writeError(w, err)
		return
	}
	run, err := s.svc.Gateway().AutomationTrigger(r.Context(), r.PathValue("kind"))
	if err != nil {
		if errors.Is(err, gateway.ErrAutomationBusy) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": "already_running"})
			return
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"run": run})
}

// handleAutomationApply POST /api/automation/apply
// body {"schedule":{...}}：先落盘 config.json（重启后仍生效），再让网关热生效
// （只改内存，不重启）。两步都成功才返回 ok。
func (s *Server) handleAutomationApply(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.EnsureWritable(); err != nil {
		writeError(w, err)
		return
	}
	var req struct {
		Schedule map[string]any `json:"schedule"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON: " + err.Error(), "code": "invalid_json"})
		return
	}
	if req.Schedule == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "schedule 字段必填", "code": "invalid_schedule"})
		return
	}
	if _, err := s.svc.SaveSchedule(req.Schedule); err != nil {
		writeError(w, err)
		return
	}
	st, err := s.svc.Gateway().AutomationApply(r.Context(), req.Schedule)
	if err != nil {
		// 文件已写、热生效失败：明确告知（重启网关即按新配置生效）。
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "配置已写入 config.json，但网关热生效失败（重启网关后生效）: " + err.Error(),
			"code":  "apply_failed",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": st})
}
