// 图片（多模态）翻译测试：user 消息 base64/url image 块 → OpenAI image_url part；
// tool_result 内的截图（Claude Code Read 工具形态）→ parts 数组；坏 source 明确 400。
package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// contentParts 把 OpenAI message 的 content（string 或 []part）统一成 parts 切片。
func contentParts(t *testing.T, raw []byte, idx int) (role string, parts []map[string]any) {
	t.Helper()
	var msgs []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	rawMsgs, _ := json.Marshal(msgs)
	_ = rawMsgs
	var arr []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	var src []anthropicMessage
	_ = src
	_ = arr
	// 简化：直接解析 translateMessagesToOpenAI 的输出
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal messages: %v raw=%s", err, raw)
	}
	if idx >= len(out) {
		t.Fatalf("message %d out of range (%d)", idx, len(out))
	}
	role, _ = out[idx]["role"].(string)
	switch c := out[idx]["content"].(type) {
	case string:
		parts = []map[string]any{{"type": "text", "text": c}}
	case []any:
		for _, pi := range c {
			if pm, ok := pi.(map[string]any); ok {
				parts = append(parts, pm)
			}
		}
	}
	return role, parts
}

func TestMessagesImageBase64InUserMessage(t *testing.T) {
	req := &anthropicRequest{
		MaxTokens: 10,
		Messages: []anthropicMessage{
			{Role: "user", Content: json.RawMessage(`[
				{"type":"text","text":"这张图里是什么"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}
			]`)},
		},
	}
	msgs, err := translateMessagesToOpenAI(req.Messages)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	raw, _ := json.Marshal(msgs)
	role, parts := contentParts(t, raw, 0)
	if role != "user" || len(parts) != 2 {
		t.Fatalf("want user with 2 parts, got role=%s parts=%s", role, raw)
	}
	if parts[0]["type"] != "text" || parts[1]["type"] != "image_url" {
		t.Fatalf("parts order wrong: %v", parts)
	}
	img, _ := parts[1]["image_url"].(map[string]any)
	url, _ := img["url"].(string)
	if url != "data:image/png;base64,AAAA" {
		t.Fatalf("data url wrong: %s", url)
	}
}

func TestMessagesImageURLSource(t *testing.T) {
	req := &anthropicRequest{
		MaxTokens: 10,
		Messages: []anthropicMessage{
			{Role: "user", Content: json.RawMessage(`[
				{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}
			]`)},
		},
	}
	msgs, err := translateMessagesToOpenAI(req.Messages)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	raw, _ := json.Marshal(msgs)
	_, parts := contentParts(t, raw, 0)
	img, _ := parts[0]["image_url"].(map[string]any)
	if url, _ := img["url"].(string); url != "https://example.com/a.png" {
		t.Fatalf("url passthrough wrong: %s", url)
	}
}

func TestMessagesImageInToolResult(t *testing.T) {
	// Claude Code 的截图形态：Read 工具返回 image 块 + 文本。
	req := &anthropicRequest{
		MaxTokens: 10,
		Messages: []anthropicMessage{
			{Role: "user", Content: json.RawMessage(`"看下截图"`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"t1","name":"Read","input":{"path":"a.png"}}]`)},
			{Role: "user", Content: json.RawMessage(`[
				{"type":"tool_result","tool_use_id":"t1","content":[
					{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"BBBB"}},
					{"type":"text","text":"截图内容"}
				]}
			]`)},
		},
	}
	msgs, err := translateMessagesToOpenAI(req.Messages)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	raw, _ := json.Marshal(msgs)
	role, parts := contentParts(t, raw, 2)
	if role != "tool" {
		t.Fatalf("want tool role, got %s", role)
	}
	hasImg, hasText := false, false
	for _, p := range parts {
		if p["type"] == "image_url" {
			hasImg = true
			if img, _ := p["image_url"].(map[string]any); img["url"] != "data:image/jpeg;base64,BBBB" {
				t.Fatalf("tool image url wrong: %v", img)
			}
		}
		if p["type"] == "text" && p["text"] == "截图内容" {
			hasText = true
		}
	}
	if !hasImg || !hasText {
		t.Fatalf("tool result parts missing image/text: %v", parts)
	}
}

func TestMessagesImageBadSourceRejected(t *testing.T) {
	req := &anthropicRequest{
		MaxTokens: 10,
		Messages: []anthropicMessage{
			{Role: "user", Content: json.RawMessage(`[{"type":"image","source":{"type":"file","path":"a.png"}}]`)},
		},
	}
	_, err := translateMessagesToOpenAI(req.Messages)
	if err == nil || !strings.Contains(err.Error(), "unsupported image source type") {
		t.Fatalf("want unsupported source error, got %v", err)
	}
}

func TestMessagesTextOnlyStaysString(t *testing.T) {
	// 回归：纯文本消息必须保持字符串形态（多模态数组仅在含图片时启用）。
	req := &anthropicRequest{
		MaxTokens: 10,
		Messages: []anthropicMessage{
			{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"第一段"},{"type":"text","text":"第二段"}]`)},
		},
	}
	msgs, err := translateMessagesToOpenAI(req.Messages)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	raw, _ := json.Marshal(msgs)
	if strings.Contains(string(raw), `"type":"text"`) {
		t.Fatalf("text-only message should stay string content: %s", raw)
	}
	role, parts := contentParts(t, raw, 0)
	if role != "user" || len(parts) != 1 || parts[0]["text"] != "第一段\n第二段" {
		t.Fatalf("text join wrong: %v", parts)
	}
}
