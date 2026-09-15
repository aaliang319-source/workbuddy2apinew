// handler_responses_test.go Responses API 兼容层：请求转换、流式再编码、端点 e2e。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// parseSSEEvents 把 Responses SSE 文本拆成 event/payload 对列表。
func parseSSEEvents(t *testing.T, body string) []struct {
	Event   string
	Payload map[string]any
} {
	t.Helper()
	var out []struct {
		Event   string
		Payload map[string]any
	}
	for _, block := range strings.Split(body, "\n\n") {
		var ev string
		var data string
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				ev = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if ev == "" && data == "" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("event %q payload not json: %v\n%s", ev, err, data)
		}
		out = append(out, struct {
			Event   string
			Payload map[string]any
		}{ev, payload})
	}
	return out
}

// ─── 请求转换 ──────────────────────────────────────────────────────────

func TestResponsesToChatStringInput(t *testing.T) {
	out, err := responsesToChat([]byte(`{"model":"cn:glm-5.3","input":"你好","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(out, &chat) != nil {
		t.Fatal("not chat json")
	}
	if chat.Model != "cn:glm-5.3" || !chat.Stream {
		t.Errorf("model=%q stream=%v", chat.Model, chat.Stream)
	}
	if len(chat.Messages) != 1 || chat.Messages[0].Role != "user" || chat.Messages[0].Content != "你好" {
		t.Errorf("messages=%+v", chat.Messages)
	}
}

func TestResponsesToChatInstructionsAndTools(t *testing.T) {
	body := `{
		"model":"cn:glm-5.3",
		"instructions":"你是编码助手",
		"max_output_tokens":1024,
		"input":[
			{"type":"message","role":"user","content":"列出文件"},
			{"type":"reasoning","summary":[]},
			{"type":"function_call","call_id":"call_a","name":"shell","arguments":"{\"cmd\":\"ls\"}"},
			{"type":"function_call_output","call_id":"call_a","output":"a.txt"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}
		],
		"tools":[
			{"type":"function","name":"shell","description":"run cmd","parameters":{"type":"object"}},
			{"type":"web_search"}
		],
		"tool_choice":"auto"
	}`
	out, err := responsesToChat([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		Messages []struct {
			Role       string `json:"role"`
			Content    string `json:"content"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name       string         `json:"name"`
				Parameters map[string]any `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
		ToolChoice      string `json:"tool_choice"`
		MaxTokens       int    `json:"max_tokens"`
		MaxOutputTokens int    `json:"max_output_tokens"`
	}
	if json.Unmarshal(out, &chat) != nil {
		t.Fatalf("not chat json: %s", out)
	}
	// instructions→system + user + assistant(tool_calls) + tool + assistant(done)
	if len(chat.Messages) != 5 {
		t.Fatalf("messages=%d want 5: %+v", len(chat.Messages), chat.Messages)
	}
	if chat.Messages[0].Role != "system" || chat.Messages[0].Content != "你是编码助手" {
		t.Errorf("system msg: %+v", chat.Messages[0])
	}
	tcMsg := chat.Messages[2]
	if tcMsg.Role != "assistant" || len(tcMsg.ToolCalls) != 1 || tcMsg.ToolCalls[0].ID != "call_a" || tcMsg.ToolCalls[0].Function.Name != "shell" {
		t.Errorf("tool_calls msg: %+v", tcMsg)
	}
	toolMsg := chat.Messages[3]
	if toolMsg.Role != "tool" || toolMsg.ToolCallID != "call_a" || toolMsg.Content != "a.txt" {
		t.Errorf("tool msg: %+v", toolMsg)
	}
	// tools：function 保留并嵌套，web_search 丢弃
	if len(chat.Tools) != 1 || chat.Tools[0].Function.Name != "shell" {
		t.Errorf("tools=%+v", chat.Tools)
	}
	if chat.ToolChoice != "auto" {
		t.Errorf("tool_choice=%v", chat.ToolChoice)
	}
	if chat.MaxTokens != 1024 || chat.MaxOutputTokens != 0 {
		t.Errorf("max_tokens=%d max_output_tokens=%d（应换键）", chat.MaxTokens, chat.MaxOutputTokens)
	}
}

func TestResponsesToChatPreviousResponseIDRejected(t *testing.T) {
	_, err := responsesToChat([]byte(`{"model":"m","input":"hi","previous_response_id":"resp_x"}`))
	if err == nil || !strings.Contains(err.Error(), "previous_response_id") {
		t.Fatalf("err=%v want previous_response_id rejection", err)
	}
}

// ─── 端点 e2e ──────────────────────────────────────────────────────────

func TestResponsesEndpointStream(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"input":"你好"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type=%q", ct)
	}
	events := parseSSEEvents(t, rec.Body.String())
	if len(events) < 3 {
		t.Fatalf("events=%d too few:\n%s", len(events), rec.Body.String())
	}
	if events[0].Event != "response.created" {
		t.Errorf("first event=%q want response.created", events[0].Event)
	}
	var lastText string
	hasDelta := false
	for _, e := range events {
		if e.Event == "response.output_text.delta" {
			hasDelta = true
			if d, ok := e.Payload["delta"].(string); ok {
				lastText += d
			}
		}
	}
	if !hasDelta || lastText == "" {
		t.Errorf("no output_text.delta events (lastText=%q)", lastText)
	}
	completed := events[len(events)-1]
	if completed.Event != "response.completed" {
		t.Fatalf("last event=%q want response.completed", completed.Event)
	}
	respObj, _ := completed.Payload["response"].(map[string]any)
	if respObj == nil || respObj["status"] != "completed" {
		t.Errorf("completed response=%v", completed.Payload)
	}
	if usage, ok := respObj["usage"].(map[string]any); ok {
		if usage["input_tokens"].(float64) != 1 || usage["output_tokens"].(float64) != 1 {
			t.Errorf("usage=%v want 1/1", usage)
		}
	} else {
		t.Errorf("usage missing in completed: %v", completed.Payload)
	}
	// output 应含 message item 且文本与 delta 拼接一致
	output, _ := respObj["output"].([]any)
	if len(output) == 0 {
		t.Fatalf("output empty")
	}
	msg, _ := output[0].(map[string]any)
	if msg["type"] != "message" {
		t.Errorf("output[0]=%v want message", msg)
	}
}

func TestResponsesEndpointNonStream(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","input":"你好"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Object string `json:"object"`
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &resp) != nil {
		t.Fatalf("not json: %s", rec.Body.String())
	}
	if resp.Object != "response" || resp.Status != "completed" {
		t.Errorf("object=%q status=%q", resp.Object, resp.Status)
	}
	if len(resp.Output) == 0 || resp.Output[0].Type != "message" || resp.Output[0].Content[0].Text == "" {
		t.Errorf("output=%+v", resp.Output)
	}
	if resp.Usage.InputTokens != 1 || resp.Usage.OutputTokens != 1 {
		t.Errorf("usage=%+v want 1/1", resp.Usage)
	}
}

func TestResponsesEndpointFunctionCallStream(t *testing.T) {
	// 用 json.Marshal 构造帧，避免多层转义出错（手工转义曾让第二帧非法被跳过）。
	mustFrame := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return "data: " + string(b) + "\n\n"
	}
	f1 := mustFrame(map[string]any{
		"id": "chatcmpl-t", "model": "glm-5.2",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
			"tool_calls": []any{map[string]any{"index": 0, "id": "call_1",
				"function": map[string]any{"name": "shell", "arguments": `{"cmd":`}}},
		}}},
	})
	f2 := mustFrame(map[string]any{
		"id": "chatcmpl-t",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
			"tool_calls": []any{map[string]any{"index": 0,
				"function": map[string]any{"arguments": `"ls"}`}}},
			"finish_reason": "tool_calls",
		}}},
		"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	})
	sse := f1 + f2 + "data: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sse, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"input":"ls","tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	events := parseSSEEvents(t, rec.Body.String())
	completed := events[len(events)-1]
	if completed.Event != "response.completed" {
		t.Fatalf("last event=%q", completed.Event)
	}
	respObj := completed.Payload["response"].(map[string]any)
	output := respObj["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output=%v want exactly 1 function_call", output)
	}
	fc, _ := output[0].(map[string]any)
	if fc["type"] != "function_call" || fc["name"] != "shell" || fc["call_id"] != "call_1" {
		t.Errorf("function_call item=%v", fc)
	}
	if args, _ := fc["arguments"].(string); args != `{"cmd":"ls"}` {
		t.Errorf("arguments=%q", args)
	}
	if u := respObj["usage"].(map[string]any); u["input_tokens"].(float64) != 10 || u["output_tokens"].(float64) != 5 {
		t.Errorf("usage=%v", u)
	}
}

func TestResponsesEndpointErrorPassthrough(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 402, `{"code":1,"msg":"余额不足"}`, false })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","input":"hi"}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d want 503（上游错误语义透传）", rec.Code)
	}
	var body map[string]any
	if json.Unmarshal(rec.Body.Bytes(), &body) != nil || body["error"] == nil {
		t.Errorf("body=%s want OpenAI error envelope", rec.Body.String())
	}
}

func TestResponsesEndpointAuth(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
		APIKey:   "secret",
	})
	for _, path := range []string{"/v1/responses", "/responses"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(`{"model":"m","input":"hi"}`)))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: code=%d want 401", path, rec.Code)
		}
		req := httptest.NewRequest("POST", path, strings.NewReader(`{"model":"m","input":"hi"}`))
		req.Header.Set("Authorization", "Bearer secret")
		rec2 := httptest.NewRecorder()
		h.ServeHTTP(rec2, req)
		if rec2.Code != http.StatusOK {
			t.Errorf("%s with key: code=%d want 200", path, rec2.Code)
		}
	}
}

func TestResponsesEndpointPreviousResponseID400(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"m","input":"hi","previous_response_id":"resp_x"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "previous_response_id") {
		t.Errorf("body=%s want guidance", rec.Body.String())
	}
}
