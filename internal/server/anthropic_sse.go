// anthropic_sse.go Anthropic 响应翻译：上游 OpenAI SSE → Anthropic 协议输出。
//
// Anthropic 流式事件序列（客户端强依赖 event: 行 + 严格的事件顺序）：
//
//	event: message_start     （消息骨架，content 为空数组）
//	event: ping              （防中间缓冲，发一次即可）
//	event: content_block_start / content_block_delta / content_block_stop
//	                         （块 index 单调递增，start→delta*→stop 严格成对）
//	event: message_delta     （stop_reason + usage.output_tokens）
//	event: message_stop      （流终结；缺它 Claude Code 会挂起重试）
//
// 每个事件写 "event: <type>\ndata: <json>\n\n" 并 flush；不写 OpenAI 的 [DONE]
// （Anthropic 协议无此帧）。上游增量词汇映射：delta.content→text_delta、
// delta.reasoning_content→thinking_delta、delta.tool_calls→input_json_delta
// （首片带 id/name，后续分片只拼 arguments 片段）。
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// anthropicStopReason OpenAI finish_reason → Anthropic stop_reason。
func anthropicStopReason(openAIFinish string) string {
	switch openAIFinish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	default:
		// stop / 其余一律 end_turn（Anthropic 的 stop_sequence 命中检测需逐字扫描，
		// 上游也不会为我们停在该语义，明示延期）。
		return "end_turn"
	}
}

// writeAnthropicError 按 Anthropic 错误信封输出 JSON（非 SSE 场景）。
// 503 固定 overloaded_error：Claude Code 对该类型会指数退避重试，恰合网关
// 「全部账号冷却中」的临时性语义；401/429/400 各对齐 Anthropic 官方类型。
func writeAnthropicError(w http.ResponseWriter, status int, msg string) {
	errType := "api_error"
	switch status {
	case http.StatusUnauthorized:
		errType = "authentication_error"
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		errType = "invalid_request_error"
	case http.StatusTooManyRequests:
		errType = "rate_limit_error"
	case http.StatusServiceUnavailable:
		errType = "overloaded_error"
	case http.StatusBadGateway:
		errType = "api_error"
	}
	writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errType, "message": msg},
	})
}

// anthropicMessageFromOpenAI 把 upstream.Aggregate 输出的 OpenAI chat.completion map
// 翻译为 Anthropic Message 对象（非流式路径）。映射：
// reasoning_content→thinking 块（signature 置空串，D3）、content→text 块、
// tool_calls[]→tool_use 块（arguments 反序列化为 input，失败回退 {}）、
// finish_reason→stop_reason、usage 字段名换算。全空回复兜底单个空 text 块（D4，
// 比 content:[] 兼容面广——部分客户端对空 content 数组直接崩）。
func anthropicMessageFromOpenAI(resp map[string]any, requestModel string) map[string]any {
	id, _ := resp["id"].(string)
	id = strings.TrimPrefix(id, "chatcmpl-")
	usage, _ := resp["usage"].(map[string]any)
	var inToks, outToks float64
	if usage != nil {
		inToks, _ = usage["prompt_tokens"].(float64)
		outToks, _ = usage["completion_tokens"].(float64)
	}
	stopReason := "end_turn"
	var blocks []map[string]any
	if chs, ok := resp["choices"].([]any); ok && len(chs) > 0 {
		c, _ := chs[0].(map[string]any)
		if c != nil {
			if fr, ok := c["finish_reason"].(string); ok {
				stopReason = anthropicStopReason(fr)
			}
			if msg, ok := c["message"].(map[string]any); ok {
				if rc, ok := msg["reasoning_content"].(string); ok && rc != "" {
					blocks = append(blocks, map[string]any{
						"type": "thinking", "thinking": rc, "signature": "",
					})
				}
				if txt, _ := msg["content"].(string); txt != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": txt})
				}
				// Aggregate 产出的 tool_calls 是 []map[string]any（非 []any），
				// 两种形态都兼容（直接断言 []any 会静默失败丢块）。
				var tcs []any
				switch v := msg["tool_calls"].(type) {
				case []any:
					tcs = v
				case []map[string]any:
					for _, m := range v {
						tcs = append(tcs, m)
					}
				}
				for _, tc := range tcs {
					call, ok := tc.(map[string]any)
					if !ok {
						continue
					}
					input := map[string]any{}
					if fn, ok := call["function"].(map[string]any); ok {
						if args, _ := fn["arguments"].(string); strings.TrimSpace(args) != "" {
							_ = json.Unmarshal([]byte(args), &input)
						}
						name, _ := fn["name"].(string)
						cid, _ := call["id"].(string)
						blocks = append(blocks, map[string]any{
							"type": "tool_use", "id": cid, "name": name, "input": input,
						})
					}
				}
			}
		}
	}
	if len(blocks) == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": ""})
	}
	return map[string]any{
		"id":            "msg_" + id,
		"type":          "message",
		"role":          "assistant",
		"model":         requestModel,
		"content":       blocks,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  int(inToks),
			"output_tokens": int(outToks),
		},
	}
}

// anthropicStreamState 流式状态机：跟踪当前打开的块与 tool_call 首片，保证
// start→delta→stop 成对、index 单调递增。
type anthropicStreamState struct {
	w        http.ResponseWriter
	fl       http.Flusher
	model    string
	msgID    string
	index    int            // 下一个块 index（单调递增）
	openIdx  int            // 当前未关闭块的 index
	openType string         // "" / "text" / "thinking" / "tool_use"
	toolSeen map[int]bool   // OpenAI tool_calls index → 已发 block_start
	toolArgs map[int]string // OpenAI tool_calls index → 已发射的 arguments 片段（防首片空串）
	outToks  int
	stopped  bool // 已发 message_stop（幂等收尾）
}

// writeEvent 写一个 SSE 事件（event: + data: 两行，Anthropic 客户端强依赖 event 行）。
func (s *anthropicStreamState) writeEvent(event string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, werr := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, raw); werr != nil {
		return werr
	}
	if s.fl != nil {
		s.fl.Flush()
	}
	return nil
}

// closeBlock 关闭当前打开的块（若有）。幂等：无打开块时空操作。
func (s *anthropicStreamState) closeBlock() error {
	if s.openType == "" {
		return nil
	}
	idx := s.openIdx
	s.openType = ""
	if err := s.writeEvent("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": idx,
	}); err != nil {
		return err
	}
	return nil
}

// finish 收尾：关块 → message_delta（stop_reason + output_tokens）→ message_stop。
// 幂等（stopped 标记）：调用方在正常流尾与空流兜底两条路径都会调。
func (s *anthropicStreamState) finish(stopReason string) error {
	if s.stopped {
		return nil
	}
	s.stopped = true
	if err := s.closeBlock(); err != nil {
		return err
	}
	if err := s.writeEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": s.outToks},
	}); err != nil {
		return err
	}
	return s.writeEvent("message_stop", map[string]any{"type": "message_stop"})
}

// anthropicStreamTransform 消费上游 OpenAI SSE，向 w 发射 Anthropic 事件流。
// 一旦写出 message_start，任何后续情况都以 message_stop 收尾（Anthropic 客户端
// 对"无 message_stop 的流"会挂起重试，宁可给空回复也不留悬挂流）。
// 返回 error 仅用于调用方日志（此时 HTTP 200 已发出，无法改状态码）。
func anthropicStreamTransform(w http.ResponseWriter, r io.Reader, model string) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)

	s := &anthropicStreamState{
		w: w, fl: fl, model: model,
		msgID:    fmt.Sprintf("msg_%d", time.Now().UnixNano()),
		toolSeen: map[int]bool{},
		toolArgs: map[int]string{},
	}
	// message_start + ping：连接建立即发（Anthropic 官方流首个事件恒为 message_start）。
	if err := s.writeEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": s.msgID, "type": "message", "role": "assistant", "model": model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	}); err != nil {
		return err
	}
	_ = s.writeEvent("ping", map[string]any{"type": "ping"})

	var (
		finishReason = "stop"
		sawFrame     bool
	)
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := br.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(trimmed, "data: ") {
			payload := strings.TrimPrefix(trimmed, "data: ")
			if payload == "[DONE]" {
				break
			}
			var chunk struct {
				ID      string `json:"id"`
				Choices []struct {
					Delta struct {
						Content          string `json:"content"`
						ReasoningContent string `json:"reasoning_content"`
						ToolCalls        []struct {
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
					CompletionTokens int `json:"completion_tokens"`
					PromptTokens     int `json:"prompt_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal([]byte(payload), &chunk) == nil {
				sawFrame = true
				if len(chunk.Choices) > 0 {
					c := chunk.Choices[0]
					d := c.Delta
					// 文本增量：跨块类型切换时先关旧块再开新块（index 单调）。
					if d.Content != "" {
						if err := s.emitDelta("text", "text_delta", d.Content); err != nil {
							return err
						}
					}
					if d.ReasoningContent != "" {
						if err := s.emitDelta("thinking", "thinking_delta", d.ReasoningContent); err != nil {
							return err
						}
					}
					for _, tc := range d.ToolCalls {
						if !s.toolSeen[tc.Index] {
							s.toolSeen[tc.Index] = true
							if err := s.closeBlock(); err != nil {
								return err
							}
							s.openType, s.openIdx = "tool_use", s.index
							name := tc.Function.Name
							if err := s.writeEvent("content_block_start", map[string]any{
								"type":  "content_block_start",
								"index": s.index,
								"content_block": map[string]any{
									"type": "tool_use", "id": tc.ID, "name": name, "input": map[string]any{},
								},
							}); err != nil {
								return err
							}
							s.index++
						}
						if tc.Function.Arguments != "" {
							s.toolArgs[tc.Index] += tc.Function.Arguments
							if err := s.writeEvent("content_block_delta", map[string]any{
								"type":  "content_block_delta",
								"index": s.openIdx,
								"delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
							}); err != nil {
								return err
							}
						}
					}
					if c.FinishReason != nil && *c.FinishReason != "" {
						finishReason = *c.FinishReason
					}
				}
				if chunk.Usage != nil {
					s.outToks = chunk.Usage.CompletionTokens
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}
	if !sawFrame {
		// 上游空流：不中断客户端——以 end_turn 空回复收尾（HTTP 200 已发出，
		// 写 error 事件外的任何"报错"形态都可能让 Claude Code 挂起重试）。
		return s.finish("end_turn")
	}
	return s.finish(anthropicStopReason(finishReason))
}

// emitDelta 文本/思考增量发射：块类型切换时先关旧块再开新块；同类型追加 delta。
// 字段名按块类型对齐 Anthropic 规范：text 块的载荷键是 "text"（text_delta），
// thinking 块的 content_block_start 与 thinking_delta 载荷键是 "thinking"——
// 键名不对 Claude Code 只会静默丢思考内容（容错解析），但面板/SDK 侧会显示为空。
func (s *anthropicStreamState) emitDelta(blockType, deltaType, text string) error {
	if s.openType != blockType {
		if err := s.closeBlock(); err != nil {
			return err
		}
		s.openType, s.openIdx = blockType, s.index
		block := map[string]any{"type": blockType}
		if blockType == "thinking" {
			block["thinking"] = ""
		} else {
			block["text"] = ""
		}
		if err := s.writeEvent("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         s.index,
			"content_block": block,
		}); err != nil {
			return err
		}
		s.index++
	}
	deltaKey := "text"
	if deltaType == "thinking_delta" {
		deltaKey = "thinking"
	}
	return s.writeEvent("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": s.openIdx,
		"delta": map[string]any{"type": deltaType, deltaKey: text},
	})
}
