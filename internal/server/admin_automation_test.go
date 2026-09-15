// admin_automation_test.go /admin/automation/* 契约测试：鉴权、触发、热更新校验、历史。
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/automation"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/scheduler"
)

// autoFakeRunner automation.Runner 的最小 fake（同步立即返回固定结果）。
type autoFakeRunner struct {
	outs []scheduler.Outcome
}

func (f *autoFakeRunner) RunKind(ctx context.Context, k scheduler.Kind) []scheduler.Outcome {
	return f.outs
}

func newAutomationTestHandler(t *testing.T) *Handler {
	t.Helper()
	m := automation.New(automation.Config{
		Runner: &autoFakeRunner{outs: []scheduler.Outcome{
			{UID: "u1", Nickname: "一号", OK: true, Status: "ok"},
			{UID: "u2", OK: false, Status: "fail", Message: "boom"},
		}},
		Spec:        specWithCheckinOnly(),
		HistoryFile: filepath.Join(t.TempDir(), "automation.json"),
	})
	t.Cleanup(m.Close)
	return NewHandler(Config{
		Pool:       pool.New(""),
		APIKey:     "admin-key",
		AdminKey:   "admin-key",
		Automation: m,
	})
}

// specWithCheckinOnly 只有 checkin 启用（9 点）。
func specWithCheckinOnly() scheduler.ScheduleSpec {
	return scheduler.ScheduleSpec{
		CheckinHours:      []int{9},
		TravelHours:       []int{9},
		ActivityHours:     []int{10},
		KeepaliveHours:    []int{22},
		SchoolHours:       []int{12},
		CatHours:          []int{1},
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		SchoolDisabled:    true,
		CatDisabled:       true,
	}
}

func adminPost(h *Handler, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func adminGet(h *Handler, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer admin-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestAutomationRoutesNotRegisteredWithoutManager 未注入 Automation 时整组路由 404。
func TestAutomationRoutesNotRegisteredWithoutManager(t *testing.T) {
	h := newTestHandler(t) // 无 Automation 字段
	if rec := adminGet(h, "/admin/automation/status"); rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404", rec.Code)
	}
}

// TestAutomationStatusRequiresAuth 管理密钥缺失 → 401。
func TestAutomationStatusRequiresAuth(t *testing.T) {
	h := newAutomationTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/automation/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401", rec.Code)
	}
}

// TestAutomationStatusAndTriggerContract 状态总览 + 手动触发的完整契约。
func TestAutomationStatusAndTriggerContract(t *testing.T) {
	h := newAutomationTestHandler(t)

	rec := adminGet(h, "/admin/automation/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("status code=%d", rec.Code)
	}
	var st struct {
		Enabled bool `json:"enabled"`
		Kinds   []struct {
			Kind     string  `json:"kind"`
			Enabled  bool    `json:"enabled"`
			Hours    []int   `json:"hours"`
			NextFire *string `json:"next_fire"`
		} `json:"kinds"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &st) != nil {
		t.Fatalf("status not json: %s", rec.Body.String())
	}
	if len(st.Kinds) != 6 {
		t.Fatalf("kinds=%d want 6", len(st.Kinds))
	}
	for _, k := range st.Kinds {
		if k.Kind == "checkin" {
			if !k.Enabled || len(k.Hours) != 1 || k.Hours[0] != 9 {
				t.Errorf("checkin status: %+v", k)
			}
			if k.NextFire == nil {
				t.Error("checkin next_fire nil")
			}
		} else if k.NextFire != nil {
			t.Errorf("%s disabled → next_fire 应为空", k.Kind)
		}
	}

	// 触发：202 + 运行中记录（fake 同步返回，可能已完成——只断言结构）。
	rec = adminPost(h, "/admin/automation/run/checkin", "{}")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("trigger code=%d body=%s", rec.Code, rec.Body.String())
	}
	var tr struct {
		Run struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
		} `json:"run"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &tr) != nil || tr.Run.ID == "" {
		t.Fatalf("trigger body=%s", rec.Body.String())
	}
	// 详情：轮询直到完成（fake 同步，通常立即完成）。
	deadline := 50
	var detail struct {
		ID     string `json:"id"`
		OK     int    `json:"ok"`
		Failed int    `json:"failed"`
		Items  []struct {
			UID    string `json:"uid"`
			Status string `json:"status"`
		} `json:"items"`
		Running bool `json:"running"`
	}
	for i := 0; i < deadline; i++ {
		rec = adminGet(h, "/admin/automation/runs/"+tr.Run.ID)
		if rec.Code != http.StatusOK {
			t.Fatalf("run detail code=%d", rec.Code)
		}
		if json.Unmarshal(rec.Body.Bytes(), &detail) != nil {
			t.Fatalf("run detail not json")
		}
		if !detail.Running {
			break
		}
	}
	if detail.OK != 1 || detail.Failed != 1 || len(detail.Items) != 2 {
		t.Errorf("detail=%+v", detail)
	}

	// 历史列表。
	rec = adminGet(h, "/admin/automation/runs?limit=5")
	if rec.Code != http.StatusOK {
		t.Fatalf("runs code=%d", rec.Code)
	}
	var list struct {
		Runs []struct {
			ID string `json:"id"`
		} `json:"runs"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list.Runs) == 0 {
		t.Fatalf("runs body=%s", rec.Body.String())
	}

	// 未知 run → 404。
	rec = adminGet(h, "/admin/automation/runs/nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown run code=%d want 404", rec.Code)
	}

	// 未知类型 → 400。
	if rec = adminPost(h, "/admin/automation/run/nope", "{}"); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown kind code=%d want 400", rec.Code)
	}
}

// TestAutomationApplyValidation 排程热更新：非法小时 400，合法后 status 立即反映。
func TestAutomationApplyValidation(t *testing.T) {
	h := newAutomationTestHandler(t)

	// 非法小时。
	rec := adminPost(h, "/admin/automation/apply", `{"schedule":{"checkin_hours":[25],"checkin_enabled":true}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "不是合法小时") {
		t.Errorf("body=%s want hour guidance", rec.Body.String())
	}

	// 启用但空小时。
	rec = adminPost(h, "/admin/automation/apply", `{"schedule":{"checkin_hours":[],"checkin_enabled":true}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty hours code=%d want 400", rec.Code)
	}

	// 合法：checkin 20 点启用 + travel 禁用。
	rec = adminPost(h, "/admin/automation/apply",
		`{"schedule":{"checkin_hours":[20],"checkin_enabled":true,"travel_enabled":false,"travel_hours":[9]}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status struct {
			Kinds []struct {
				Kind     string  `json:"kind"`
				Enabled  bool    `json:"enabled"`
				Hours    []int   `json:"hours"`
				NextFire *string `json:"next_fire"`
			} `json:"kinds"`
		} `json:"status"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &resp) != nil {
		t.Fatalf("apply response not json")
	}
	for _, k := range resp.Status.Kinds {
		if k.Kind != "checkin" {
			continue
		}
		if !k.Enabled || k.Hours[0] != 20 || k.NextFire == nil {
			t.Errorf("checkin after apply: %+v", k)
		}
		if got := k.NextFire; got != nil && !strings.Contains(*got, "T20:00:00") {
			t.Errorf("next_fire=%s want 20:00:00（热更新立即生效）", *got)
		}
	}
}
