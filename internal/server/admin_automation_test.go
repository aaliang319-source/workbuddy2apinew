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
	"time"

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
		// 定时循环必须真跑起来：面板的 next_fire 读的是**循环排定时掷出的落点**
		// （Randomizer.Scheduled），不跑循环就永远读不到值——那是「待排定」，
		// 不是「没实现」。这也正是生产路径：main.go 传的就是同一个实例。
		LoopEnabled: true,
	})
	t.Cleanup(m.Close)
	ctx, cancel := context.WithCancel(context.Background())
	go m.Run(ctx)
	t.Cleanup(cancel)
	return NewHandler(Config{
		Pool:       pool.New(""),
		APIKey:     "admin-key",
		AdminKey:   "admin-key",
		Automation: m,
	})
}

// statusResp /admin/automation/status 的响应契约（面板与测试共用同一份字段名）。
type statusResp struct {
	Enabled bool `json:"enabled"`
	Kinds   []struct {
		Kind          string  `json:"kind"`
		Enabled       bool    `json:"enabled"`
		Hours         []int   `json:"hours"`
		NextFire      *string `json:"next_fire"`
		JitterMinutes int     `json:"jitter_minutes"`
	} `json:"kinds"`
}

func getStatus(t *testing.T, h *Handler) statusResp {
	t.Helper()
	rec := adminGet(h, "/admin/automation/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("status code=%d", rec.Code)
	}
	var st statusResp
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("status not json: %s", rec.Body.String())
	}
	return st
}

// waitNextFire 轮询状态直到 kind 的 next_fire 出现并落在 wantHour 窗口内
// （wantHour < 0 表示不关心小时）。
//
// 必须有这个轮询：**排定是定时循环异步做的**（它掷随机数并记下落点），热更新后
// 还要等循环被唤醒重排。立即断言会与循环赛跑——那不是被测代码的问题。
func waitNextFire(t *testing.T, h *Handler, kind string, wantHour int) *time.Time {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, k := range getStatus(t, h).Kinds {
			if k.Kind != kind || k.NextFire == nil {
				continue
			}
			at, err := time.Parse(time.RFC3339, *k.NextFire)
			if err != nil {
				continue
			}
			if wantHour < 0 || at.Hour() == wantHour {
				return &at
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
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
	var st statusResp
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
			if k.JitterMinutes != 55 {
				t.Errorf("checkin jitter_minutes=%d want 55（默认跨度）", k.JitterMinutes)
			}
		} else if k.NextFire != nil {
			t.Errorf("%s disabled → next_fire 应为空（禁用任务不排定）", k.Kind)
		}
	}
	if at := waitNextFire(t, h, "checkin", 9); at == nil {
		t.Error("checkin 已启用但 never 排定出 next_fire")
	} else if at.Minute() == 0 && at.Second() == 0 {
		t.Errorf("next_fire=%v 落在整点——应已被抖动打散", at)
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
		Status statusResp `json:"status"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &resp) != nil {
		t.Fatalf("apply response not json")
	}
	for _, k := range resp.Status.Kinds {
		if k.Kind == "checkin" && (!k.Enabled || k.Hours[0] != 20) {
			t.Errorf("checkin after apply: %+v", k)
		}
	}
	// 热更新立即生效：next_fire 落到新配的 20 点窗口内。
	// 具体分钟由现掷的随机偏移决定（刻意避开整点），故只断言小时与"非整点"。
	// 循环需要被唤醒后才会重排，所以这里轮询而不是立即断言。
	at := waitNextFire(t, h, "checkin", 20)
	if at == nil {
		t.Fatalf("热更新后 checkin 未在 3s 内重排到 20 点窗口：%s", dumpStatus(t, h))
	}
	if at.Minute() == 0 && at.Second() == 0 {
		t.Errorf("next_fire=%v 落在整点——应已被抖动打散", at)
	}
}

// dumpStatus 状态原始 JSON（断言失败时打印，便于看清到底排到了哪一小时）。
func dumpStatus(t *testing.T, h *Handler) string {
	t.Helper()
	return adminGet(h, "/admin/automation/status").Body.String()
}

// TestAutomationApplyJitterDisabled 显式要求准点：jitter_disabled=true 后
// next_fire 落在整点，且 status 回报的跨度为 0——面板据此显示"准点执行"。
func TestAutomationApplyJitterDisabled(t *testing.T) {
	h := newAutomationTestHandler(t)
	rec := adminPost(h, "/admin/automation/apply", `{"schedule":{"jitter_disabled":true}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply code=%d body=%s", rec.Code, rec.Body.String())
	}
	at := waitNextFire(t, h, "checkin", -1)
	if at == nil {
		t.Fatalf("禁用抖动后未排定 next_fire：%s", dumpStatus(t, h))
	}
	if at.Minute() != 0 || at.Second() != 0 {
		t.Errorf("jitter_disabled=true 却排到 %v（want 整点）", at)
	}
	for _, k := range getStatus(t, h).Kinds {
		if k.Kind == "checkin" && k.JitterMinutes != 0 {
			t.Errorf("jitter_minutes=%d want 0（准点）", k.JitterMinutes)
		}
	}
}
