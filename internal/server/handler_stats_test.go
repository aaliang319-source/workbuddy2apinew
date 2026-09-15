// handler_stats_test.go /v1/stats 端点与观测埋点。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/metrics"
)

func statsHandler(t *testing.T) (*Handler, *metrics.Tracker) {
	t.Helper()
	tr := metrics.New(filepath.Join(t.TempDir(), "metrics.json"))
	t.Cleanup(tr.Close)
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
		Metrics:  tr,
	})
	return h, tr
}

func getJSON(t *testing.T, h *Handler, method, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	h.ServeHTTP(rec, req)
	var body map[string]any
	if rec.Body != nil {
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
	}
	return rec.Code, body
}

func TestStatsDisabledReturnsEnabledFalse(t *testing.T) {
	// Metrics==nil（server.metrics_enabled=false）→ 200 + enabled=false，
	// message 提示 server.metrics_enabled（面板 fallback 文案依赖该键名）。
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
	})
	code, body := getJSON(t, h, "GET", "/v1/stats")
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if enabled, _ := body["enabled"].(bool); enabled {
		t.Errorf("enabled=%v want false", body["enabled"])
	}
	if msg, _ := body["message"].(string); !strings.Contains(msg, "metrics_enabled") {
		t.Errorf("message=%q want contains metrics_enabled", msg)
	}
}

func TestStatsStreamObservation(t *testing.T) {
	h, tr := statsHandler(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`))
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("chat code=%d", rec.Code)
	}
	s := tr.Snapshot()
	if len(s.Models) != 1 {
		t.Fatalf("models=%d want 1", len(s.Models))
	}
	m := s.Models[0]
	if m.Model != "glm-5.2" || m.Requests != 1 || m.Success != 1 || m.Failed != 0 || m.Streaming != 1 {
		t.Errorf("stat mismatch: %+v", m)
	}
	if m.AvgTTFBMS <= 0 {
		t.Errorf("avg_ttfb_ms=%v want >0", m.AvgTTFBMS)
	}
	// sseOK 末帧 usage: prompt=1 completion=1。
	if m.PromptTokens != 1 || m.CompletionTokens != 1 || m.TotalTokens != 2 {
		t.Errorf("tokens p=%d c=%d t=%d want 1/1/2", m.PromptTokens, m.CompletionTokens, m.TotalTokens)
	}
	if s.Total.Requests != 1 {
		t.Errorf("total requests=%d want 1", s.Total.Requests)
	}
}

func TestStatsNonStreamObservation(t *testing.T) {
	h, tr := statsHandler(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("chat code=%d", rec.Code)
	}
	m := tr.Snapshot().Models
	if len(m) != 1 {
		t.Fatalf("models=%d want 1", len(m))
	}
	if m[0].Streaming != 0 {
		t.Errorf("streaming=%d want 0 (sync request)", m[0].Streaming)
	}
	if m[0].TotalTokens != 2 {
		t.Errorf("total_tokens=%d want 2 (aggregate usage)", m[0].TotalTokens)
	}
}

func TestStatsCacheObservation(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":5,\"prompt_cache_hit_tokens\":80,\"prompt_cache_miss_tokens\":20,\"credit\":0.5}}\n\n" +
		"data: [DONE]\n\n"
	tr := metrics.New(filepath.Join(t.TempDir(), "metrics.json"))
	t.Cleanup(tr.Close)
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sse, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		Metrics:  tr,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[]}`)))
	m := tr.Snapshot().Models[0]
	if m.CacheHitTokens != 80 || m.CacheMissTokens != 20 || m.CacheHitRate != 0.8 {
		t.Errorf("cache hit=%d miss=%d rate=%v want 80/20/0.8", m.CacheHitTokens, m.CacheMissTokens, m.CacheHitRate)
	}
	if m.Credit != 0.5 {
		t.Errorf("credit=%v want 0.5", m.Credit)
	}
}

func TestStatsFailureObservation(t *testing.T) {
	tr := metrics.New(filepath.Join(t.TempDir(), "metrics.json"))
	t.Cleanup(tr.Close)
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 402, `{"code":1,"msg":"余额不足"}`, false })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		Metrics:  tr,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d want 503", rec.Code)
	}
	s := tr.Snapshot()
	if len(s.Models) != 1 {
		t.Fatalf("models=%d want 1（失败请求也要计入统计）", len(s.Models))
	}
	m := s.Models[0]
	if m.Requests != 1 || m.Success != 0 || m.Failed != 1 {
		t.Errorf("req=%d ok=%d fail=%d want 1/0/1（轮转整体只记 1 次）", m.Requests, m.Success, m.Failed)
	}
}

func TestStatsResetEndpoint(t *testing.T) {
	h, tr := statsHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if tr.Snapshot().Total.Requests != 1 {
		t.Fatal("precondition: 1 request recorded")
	}
	first := tr.Snapshot().Since
	time.Sleep(2 * time.Millisecond)

	code, body := getJSON(t, h, "POST", "/v1/stats/reset")
	if code != 200 {
		t.Fatalf("reset code=%d", code)
	}
	if ok, _ := body["ok"].(bool); !ok {
		t.Errorf("reset body=%v want ok=true", body)
	}
	s := tr.Snapshot()
	if s.Total.Requests != 0 || len(s.Models) != 0 {
		t.Errorf("after reset: req=%d models=%d want 0/0", s.Total.Requests, len(s.Models))
	}
	if !s.Since.After(first) {
		t.Errorf("since not bumped by reset: %v", s.Since)
	}
}

func TestStatsEndpointsRequireAuth(t *testing.T) {
	tr := metrics.New(filepath.Join(t.TempDir(), "metrics.json"))
	t.Cleanup(tr.Close)
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
		APIKey:   "secret",
		Metrics:  tr,
	})
	for _, tc := range []struct{ method, path string }{
		{"GET", "/v1/stats"},
		{"POST", "/v1/stats/reset"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}")))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: code=%d want 401", tc.method, tc.path, rec.Code)
		}
		// 正确 key 应 200。
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer secret")
		rec2 := httptest.NewRecorder()
		h.ServeHTTP(rec2, req)
		if rec2.Code != http.StatusOK {
			t.Errorf("%s %s with key: code=%d want 200", tc.method, tc.path, rec2.Code)
		}
	}
}
