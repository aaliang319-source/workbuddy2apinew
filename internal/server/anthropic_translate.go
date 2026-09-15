// anthropic_translate.go Anthropic Messages 请求 → OpenAI chat body 翻译层。
//
// 参考：https://docs.anthropic.com/en/api/messages（Messages API）。
// 翻译策略：产出标准 OpenAI chat completions JSON（map 形态），交给既有
// ChatStreamContext → prepareBody 管线（强制 stream / tool_choice 归一 / 孤儿
// tool 配对清理等全部复用）。仅 /v1/messages 端点使用本文件的 typed struct；
// OpenAI 路径保持 raw passthrough 不受影响。
//
// 关键映射（与 handler_messages.go 的 resolveAnthropicModel 配合）：
//   - system（string 或 text 块数组）→ messages[0] 的 system 消息；
//   - assistant 历史中的 thinking 块丢弃（D3）：上游不认该角色，且 signature 无法
//     回传校验——不上送即不会因 signature 失败；
//   - tool_use 块 → 同一 assistant 消息的 tool_calls[]（arguments 由 input 序列化）；
//   - tool_result 块 → 独立 role=tool 消息（保持出现顺序，OpenAI 语义）；
//   - metadata.user_id → OpenAI body metadata.conversation_id（粘性会话键，
//     session.ExtractKey 直接识别，零改动复用粘性路由）。
package server

import (
	"encoding/json"
	"fmt"
	"strings"
)

// anthropicRequest Anthropic Messages API 请求体（docs.anthropic.com/messages）。
// 只声明需要翻译/路由的字段；未知字段（container/mcp 等）由 json 忽略。
type anthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"` // Anthropic 强制字段，缺失 → 400
	System        json.RawMessage    `json:"system"`     // string 或 []content block
	Messages      []anthropicMessage `json:"messages"`
	Tools         []anthropicTool    `json:"tools"`
	ToolChoice    json.RawMessage    `json:"tool_choice"`
	Temperature   *float64           `json:"temperature"`
	TopP          *float64           `json:"top_p"`
	StopSequences []string           `json:"stop_sequences"`
	Stream        bool               `json:"stream"`
	Metadata      json.RawMessage    `json:"metadata"` // 取 user_id 做粘性键
}

// anthropicMessage 单条消息：content 为 string 或 []block（双形态，RawMessage 承接）。
type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// anthropicBlock content block：type = text / tool_use / tool_result / thinking / image。
// image 等不支持类型在翻译时报错（400 invalid_request_error，明示拒绝优于静默丢内容）。
type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`          // tool_use
	Name      string          `json:"name"`        // tool_use
	Input     json.RawMessage `json:"input"`       // tool_use
	ToolUseID string          `json:"tool_use_id"` // tool_result
	Content   json.RawMessage `json:"content"`     // tool_result（string 或 []block）
	Thinking  string          `json:"thinking"`    // thinking
	Source    json.RawMessage `json:"source"`      // image（仅用于识别报错）
}

// anthropicTool 工具定义：input_schema 即 OpenAI function.parameters。
type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// translateAnthropicToOpenAI 把 Anthropic 请求翻译为 OpenAI chat body。
// mappedModel 已由 resolveAnthropicModel 解析（含 cn:/global: 路由前缀）。
func translateAnthropicToOpenAI(req *anthropicRequest, mappedModel string) (map[string]any, error) {
	msgs, err := translateMessagesToOpenAI(req.Messages)
	if err != nil {
		return nil, err
	}
	// system：string 直取 / []text 块 join，置于消息序列首位。
	if sys := anthropicSystemText(req.System); sys != "" {
		msgs = append([]map[string]any{{"role": "system", "content": sys}}, msgs...)
	}
	body := map[string]any{
		"model":      mappedModel,
		"max_tokens": req.MaxTokens,
		"messages":   msgs,
		"stream":     req.Stream,
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					// input_schema 缺省给空对象（OpenAI 要求 parameters 为 schema 对象）
					"parameters": orEmptyObject(t.InputSchema),
				},
			})
		}
		body["tools"] = tools
	}
	if tc := translateToolChoice(req.ToolChoice); tc != nil {
		body["tool_choice"] = tc
	}
	if len(req.StopSequences) > 0 {
		body["stop"] = req.StopSequences
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	// 粘性会话键：Anthropic 客户端没有 conversation_id 概念，借 metadata.user_id
	// （Claude Code 恒带，值为机器级稳定哈希）注入 OpenAI 形态的 metadata，
	// session.ExtractKey 即可零改动提取（多轮对话钉住同账号）。
	if uid := anthropicMetadataUserID(req.Metadata); uid != "" {
		body["metadata"] = map[string]any{"conversation_id": uid}
	}
	return body, nil
}

// translateMessagesToOpenAI 逐消息逐块映射（核心映射见文件头注释）。
// user 消息内 text 与 tool_result 交错时：text 累积为 user 消息，遇 tool_result
// 先冲刷 pending text 再追加 tool 消息——保持 Anthropic 的出现顺序语义。
// 容错：messages 数组内出现 system/developer 角色时按 OpenAI 同义角色透传
// （严格实现会 400，但部分客户端/代理会把系统提示塞进 messages；网关自身也会
// 用 role=system 组织上游请求，说明上游接受，没必要在此打断整段对话）。
func translateMessagesToOpenAI(msgs []anthropicMessage) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(msgs)+1)
	for _, m := range msgs {
		switch m.Role {
		case "user", "assistant":
		case "system", "developer":
			// string 或 []block(text join) 两种形态统一取纯文本。
			text := anthropicSystemText(m.Content)
			if text != "" {
				out = append(out, map[string]any{"role": m.Role, "content": text})
			}
			continue
		default:
			return nil, fmt.Errorf("unsupported message role %q", m.Role)
		}
		// content 为纯 string：直通（最常见形态，零损耗）。
		var textOnly string
		if err := json.Unmarshal(m.Content, &textOnly); err == nil {
			out = append(out, map[string]any{"role": m.Role, "content": textOnly})
			continue
		}
		var blocks []anthropicBlock
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			return nil, fmt.Errorf("parse %s content: %w", m.Role, err)
		}
		if m.Role == "user" {
			translated, err := translateUserBlocks(blocks)
			if err != nil {
				return nil, err
			}
			out = append(out, translated...)
			continue
		}
		// assistant：text 块 join 为 content，tool_use 块聚合为 tool_calls，thinking 丢弃。
		var texts []string
		var toolCalls []map[string]any
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if b.Text != "" {
					texts = append(texts, b.Text)
				}
			case "tool_use":
				args := orEmptyObject(b.Input)
				argsRaw, err := json.Marshal(args)
				if err != nil {
					return nil, fmt.Errorf("marshal tool_use input: %w", err)
				}
				toolCalls = append(toolCalls, map[string]any{
					"id":   b.ID,
					"type": "function",
					"function": map[string]any{
						"name":      b.Name,
						"arguments": string(argsRaw),
					},
				})
			case "thinking", "redacted_thinking":
				// 丢弃（D3）：signature 无法回传校验，上送反而有风险。
				// redacted_thinking 一并丢弃——Claude Code 开扩展思考后历史消息会带回
				// 该块（含 base64 数据），按未知类型 400 会打断整段多轮对话。
			default:
				return nil, fmt.Errorf("unsupported assistant content block type %q", b.Type)
			}
		}
		msg := map[string]any{"role": "assistant", "content": strings.Join(texts, "\n")}
		if len(toolCalls) > 0 {
			msg["tool_calls"] = toolCalls
		}
		out = append(out, msg)
	}
	return out, nil
}

// translateUserBlocks 把 user 消息的 block 数组翻译为 0..n 条 OpenAI 消息：
// text 块累积成一条 user 消息；tool_result 块逐块独立成 role=tool 消息。
func translateUserBlocks(blocks []anthropicBlock) ([]map[string]any, error) {
	var out []map[string]any
	var texts []string
	flushText := func() {
		if len(texts) > 0 {
			out = append(out, map[string]any{"role": "user", "content": strings.Join(texts, "\n")})
			texts = texts[:0]
		}
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				texts = append(texts, b.Text)
			}
		case "tool_result":
			flushText()
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": b.ToolUseID,
				"content":      toolResultText(b),
			})
		case "image":
			return nil, fmt.Errorf("image blocks are not supported by this gateway (cn:/global: text models only)")
		default:
			return nil, fmt.Errorf("unsupported user content block type %q", b.Type)
		}
	}
	flushText()
	return out, nil
}

// toolResultText 提取 tool_result 的文本内容：string 直取；[]block 取 text join。
// 非文本块（如 image）在 tool_result 内按 Anthropic 语义跳过（工具结果以文本回传为主流）。
func toolResultText(b anthropicBlock) string {
	var s string
	if err := json.Unmarshal(b.Content, &s); err == nil {
		return s
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(b.Content, &blocks); err == nil {
		var texts []string
		for _, sub := range blocks {
			if sub.Type == "text" && sub.Text != "" {
				texts = append(texts, sub.Text)
			}
		}
		return strings.Join(texts, "\n")
	}
	return ""
}

// anthropicSystemText 提取 system 字段文本：string 直取；[]block 取 text 块 join "\n"。
func anthropicSystemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var texts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				texts = append(texts, b.Text)
			}
		}
		return strings.Join(texts, "\n")
	}
	return ""
}

// translateToolChoice 把 Anthropic tool_choice 翻译为 OpenAI **对象形态**：
// 上游 normalizeToolChoice（payload.go）已支持对象归一（auto/required→string、
// function→name 字符串、none→删 tools），翻译层产出对象即可零改动复用。
// 返回 nil = 不设置该字段。解析失败回退 "auto"（宽容：tool_choice 非法不该 400 整个请求）。
func translateToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "auto"
	}
	switch obj.Type {
	case "any":
		return map[string]any{"type": "required"}
	case "tool":
		return map[string]any{"type": "function", "function": map[string]any{"name": obj.Name}}
	case "none":
		return map[string]any{"type": "none"}
	case "auto":
		return map[string]any{"type": "auto"}
	default:
		return "auto"
	}
}

// anthropicMetadataUserID 从 metadata 提取 user_id（粘性键）；缺失返回空串。
func anthropicMetadataUserID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var meta struct {
		UserID string `json:"user_id"`
	}
	if json.Unmarshal(raw, &meta) != nil {
		return ""
	}
	return meta.UserID
}

// orEmptyObject JSON 原始值兜底：nil/非法 → 空对象（schema/参数缺省形态）。
func orEmptyObject(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || !json.Valid(raw) {
		return json.RawMessage("{}")
	}
	return raw
}

// anthropicContentText 提取消息 content 的纯文本（count_tokens 估算与调试用）。
func anthropicContentText(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var texts []string
		for _, b := range blocks {
			if b.Text != "" {
				texts = append(texts, b.Text)
			}
		}
		return strings.Join(texts, "\n")
	}
	return ""
}
