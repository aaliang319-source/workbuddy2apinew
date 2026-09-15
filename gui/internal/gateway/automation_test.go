// automation_test.go 自动化客户端：202/409/状态解码的契约回归。
package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAutomationTriggerBusyMapping 网关 409 → ErrAutomationBusy（api 层据此回 409）。
func TestAutomationTriggerBusyMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin/automation/run/checkin" && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"task checkin is already running","code":"already_running"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := New(srv.URL, func() string { return "k" }, 0)
	if _, err := c.AutomationTrigger(context.Background(), "checkin"); !errors.Is(err, ErrAutomationBusy) {
		t.Fatalf("err=%v want ErrAutomationBusy", err)
	}
}

// TestAutomationTriggerAccepted 202 + run 载荷解码。
func TestAutomationTriggerAccepted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"kind":"checkin","run":{"id":"checkin-x","kind":"checkin","running":true,"ok":0,"failed":0,"skipped":0}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, func() string { return "test-key" }, 0)
	run, err := c.AutomationTrigger(context.Background(), "checkin")
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if run.ID != "checkin-x" || !run.Running {
		t.Errorf("run=%+v", run)
	}
}

// TestAutomationStatusDecode 状态总览解码（kinds 顺序与字段透传）。
func TestAutomationStatusDecode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"enabled":true,"now":"2026-09-18T12:00:00+08:00","kinds":[
			{"kind":"checkin","enabled":true,"hours":[9,21],"next_fire":"2026-09-18T21:00:00+08:00","running":false},
			{"kind":"cat","enabled":false,"hours":[1],"running":false}
		]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, func() string { return "k" }, 0)
	st, err := c.AutomationStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Enabled || len(st.Kinds) != 2 {
		t.Fatalf("status=%+v", st)
	}
	if st.Kinds[1].NextFire != nil {
		t.Errorf("disabled kind next_fire=%v want nil", st.Kinds[1].NextFire)
	}
}
