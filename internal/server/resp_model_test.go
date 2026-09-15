package server

import (
	"io"
	"strings"
	"testing"
	"time"
)

// 验证 reader 能从 SSE 流里抓到实际模型。
func TestRespModelCapture(t *testing.T) {
	frames := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}],\"model\":\"deepseek-v4.1-flash\"}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"model\":\"deepseek-v4.1-flash\",\"usage\":{\"completion_tokens\":2,\"prompt_tokens\":5}}\n\n" +
		"data: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(frames), time.Now())
	_, _ = io.Copy(io.Discard, r)
	if got := r.Model(); got != "deepseek-v4.1-flash" {
		t.Fatalf("respModel=%q want deepseek-v4.1-flash", got)
	}
}
