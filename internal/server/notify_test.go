// /admin/notify/test 端点测试：鉴权、未启用、发送成功/失败。
package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/pool"
)

func postNotifyTest(t *testing.T, h *Handler, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/notify/test", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAdminNotifyTestDisabled(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), APIKey: "adm", AdminKey: "adm"})
	rec := postNotifyTest(t, h, map[string]string{"Authorization": "Bearer adm"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 when NotifyTest nil, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !assertJSONErrorCode(t, rec.Body.String(), "notify_disabled") {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

func TestAdminNotifyTestAuth(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), APIKey: "adm", AdminKey: "adm", NotifyTest: func() error { return nil }})
	rec := postNotifyTest(t, h, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without admin key, got %d", rec.Code)
	}
}

func TestAdminNotifyTestSuccessAndFailure(t *testing.T) {
	ok := NewHandler(Config{Pool: pool.New(""), APIKey: "adm", AdminKey: "adm", NotifyTest: func() error { return nil }})
	rec := postNotifyTest(t, ok, map[string]string{"Authorization": "Bearer adm"})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	bad := NewHandler(Config{Pool: pool.New(""), APIKey: "adm", AdminKey: "adm", NotifyTest: func() error {
		return errors.New("SMTP 连接失败")
	}})
	rec = postNotifyTest(t, bad, map[string]string{"Authorization": "Bearer adm"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", rec.Code)
	}
	if !assertJSONErrorCode(t, rec.Body.String(), "notify_failed") {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}
