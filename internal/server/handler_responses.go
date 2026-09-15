// handler_responses.go OpenAI Responses API 兼容层（/v1/responses、/responses）：
// Codex CLI 默认走 Responses 协议（POST {base}/responses，wire_api="responses"），
// 网关上游只有 chat completions。本层做双向转换：
//
//	请求：Responses {instructions, input[], tools[]} → chat {messages[], tools[], max_tokens}
//	非流式：chat JSON → Response 对象（output: message / function_call items）
//	流式：chat SSE → Responses SSE 事件（response.created / output_text.delta /
//	      output_text.done / output_item.done / response.completed，usage 回传）
//
// 实现方式：请求侧纯 JSON 转换后**委托 h.chatCompletions**（复用账号轮转、粘性、
// 降级、熔断、/v1/stats 观测全链路）；响应侧用一个拦截 ResponseWriter 在字节层
// 再编码——chat 的错误 JSON（4xx/5xx）原样透传给 Codex 展示。
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// responsesReqBody 单请求体上限：Codex 携带完整对话历史，给到与 chat 相同量级。
const responsesMaxBody = 8 << 20

// ─── 请求侧：Responses → chat completions ─────────────────────────────

// respContentPart input/output 消息的内容分片（text 是唯一关心的载荷）。
type respContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// respInputItem input 数组的条目：消息 / 函数调用 / 函数结果 / 推理（忽略）。
type respInputItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string | []respContentPart
	CallID  string          `json:"call_id"`
	Name    string          `json:"name"`
	Args    string          `json:"arguments"`
	Output  json.RawMessage `json:"output"`
}

// respToolItem Responses tools[] 条目；只有 type=function 能映射到 chat。
type respToolItem struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      json.RawMessage `json:"strict"`
}

type responsesRequest struct {
	Model              string            `json:"model"`
	Input              json.RawMessage   `json:"input"` // string | []respInputItem
	Instructions       string            `json:"instructions"`
	Stream             bool              `json:"stream"`
	MaxOutputTokens    int               `json:"max_output_tokens"`
	Temperature        *float64          `json:"temperature"`
	TopP               *float64          `json:"top_p"`
	Tools              []json.RawMessage `json:"tools"`
	ToolChoice         json.RawMessage   `json:"tool_choice"`
	ParallelToolCalls  *bool             `json:"parallel_tool_calls"`
	PreviousResponseID string            `json:"previous_response_id"`
}

// chatMessage 构造用中间结构（与 chat completions 的 message 字段对齐）。
type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"` // role=tool 时对应 function_call 的 call_id
	ToolCalls  []chatToolCall  `json:"tool_calls,omitempty"`
}

type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // 恒 "function"
	Function chatToolCallFunc `json:"function"`
}

type chatToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// respItemText 解析 content 字段（string 或分片数组）为纯文本。
func respItemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []respContentPart
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}

// responsesToChat 把 Responses 请求体转换为 chat completions 请求体。
// 返回错误时消息可直接透给 400 invalid_request_error。
func responsesToChat(body []byte) ([]byte, error) {
	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid Responses request body: %v", err)
	}
	if req.PreviousResponseID != "" {
		// 网关无状态不存 response；Codex 在 disable_response_storage（默认开）下
		// 每轮带全量历史，不会用该字段。显式给出修复指引而不是静默丢上下文。
		return nil, fmt.Errorf("previous_response_id is not supported (gateway is stateless): enable store=false / disable_response_storage so each request carries the full conversation")
	}

	msgs := []chatMessage{}
	if req.Instructions != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: mustJSON(req.Instructions)})
	}

	// openToolGroup 把连续的 function_call 条目归并进同一条 assistant tool_calls
	// 消息（chat 协议要求 tool 消息必须紧跟携带 tool_calls 的 assistant 消息）。
	appendToolCall := func(call chatToolCall) {
		last := len(msgs) - 1
		if last >= 0 && len(msgs[last].ToolCalls) > 0 {
			msgs[last].ToolCalls = append(msgs[last].ToolCalls, call)
			return
		}
		msgs = append(msgs, chatMessage{Role: "assistant", ToolCalls: []chatToolCall{call}})
	}

	// openCalls 未闭合的 function_call call_id（FIFO）；callSeq 兜底生成唯一 id。
	var openCalls []string
	callSeq := 0

	if len(req.Input) > 0 {
		var inputStr string
		if json.Unmarshal(req.Input, &inputStr) == nil {
			msgs = append(msgs, chatMessage{Role: "user", Content: mustJSON(inputStr)})
		} else {
			var items []respInputItem
			if err := json.Unmarshal(req.Input, &items); err != nil {
				return nil, fmt.Errorf("invalid input: %v", err)
			}
			for _, it := range items {
				switch it.Type {
				case "", "message":
					role := it.Role
					if role == "" {
						role = "user"
					}
					if role == "developer" {
						role = "system" // chat 侧统一按 system 处理
					}
					msgs = append(msgs, chatMessage{Role: role, Content: mustJSON(respItemText(it.Content))})
				case "function_call":
					callSeq++
					callID := it.CallID
					if callID == "" {
						callID = "call_" + strconv.Itoa(callSeq)
					}
					appendToolCall(chatToolCall{ID: callID, Type: "function", Function: chatToolCallFunc{Name: it.Name, Arguments: it.Args}})
					openCalls = append(openCalls, callID) // 入队：等对应 output 出队配对
				case "function_call_output":
					out := ""
					if len(it.Output) > 0 {
						var str string
						if json.Unmarshal(it.Output, &str) == nil {
							out = str
						} else {
							out = string(it.Output)
						}
					}
					// call_id 配对：优先用 output 自带的；缺失则按 FIFO 取最早未闭合的调用
					//（Codex 恒按调用顺序回传结果）。配对失败的 tool 消息上游会报错，
					// 但这只能由客户端历史顺序异常导致，此处不做静默重排。
					toolID := it.CallID
					if toolID == "" && len(openCalls) > 0 {
						toolID = openCalls[0]
						openCalls = openCalls[1:]
					} else if toolID != "" {
						for i, c := range openCalls {
							if c == toolID {
								openCalls = append(openCalls[:i], openCalls[i+1:]...)
								break
							}
						}
					}
					msgs = append(msgs, chatMessage{Role: "tool", ToolCallID: toolID, Content: mustJSON(out)})
				case "reasoning", "local_shell_call", "web_search_call", "item_reference", "mcp_call", "mcp_list_tools":
					// 上游不可回放的中间态/不支持的远端工具条目：跳过（历史不完整只影响
					// 模型对推理链的记忆，不影响对话与函数调用闭环）。
				default:
					// 未知类型保守跳过，不让单个条目打死整条请求。
				}
			}
		}
	}

	chat := map[string]any{
		"model":    req.Model,
		"messages": msgs,
		"stream":   req.Stream,
	}
	if req.MaxOutputTokens > 0 {
		chat["max_tokens"] = req.MaxOutputTokens
	}
	if req.Temperature != nil {
		chat["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		chat["top_p"] = *req.TopP
	}
	if req.ParallelToolCalls != nil {
		chat["parallel_tool_calls"] = *req.ParallelToolCalls
	}
	var chatTools []map[string]any
	for _, raw := range req.Tools {
		var t respToolItem
		if json.Unmarshal(raw, &t) != nil || t.Type != "function" || t.Name == "" {
			continue // web_search/custom 等远端工具上游不支持，丢弃
		}
		fn := map[string]any{"name": t.Name, "description": t.Description}
		if len(t.Parameters) > 0 && !bytes.Equal(t.Parameters, []byte("null")) {
			fn["parameters"] = json.RawMessage(t.Parameters)
		}
		if len(t.Strict) > 0 {
			fn["strict"] = json.RawMessage(t.Strict)
		}
		chatTools = append(chatTools, map[string]any{"type": "function", "function": fn})
	}
	if len(chatTools) > 0 {
		chat["tools"] = chatTools
	}
	if len(req.ToolChoice) > 0 && string(req.ToolChoice) != "null" {
		var tcName string
		var tcAny any
		if json.Unmarshal(req.ToolChoice, &tcName) == nil {
			chat["tool_choice"] = tcName // "auto" / "none" / "required"
		} else if json.Unmarshal(req.ToolChoice, &tcAny) == nil {
			// 对象形态 {type:"function", name:"x"} → chat 的 {type:"function", function:{name}}
			var obj struct {
				Type string `json:"type"`
				Name string `json:"name"`
			}
			if json.Unmarshal(req.ToolChoice, &obj) == nil && obj.Type == "function" && obj.Name != "" {
				chat["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": obj.Name}}
			}
		}
	}
	return json.Marshal(chat)
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return b
}

// ─── 响应侧：拦截 ResponseWriter 再编码 ────────────────────────────────

// chatChunk 流式解析用的 chat SSE 帧（只取关心的字段）。
type chatChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Created int64  `json:"created"`
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
	} `json:"usage"`
}

// sseToolAccum 流式聚合中的单个工具调用。
type sseToolAccum struct {
	ID   string
	Name string
	Args strings.Builder
}

// responsesWriter 拦截 chatCompletions 的输出：
//   - 检测到 chat SSE 帧 → 进入流式模式，把帧转成 Responses 事件实时写出；
//   - 收到 [DONE] → 补齐 done/completed 事件收尾；
//   - 全程未出现 SSE（非流式 JSON 或错误 JSON）→ finish() 时按状态透传
//     （200 的 chat 响应体转换成 Response 对象；错误体原样透传给 Codex 展示）。
type responsesWriter struct {
	w       http.ResponseWriter
	status  int    // 拦截到的 WriteHeader 状态（0 = 未显式调用）
	pending []byte // 未判定模式前的字节缓冲（上限 responsesMaxBody）
	stream  bool   // 已进入流式模式
	done    bool   // 已收到 [DONE]（completed 事件已发）

	seq     int64 // 事件序号
	respID  string
	model   string
	created int64

	textOpen  bool            // message item 已开播（added 事件已发）
	text      strings.Builder // 全量输出文本
	tools     map[int]*sseToolAccum
	toolOrder []int

	usageIn, usageOut, usageTotal int64
}

func newResponsesWriter(w http.ResponseWriter) *responsesWriter {
	return &responsesWriter{w: w, tools: map[int]*sseToolAccum{}}
}

func (rw *responsesWriter) Header() http.Header { return rw.w.Header() }

func (rw *responsesWriter) WriteHeader(status int) {
	if rw.stream {
		rw.w.WriteHeader(status) // 流式模式状态码直通
		return
	}
	rw.status = status // 未判定：先扣下，finish() 再发
}

func (rw *responsesWriter) Write(p []byte) (int, error) {
	if rw.stream {
		return rw.feedStream(p)
	}
	// 未判定模式：找 SSE 帧特征。逐行扫缓冲（保留半行到 pending）。
	rw.pending = append(rw.pending, p...)
	if i := bytes.Index(rw.pending, []byte("data: ")); i >= 0 {
		rw.stream = true
		rest := rw.pending[i:]
		rw.pending = nil
		_, err := rw.feedStream(rest)
		return len(p), err
	}
	if len(rw.pending) > responsesMaxBody {
		// 异常大且非 SSE：放透（防御性，正常 chat 响应不会走到这）。
		rw.flushPassthrough()
	}
	return len(p), nil
}

// flushPassthrough 防御分支：未判定模式下缓冲超过上限仍无 SSE 特征时，
// 把缓冲字节原样放透（正常 chat 响应不会走到这里——SSE 特征总会先出现）。
func (rw *responsesWriter) flushPassthrough() {
	if rw.stream || len(rw.pending) == 0 {
		return
	}
	status := rw.status
	if status == 0 {
		status = http.StatusOK
	}
	rw.w.WriteHeader(status)
	_, _ = rw.w.Write(rw.pending)
	rw.pending = nil
}

func (rw *responsesWriter) Flush() {
	if fl, ok := rw.w.(http.Flusher); ok && rw.stream {
		fl.Flush()
	}
}

// feedStream 处理流式模式字节：按行拆 SSE 帧，转换成 Responses 事件。
func (rw *responsesWriter) feedStream(p []byte) (int, error) {
	rw.pending = append(rw.pending, p...)
	for {
		i := bytes.IndexByte(rw.pending, '\n')
		if i < 0 {
			break
		}
		line := string(rw.pending[:i+1])
		rw.pending = rw.pending[i+1:]
		line = strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			rw.finishStream()
			return len(p), nil
		}
		var cc chatChunk
		if json.Unmarshal([]byte(payload), &cc) != nil {
			continue // 噪声帧跳过
		}
		if rw.respID == "" {
			rw.begin(cc)
		}
		for _, ch := range cc.Choices {
			if ch.Delta.Content != "" {
				rw.onTextDelta(ch.Delta.Content)
			}
			for _, tc := range ch.Delta.ToolCalls {
				rw.onToolDelta(tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments)
			}
		}
		if cc.Usage != nil {
			rw.usageIn = cc.Usage.PromptTokens
			rw.usageOut = cc.Usage.CompletionTokens
			rw.usageTotal = cc.Usage.TotalTokens
		}
	}
	return len(p), nil
}

// emit 写一条 Responses SSE 事件并 flush。
func (rw *responsesWriter) emit(event string, payload map[string]any) {
	rw.seq++
	payload["type"] = event
	payload["sequence_number"] = rw.seq
	b, _ := json.Marshal(payload)
	fmt.Fprintf(rw.w, "event: %s\ndata: %s\n\n", event, b)
	if fl, ok := rw.w.(http.Flusher); ok {
		fl.Flush()
	}
}

// begin 首帧：发送 response.created。
func (rw *responsesWriter) begin(cc chatChunk) {
	rw.respID = "resp_" + cc.ID
	if cc.ID == "" {
		rw.respID = "resp_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	rw.model = cc.Model
	rw.created = cc.Created
	if rw.created == 0 {
		rw.created = time.Now().Unix()
	}
	rw.emit("response.created", map[string]any{
		"response": rw.responseObj("in_progress", nil),
	})
}

// onTextDelta 文本增量：首个增量先补 item/part added，再发 delta。
func (rw *responsesWriter) onTextDelta(text string) {
	if !rw.textOpen {
		rw.textOpen = true
		rw.emit("response.output_item.added", map[string]any{
			"output_index": 0,
			"item":         map[string]any{"type": "message", "id": "msg_0", "role": "assistant", "status": "in_progress", "content": []any{}},
		})
		rw.emit("response.content_part.added", map[string]any{
			"item_id": "msg_0", "output_index": 0, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
	}
	rw.text.WriteString(text)
	rw.emit("response.output_text.delta", map[string]any{
		"item_id": "msg_0", "output_index": 0, "content_index": 0, "delta": text,
	})
}

// onToolDelta 工具调用增量：按 index 聚合 id/name/arguments。
func (rw *responsesWriter) onToolDelta(index int, id, name, args string) {
	t := rw.tools[index]
	if t == nil {
		t = &sseToolAccum{}
		rw.tools[index] = t
		rw.toolOrder = append(rw.toolOrder, index)
	}
	if id != "" {
		t.ID = id
	}
	if name != "" {
		t.Name = name
	}
	t.Args.WriteString(args)
}

// responseObj 组装 Response 对象（created / completed / 兜底共用）。
func (rw *responsesWriter) responseObj(status string, output []any) map[string]any {
	if output == nil {
		output = []any{}
	}
	obj := map[string]any{
		"id":                 rw.respID,
		"object":             "response",
		"created_at":         rw.created,
		"status":             status,
		"model":              rw.model,
		"output":             output,
		"error":              nil,
		"incomplete_details": nil,
	}
	if status == "completed" {
		obj["usage"] = map[string]any{
			"input_tokens":          rw.usageIn,
			"output_tokens":         rw.usageOut,
			"total_tokens":          rw.usageTotal,
			"input_tokens_details":  map[string]any{"cached_tokens": 0},
			"output_tokens_details": map[string]any{"reasoning_tokens": 0},
		}
	}
	return obj
}

// finishStream [DONE]（或上游流截断）后的收尾：done 事件 + completed。
func (rw *responsesWriter) finishStream() {
	if rw.done {
		return
	}
	rw.done = true
	output := []any{}
	if rw.textOpen {
		text := rw.text.String()
		rw.emit("response.output_text.done", map[string]any{
			"item_id": "msg_0", "output_index": 0, "content_index": 0, "text": text,
		})
		msgItem := map[string]any{
			"type": "message", "id": "msg_0", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
		}
		rw.emit("response.output_item.done", map[string]any{"output_index": 0, "item": msgItem})
		output = append(output, msgItem)
	}
	for i, idx := range rw.toolOrder {
		t := rw.tools[idx]
		callID := t.ID
		if callID == "" {
			callID = "call_" + strconv.Itoa(idx)
		}
		fc := map[string]any{
			"type": "function_call", "id": "fc_" + strconv.Itoa(i), "call_id": callID,
			"name": t.Name, "arguments": t.Args.String(), "status": "completed",
		}
		rw.emit("response.output_item.done", map[string]any{"output_index": i + 1, "item": fc})
		output = append(output, fc)
	}
	rw.emit("response.completed", map[string]any{"response": rw.responseObj("completed", output)})
}

// finish handler 返回后收尾：
//   - 流式：若上游没发 [DONE] 就断了（异常截断），仍补 completed 保证 Codex 能结束；
//   - 未判定（非流式）：200 的 chat JSON → Response 对象；错误体原样透传。
func (rw *responsesWriter) finish() {
	if rw.stream {
		if !rw.done {
			rw.finishStream()
		}
		return
	}
	body := rw.pending
	status := rw.status
	if status == 0 {
		status = http.StatusOK
	}
	// 错误或空体：原样透传（headers 已由 chat 侧 writeJSON 设置好）。
	if status != http.StatusOK || len(body) == 0 {
		rw.w.WriteHeader(status)
		_, _ = rw.w.Write(body)
		return
	}
	// 200：chat 响应 JSON → Response 对象；转换失败原样透传。
	var chat map[string]any
	if json.Unmarshal(body, &chat) != nil {
		rw.w.WriteHeader(status)
		_, _ = rw.w.Write(body)
		return
	}
	rw.w.WriteHeader(status)
	_, _ = rw.w.Write(mustJSON(chatRespToResponses(chat)))
}

// chatRespToResponses 非流式 chat 响应 → Responses 对象。
func chatRespToResponses(chat map[string]any) map[string]any {
	respID := "resp_" + strOr(chat["id"], "gw")
	model := strOr(chat["model"], "")
	created := intOr(chat["created"])

	output := []any{}
	idx := 0
	if choices, ok := chat["choices"].([]any); ok && len(choices) > 0 {
		if c0, ok := choices[0].(map[string]any); ok {
			msg, _ := c0["message"].(map[string]any)
			if msg != nil {
				if text := strOr(msg["content"]); text != "" {
					output = append(output, map[string]any{
						"type": "message", "id": fmt.Sprintf("msg_%d", idx), "role": "assistant",
						"status":  "completed",
						"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
					})
					idx++
				}
				if tcs, ok := msg["tool_calls"].([]any); ok {
					for _, tc := range tcs {
						tcMap, ok := tc.(map[string]any)
						if !ok {
							continue
						}
						fn, _ := tcMap["function"].(map[string]any)
						output = append(output, map[string]any{
							"type": "function_call", "id": fmt.Sprintf("fc_%d", idx),
							"call_id": strOr(tcMap["id"]), "name": strOr(fn["name"]),
							"arguments": strOr(fn["arguments"]), "status": "completed",
						})
						idx++
					}
				}
			}
		}
	}

	resp := map[string]any{
		"id": respID, "object": "response", "created_at": created, "status": "completed",
		"model": model, "output": output, "error": nil, "incomplete_details": nil,
		"parallel_tool_calls": true,
	}
	if u, ok := chat["usage"].(map[string]any); ok {
		in := intOr(u["prompt_tokens"])
		out := intOr(u["completion_tokens"])
		resp["usage"] = map[string]any{
			"input_tokens": in, "output_tokens": out, "total_tokens": intOr(u["total_tokens"]),
			"input_tokens_details":  map[string]any{"cached_tokens": 0},
			"output_tokens_details": map[string]any{"reasoning_tokens": 0},
		}
		_ = in
		_ = out
	}
	return resp
}

func strOr(v any, def ...string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	if len(def) > 0 {
		return def[0]
	}
	return ""
}

func intOr(v any) int64 {
	if f, ok := v.(float64); ok {
		return int64(f)
	}
	return 0
}

// ─── 端点 ──────────────────────────────────────────────────────────────

// responses POST /v1/responses（及 /responses 别名）：Codex CLI 的默认协议。
// 请求转换为 chat completions 后**委托 chatCompletions**——账号轮转、粘性会话、
// 降级重试、熔断与 /v1/stats 观测全部复用；响应由 responsesWriter 再编码。
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, responsesMaxBody+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "read request body: "+err.Error())
		return
	}
	if len(body) > responsesMaxBody {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_body_too_large", "request body exceeds limit")
		return
	}
	chatBody, cerr := responsesToChat(body)
	if cerr != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", cerr.Error())
		return
	}
	// 以转换后的 chat 请求体替换原请求，委托给 chat 主链路。
	r.Body = io.NopCloser(bytes.NewReader(chatBody))
	r.ContentLength = int64(len(chatBody))

	rw := newResponsesWriter(w)
	defer rw.finish()
	h.chatCompletions(rw, r)
}
