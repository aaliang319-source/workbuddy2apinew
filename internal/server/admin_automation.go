// admin_automation.go 自动化管理端：状态总览 / 运行历史 / 手动触发 / 排程热更新。
//
// 鉴权：withAdminAuth（仅管理密钥）。数据源是 internal/automation.Manager；本文件只做
// 协议转换，不含任务逻辑。
//
// 重要约束：/apply 只改**内存排程**，永不写 config.json——网关容器里该文件是只读挂载，
// 面板才是唯一文件写者（面板保存文件成功后再调本接口，避免双写者互相覆盖）。
package server

import (
	"encoding/json"
	"net/http"
	"strconv"

	"workbuddy2api/internal/automation"
	"workbuddy2api/internal/scheduler"
)

// registerAutomation 注册自动化路由（Automation 未注入时不注册：404 保持"功能未启用"语义）。
func (h *Handler) registerAutomation() {
	if h.cfg.Automation == nil {
		return
	}
	h.mux.HandleFunc("GET /admin/automation/status", h.withAdminAuth(h.adminAutomationStatus))
	h.mux.HandleFunc("GET /admin/automation/runs", h.withAdminAuth(h.adminAutomationRuns))
	h.mux.HandleFunc("GET /admin/automation/runs/{id}", h.withAdminAuth(h.adminAutomationRun))
	h.mux.HandleFunc("POST /admin/automation/run/{kind}", h.withAdminAuth(h.adminAutomationTrigger))
	h.mux.HandleFunc("POST /admin/automation/apply", h.withAdminAuth(h.adminAutomationApply))
}

// adminAutomationStatus GET /admin/automation/status
func (h *Handler) adminAutomationStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.cfg.Automation.Status())
}

// adminAutomationRuns GET /admin/automation/runs?limit=20
func (h *Handler) adminAutomationRuns(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
			if limit > 200 {
				limit = 200
			}
		}
	}
	runs := h.cfg.Automation.Runs(limit)
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

// adminAutomationRun GET /admin/automation/runs/{id}
func (h *Handler) adminAutomationRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run, ok := h.cfg.Automation.GetRun(id)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "run_not_found", "run not found: "+id)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// adminAutomationTrigger POST /admin/automation/run/{kind}
// 202 + 运行中记录（异步执行）；同类型在跑 → 409 busy；未知类型 → 400。
func (h *Handler) adminAutomationTrigger(w http.ResponseWriter, r *http.Request) {
	kindStr := r.PathValue("kind")
	kind, ok := automation.ParseKind(kindStr)
	if !ok {
		writeOpenAIError(w, http.StatusBadRequest, "bad_kind", "unknown task kind: "+kindStr)
		return
	}
	run, err := h.cfg.Automation.Trigger(kind)
	if err != nil {
		writeOpenAIError(w, http.StatusConflict, "already_running",
			"task "+kindStr+" is already running, please wait")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"kind": string(kind), "run": run})
}

// adminAutomationApply POST /admin/automation/apply
// body {"schedule":{...}}：校验后排程热生效（不写文件、不重启）。
func (h *Handler) adminAutomationApply(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Schedule *scheduleBody `json:"schedule"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if body.Schedule == nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_schedule", "schedule object is required")
		return
	}
	spec := body.Schedule.toSpec(h.cfg.Automation.Spec())
	if err := h.cfg.Automation.Reconfigure(spec); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_schedule", err.Error())
		return
	}
	// 回读生效后的状态（含重新计算的 next_fire），面板可直接刷新展示。
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": h.cfg.Automation.Status()})
}

// scheduleBody 排程请求体：字段名与 config.json 的 schedule 段一致，便于面板整段透传。
// 合并语义：nil 字段 = 保持现值（支持部分更新）；显式 [] = 覆盖为空。
type scheduleBody struct {
	CheckinHours     *[]int `json:"checkin_hours"`
	TravelHours      *[]int `json:"travel_hours"`
	ActivityHours    *[]int `json:"activity_hours"`
	KeepaliveHours   *[]int `json:"keepalive_hours"`
	SchoolHours      *[]int `json:"school_hours"`
	CatHours         *[]int `json:"cat_hours"`
	CheckinEnabled   *bool  `json:"checkin_enabled"`
	TravelEnabled    *bool  `json:"travel_enabled"`
	ActivityEnabled  *bool  `json:"activity_enabled"`
	KeepaliveEnabled *bool  `json:"keepalive_enabled"`
	SchoolEnabled    *bool  `json:"school_enabled"`
	CatEnabled       *bool  `json:"cat_enabled"`
	// ActivityReportCount 当前不支持热更新（需重建 Scheduler），仅为契约完整性保留；
	// 显式传值会被忽略（面板需标注"改动后需重启"）。
	ActivityReportCount *int `json:"activity_report_count"`
}

// toSpec 基于当前内存排程合并请求字段，产出新规格。
func (b *scheduleBody) toSpec(cur scheduler.ScheduleSpec) scheduler.ScheduleSpec {
	spec := cur
	if b.CheckinHours != nil {
		spec.CheckinHours = *b.CheckinHours
	}
	if b.TravelHours != nil {
		spec.TravelHours = *b.TravelHours
	}
	if b.ActivityHours != nil {
		spec.ActivityHours = *b.ActivityHours
	}
	if b.KeepaliveHours != nil {
		spec.KeepaliveHours = *b.KeepaliveHours
	}
	if b.SchoolHours != nil {
		spec.SchoolHours = *b.SchoolHours
	}
	if b.CatHours != nil {
		spec.CatHours = *b.CatHours
	}
	if b.CheckinEnabled != nil {
		spec.CheckinDisabled = !*b.CheckinEnabled
	}
	if b.TravelEnabled != nil {
		spec.TravelDisabled = !*b.TravelEnabled
	}
	if b.ActivityEnabled != nil {
		spec.ActivityDisabled = !*b.ActivityEnabled
	}
	if b.KeepaliveEnabled != nil {
		spec.KeepaliveDisabled = !*b.KeepaliveEnabled
	}
	if b.SchoolEnabled != nil {
		spec.SchoolDisabled = !*b.SchoolEnabled
	}
	if b.CatEnabled != nil {
		spec.CatDisabled = !*b.CatEnabled
	}
	return spec
}
