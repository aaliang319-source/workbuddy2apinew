// handler_messages_test.go /v1/messages（Anthropic 兼容端点）测试：
// 请求翻译 / 响应映射 / SSE 事件序列 / 工具调用往返 / x-api-key 鉴权 / 模型映射 /
// 轮转与池记账 / count_tokens / max_tokens 强制校验。
// 沿用 handler_test.go 的 newFakeUpstream / testPoolWith 模式（stdlib testing，无第三方断言库）。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// toolSSE 带工具调用的上游 SSE fixture：首片 tool_calls（id+name+半截 arguments），
// 次片补完 arguments，末帧 finish_reason=tool_calls + usage。
const toolSSE = "data: {\"id\":\"chatcmpl-9\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\"\"}}]}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-9\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\":\\\"北京\\\"}\"}}]}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-9\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\n" +
	"data: [DONE]\n\n"

// sseCacheUsage 带缓存观测的上游 SSE：末帧 usage 含 prompt_cache_* 三元组
// （prompt=1000 = 命中 700 + 普通输入 200 + 写入 100，miss=300=200+100）。
const sseCacheUsage = "data: {\"id\":\"chatcmpl-10\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"好\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-10\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":50,\"total_tokens\":1050,\"prompt_cache_hit_tokens\":700,\"prompt_cache_miss_tokens\":300,\"prompt_cache_write_tokens\":100}}\n\n" +
	"data: [DONE]\n\n"

// sseDataAfter 提取 event 行之后的第一条 data JSON（Anthropic SSE 断言辅助）。
func sseDataAfter(t *testing.T, body, event string) string {
	t.Helper()
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == event && i+1 < len(lines) {
			return strings.TrimPrefix(strings.TrimSpace(lines[i+1]), "data: ")
		}
	}
	t.Fatalf("event %q not found; body=\n%s", event, body)
	return ""
}

// newMessagesHandler 构建带 Anthropic 模型映射的测试 handler（默认不鉴权）。
func newMessagesHandler(t *testing.T, up *upstream.Client, auths ...*auth.Auth) *Handler {
	t.Helper()
	return NewHandler(Config{
		Pool:                  testPoolWith(auths...),
		Upstream:              up,
		MaxBodyBytes:          1 << 20,
		AnthropicDefaultModel: "cn:auto",
		AnthropicModelMap:     map[string]string{"claude-sonnet-4-5": "cn:glm-5.3"},
	})
}

// postMessages 构造 /v1/messages 请求并执行（返回 recorder 供断言）。
func postMessages(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// newCapturingUpstream 在 newFakeUpstream 语义上加出站 body 捕获（tool/模型映射断言用）。
func newCapturingUpstream(t *testing.T, captured *[]byte, behavior func(authz string) (int, string, bool)) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if raw, err := io.ReadAll(r.Body); err == nil {
				*captured = raw
			}
			status, body, isStream := behavior(r.Header.Get("Authorization"))
			ct := "application/json"
			if isStream {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// TestMessagesStreamUsageCarriesContext 流式：message_delta 必须携带上下文口径字段
// ——input_tokens/cache_read/cache_creation/output_tokens（Claude Code 客户端在
// message_delta 分支对这三字段做 !=null 覆盖合并，是面板"上下文占用"的唯一数据源；
// message_start 时上游 usage 末帧未到，0 占位是官方行为）。cache 按 Anthropic
// 字段名映射：cache_read_input_tokens / cache_creation_input_tokens。
func TestMessagesStreamUsageCarriesContext(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseCacheUsage, true })
	h := newMessagesHandler(t, up, &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	rec := postMessages(t, h, `{"model":"claude-sonnet-4-5","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"你好"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	var delta struct {
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(sseDataAfter(t, body, "event: message_delta")), &delta); err != nil {
		t.Fatalf("message_delta unmarshal: %v", err)
	}
	u := delta.Usage
	// input_tokens = 未命中输入（miss - write）：缓存部分走 cache_read，写入部分走 cache_creation。
	if u.InputTokens != 200 || u.OutputTokens != 50 || u.CacheReadInputTokens != 700 || u.CacheCreationInputTokens != 100 {
		t.Errorf("message_delta usage=%+v want in=200 out=50 read=700 creation=100", u)
	}
}

// TestMessagesNonStreamUsageCarriesContext 非流式：usage 映射 Anthropic 全字段，
// input_tokens = 普通输入，缓存命中/写入分列（口径与流式一致）。
func TestMessagesNonStreamUsageCarriesContext(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseCacheUsage, true })
	h := newMessagesHandler(t, up, &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	rec := postMessages(t, h, `{"model":"claude-sonnet-4-5","max_tokens":100,"messages":[{"role":"user","content":"你好"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	if resp.Usage.InputTokens != 200 || resp.Usage.OutputTokens != 50 ||
		resp.Usage.CacheReadInputTokens != 700 || resp.Usage.CacheCreationInputTokens != 100 {
		t.Errorf("usage=%+v want in=200 out=50 read=700 creation=100", resp.Usage)
	}
}

// TestMessagesNonStream 非流式：OpenAI 聚合结果正确映射为 Anthropic Message 对象。
func TestMessagesNonStream(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := newMessagesHandler(t, up, &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	rec := postMessages(t, h, `{"model":"claude-sonnet-4-5","max_tokens":100,"messages":[{"role":"user","content":"你好"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Role       string `json:"role"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	if resp.Type != "message" || resp.Role != "assistant" {
		t.Errorf("type=%q role=%q want message/assistant", resp.Type, resp.Role)
	}
	if !strings.HasPrefix(resp.ID, "msg_") {
		t.Errorf("id=%q want msg_ prefix", resp.ID)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != "text" || resp.Content[0].Text != "你好" {
		t.Errorf("content=%+v want single text block 你好", resp.Content)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("stop_reason=%q want end_turn", resp.StopReason)
	}
	if resp.Usage.InputTokens != 1 || resp.Usage.OutputTokens != 1 {
		t.Errorf("usage=%+v want in=1 out=1", resp.Usage)
	}
}

// TestMessagesStreamEventSequence 流式：Anthropic SSE 事件按序发射、成对关闭、无 [DONE]。
func TestMessagesStreamEventSequence(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := newMessagesHandler(t, up, &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	rec := postMessages(t, h, `{"model":"claude-sonnet-4-5","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"你好"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type=%q want text/event-stream", ct)
	}
	body := rec.Body.String()
	wantOrder := []string{
		"event: message_start",
		"event: ping",
		"event: content_block_start",
		// 同一行 data 内 text 位于 text_delta 之前（{"delta":{"text":"你好","type":"text_delta"}），
		// 顺序断言需按字节出现次序排列。
		"你好",
		"\"type\":\"text_delta\"",
		"event: content_block_stop",
		"event: message_delta",
		"\"stop_reason\":\"end_turn\"",
		"event: message_stop",
	}
	pos := -1
	for _, w := range wantOrder {
		idx := strings.Index(body[pos+1:], w)
		if idx < 0 {
			t.Fatalf("event %q not found in order; body=\n%s", w, body)
		}
		pos += idx + 1
	}
	if strings.Contains(body, "data: [DONE]") {
		t.Errorf("anthropic stream must not emit OpenAI [DONE]; body=\n%s", body)
	}
	// text 块 index=0：start 与 stop 的 index 一致且均为 0。
	if strings.Count(body, "\"index\":0") < 3 {
		t.Errorf("expected index:0 on start/delta/stop; body=\n%s", body)
	}
}

// TestMessagesToolRoundTrip 工具调用往返：请求 tools 翻译为 OpenAI functions、
// tool_choice any → required；响应 tool_calls → tool_use 块（input 反序列化）。
func TestMessagesToolRoundTrip(t *testing.T) {
	var captured []byte
	up := newCapturingUpstream(t, &captured, func(string) (int, string, bool) { return 200, toolSSE, true })
	h := newMessagesHandler(t, up, &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	rec := postMessages(t, h, `{
		"model":"claude-sonnet-4-5","max_tokens":100,
		"system":"你是天气助手",
		"tools":[{"name":"get_weather","description":"查天气","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],
		"tool_choice":{"type":"any"},
		"messages":[{"role":"user","content":"北京天气如何"}]
	}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	// 出站 body 断言：OpenAI functions 形态 + system 消息 + tool_choice required + 裸模型名。
	var outbound struct {
		Model      string `json:"model"`
		ToolChoice any    `json:"tool_choice"`
		Messages   []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(captured, &outbound); err != nil {
		t.Fatalf("outbound unmarshal: %v raw=%s", err, captured)
	}
	if outbound.Model != "glm-5.3" {
		t.Errorf("outbound model=%q want glm-5.3（前缀已剥）", outbound.Model)
	}
	if len(outbound.Messages) == 0 || outbound.Messages[0].Role != "system" || outbound.Messages[0].Content != "你是天气助手" {
		t.Errorf("system message missing: %+v", outbound.Messages)
	}
	if len(outbound.Tools) != 1 || outbound.Tools[0].Type != "function" || outbound.Tools[0].Function.Name != "get_weather" {
		t.Errorf("tools=%+v want single function get_weather", outbound.Tools)
	}
	tc, _ := outbound.ToolChoice.(string)
	if tc != "required" {
		t.Errorf("tool_choice=%v want \"required\"（any 映射）", outbound.ToolChoice)
	}
	// 响应断言：tool_use 块 + 完整 input + stop_reason=tool_use。
	var resp struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp unmarshal: %v body=%s", err, rec.Body.String())
	}
	if resp.StopReason != "tool_use" {
		t.Errorf("stop_reason=%q want tool_use", resp.StopReason)
	}
	var toolBlock *struct {
		Type  string          `json:"type"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	for i := range resp.Content {
		if resp.Content[i].Type == "tool_use" {
			toolBlock = &resp.Content[i]
		}
	}
	if toolBlock == nil {
		t.Fatalf("no tool_use block; body=%s", rec.Body.String())
	}
	if toolBlock.ID != "call_1" || toolBlock.Name != "get_weather" {
		t.Errorf("tool block=%+v want call_1/get_weather", toolBlock)
	}
	var input map[string]any
	if err := json.Unmarshal(toolBlock.Input, &input); err != nil {
		t.Fatalf("input unmarshal: %v raw=%s", err, toolBlock.Input)
	}
	if input["city"] != "北京" {
		t.Errorf("input=%v want city=北京（分片 arguments 已拼全）", input)
	}
}

// TestMessagesToolResultTranslation 单元：user 消息里的 tool_result 块翻译为 role=tool 消息。
func TestMessagesToolResultTranslation(t *testing.T) {
	req := &anthropicRequest{
		MaxTokens: 10,
		Messages: []anthropicMessage{
			{Role: "user", Content: json.RawMessage(`"北京天气如何"`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"call_1","name":"get_weather","input":{"city":"北京"}}]`)},
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"call_1","content":"晴，25 度"}]`)},
		},
	}
	msgs, err := translateMessagesToOpenAI(req.Messages)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	raw, _ := json.Marshal(msgs)
	var parsed []struct {
		Role       string `json:"role"`
		ToolCallID string `json:"tool_call_id"`
		Content    string `json:"content"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("re-unmarshal: %v raw=%s", err, raw)
	}
	if len(parsed) != 3 {
		t.Fatalf("got %d messages want 3: %s", len(parsed), raw)
	}
	toolMsg := parsed[2]
	if toolMsg.Role != "tool" || toolMsg.ToolCallID != "call_1" || toolMsg.Content != "晴，25 度" {
		t.Errorf("tool message=%+v want role=tool/call_1/晴，25 度", toolMsg)
	}
}

// TestMessagesAuthXApiKey x-api-key 双头鉴权：Anthropic 形态命中、Bearer 回归、错误/缺失 401。
func TestMessagesAuthXApiKey(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up, MaxBodyBytes: 1 << 20, APIKey: "secret",
		AnthropicDefaultModel: "cn:auto",
	})
	cases := []struct {
		name   string
		header string
		key    string
		want   int
	}{
		{"x-api-key ok", "X-Api-Key", "secret", 200},
		{"x-api-key wrong", "X-Api-Key", "nope", 401},
		{"x-api-key missing", "", "", 401},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages",
			strings.NewReader(`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`))
		if tc.key != "" {
			req.Header.Set(tc.header, tc.key)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s: status=%d want %d", tc.name, rec.Code, tc.want)
		}
	}
	// Bearer 回归：OpenAI 形态仍通。
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("bearer regression: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestMessagesModelMapping 模型映射：model_map 命中 / cn: 前缀直通 / 未知走 default_model。
func TestMessagesModelMapping(t *testing.T) {
	var captured []byte
	up := newCapturingUpstream(t, &captured, func(string) (int, string, bool) { return 200, sseOK, true })
	h := newMessagesHandler(t, up, &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	cases := []struct{ reqModel, wantOutbound string }{
		{"claude-sonnet-4-5", "glm-5.3"}, // model_map 命中 → cn:glm-5.3 → 出站剥前缀
		{"cn:glm-5.3", "glm-5.3"},        // 前缀直通
		{"glm-5.2", "glm-5.2"},           // 已知裸名 → cn:glm-5.2
		{"claude-opus-99", "auto"},       // 未知 → default cn:auto
	}
	for _, tc := range cases {
		captured = nil
		rec := postMessages(t, h, `{"model":"`+tc.reqModel+`","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
		if rec.Code != 200 {
			t.Fatalf("%s: status=%d body=%s", tc.reqModel, rec.Code, rec.Body.String())
		}
		var outbound struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(captured, &outbound); err != nil {
			t.Fatalf("%s: outbound unmarshal: %v raw=%s", tc.reqModel, err, captured)
		}
		if outbound.Model != tc.wantOutbound {
			t.Errorf("%s: outbound model=%q want %q", tc.reqModel, outbound.Model, tc.wantOutbound)
		}
	}
}

// TestMessagesRotatesOn402 轮转 + 池记账：坏号 402 余额不足 → 冷却换好号成功。
func TestMessagesRotatesOn402(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 402, `{"code":1,"msg":"余额不足"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, MaxBodyBytes: 1 << 20, AnthropicDefaultModel: "cn:auto"})
	rec := postMessages(t, h, `{"model":"claude-sonnet-4-5","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls["Bearer at-bad"] == 0 || calls["Bearer at-good"] == 0 {
		t.Errorf("rotation incomplete: calls=%v want both attempted", calls)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.Reason == "" {
		t.Errorf("bad account should be hard-cooled after 402 余额不足: %+v", st)
	}
}

// TestMessagesCountTokens count_tokens：启发式正整数输出。
func TestMessagesCountTokens(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := newMessagesHandler(t, up, &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"claude-sonnet-4-5","max_tokens":10,"system":"sys","messages":[{"role":"user","content":"hello world this is a token estimate test"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	if resp.InputTokens <= 0 {
		t.Errorf("input_tokens=%d want > 0", resp.InputTokens)
	}
}

// TestMessagesMaxTokensRequired max_tokens 是 Anthropic 强制字段，缺失 → 400。
func TestMessagesMaxTokensRequired(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := newMessagesHandler(t, up, &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	rec := postMessages(t, h, `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 400 {
		t.Fatalf("status=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	if envelope.Error.Type != "invalid_request_error" {
		t.Errorf("error.type=%q want invalid_request_error", envelope.Error.Type)
	}
}

// TestMessagesSystemRoleInArray 回归：messages 数组内出现 system/developer 角色时
// 不应 400，而应按同义角色透传给上游（部分客户端/代理把系统提示塞进 messages）。
func TestMessagesSystemRoleInArray(t *testing.T) {
	var captured []byte
	// 网关始终以上游流式模式请求并聚合，故 fixture 用 SSE（同 TestMessagesNonStream）。
	up := newCapturingUpstream(t, &captured, func(string) (int, string, bool) { return 200, sseOK, true })
	h := newMessagesHandler(t, up, &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})

	body := `{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[
		{"role":"system","content":"you are terse"},
		{"role":"user","content":"hi"},
		{"role":"developer","content":[{"type":"text","text":"be brief"}]},
		{"role":"user","content":"again"}
	]}`
	rec := postMessages(t, h, body)
	if rec.Code != 200 {
		t.Fatalf("status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(captured, &out); err != nil {
		t.Fatalf("parse upstream body: %v raw=%s", err, captured)
	}
	if len(out.Messages) != 4 {
		t.Fatalf("want 4 upstream messages, got %d: %s", len(out.Messages), captured)
	}
	if out.Messages[0].Role != "system" || out.Messages[0].Content != "you are terse" {
		t.Fatalf("system msg mismatch: %+v", out.Messages[0])
	}
	// developer 由上游出站层归一化为 system（upstream/payload.go），内容必须保留。
	if (out.Messages[2].Role != "developer" && out.Messages[2].Role != "system") || out.Messages[2].Content != "be brief" {
		t.Fatalf("developer msg mismatch: %+v", out.Messages[2])
	}
}

// TestMessagesUnknownRoleStill400 未知角色仍应 400（放行仅限 system/developer）。
func TestMessagesUnknownRoleStill400(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := newMessagesHandler(t, up, &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	rec := postMessages(t, h, `{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"tool","content":"x"}]}`)
	if rec.Code != 400 {
		t.Fatalf("status=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
}
