// logging.go 请求级表格日志：每个 /v1/chat/completions 请求结束后打印一行到 stdout。
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// chatSeq 进程级请求序号。
var chatSeq atomic.Int64

// chatLogEnabled 聊天表格日志总开关。生产恒 true；
// 测试包经 TestMain 置 false 关闭 stdout 噪音，需要断言行输出的测试用 withChatLog 临时开启（R5）。
var chatLogEnabled = true

// chatStat 单个 chat 请求的日志统计；handler 挂 defer，请求出口后落一行。
type chatStat struct {
	start  time.Time
	model  string
	mode   string // "stream" | "sync"
	uid    string // 完整 uid，展示时只取前 8 位
	ttfb   time.Duration
	toks   int // <0 表示 usage 缺失 → 显示 "-"
	status int

	// metrics 观测载荷（/v1/stats）：两个汇合点（流式/非流式）填入，
	// observeMetrics 在出口统一上报一次。m* 前缀与日志字段区分。
	mPrompt   int64
	mHit      int64 // usage.prompt_cache_hit_tokens（缺失 0）
	mMiss     int64 // usage.prompt_cache_miss_tokens（缺失 0）
	mWrite    int64 // usage.prompt_cache_write_tokens（缺失 0）
	mCredit   float64
	mCreditOK bool // usage.credit 显式出现过（缺失≠0）
	mHasCache bool // 上游出现过任一 cache 字段（决定 hit_rate 口径）

	// errMsg 失败原因（权威分类，如 "hard_credit(402)"）：仅非 200 请求由轮转
	// 循环错误路径设置，observeMetrics 出口带进单条请求明细。刻意只存分类不存
	// 上游原始 Msg——明细会展示给面板用户，原文可能泄露账号 UID 与内部错误码。
	errMsg string

	// 单条明细的三个诊断维度（observeMetrics 出口带进 RequestRecord）：
	proto     string // "openai" / "anthropic"（响应协议来源）
	keyName   string // 业务 Key 名（多 Key 模式；旧单 Key 模式为空）
	tries     int    // 轮转尝试次数（真正出站的尝试，Acquire 竞态失败不计）
	respModel string // 上游响应回传的实际模型（auto 档可见真实路由；失败尝试为空）

	logged bool
}

// newChatStat 以请求进入 handler 的时刻为起点构造统计对象；toks 默认 -1（usage 缺失）。
func newChatStat(now time.Time, body []byte, stream bool) *chatStat {
	mode := "sync"
	if stream {
		mode = "stream"
	}
	return &chatStat{start: now, model: parseModelFromBody(body), mode: mode, toks: -1}
}

// done 幂等落一行表格日志。
func (s *chatStat) done() {
	if s.logged {
		return
	}
	s.logged = true
	logChatRow(s.ttfb, time.Since(s.start), s.model, s.mode, s.uid, s.status, s.toks)
}

// chatStatsReader 在流式透传时抓取 SSE 末帧的 usage.completion_tokens 精确值，
// 并记录首个 data 帧的 TTFB；原始字节原样返回给下游透传。
// 注意：不做 rune 估算，token 数一律采信上游 usage。
type chatStatsReader struct {
	br        *bufio.Reader
	start     time.Time
	ttfb      time.Duration
	seen      bool // 已见过首个 data 帧（TTFB 只记一次）
	hasUsage  bool // 末帧是否带 usage
	hasCredit bool // 是否出现过带 credit 的 usage（缺失≠0，见 Credit() 注释）
	hasCache  bool // 是否出现过任一 prompt_cache_* 字段（缺失≠全 0）
	tokens    int
	credit    float64 // 末帧 usage.credit（本次真实扣费，供成本账本）
	prompt    int     // 末帧 usage.prompt_tokens（与 completion 合计折算单价）

	cacheHit   int64  // 末帧 usage.prompt_cache_hit_tokens
	cacheMiss  int64  // 末帧 usage.prompt_cache_miss_tokens
	cacheWrite int64  // 末帧 usage.prompt_cache_write_tokens
	pend       []byte // 已读未返回的行缓存
	respModel  string // 上游首帧回传的实际模型（auto 档路由结果）
}

// newChatStatsReaderSince 以 since 为 TTFB 计时起点（通常是请求进入 handler 的时刻）。
func newChatStatsReaderSince(r io.Reader, since time.Time) *chatStatsReader {
	return &chatStatsReader{br: bufio.NewReaderSize(r, 64*1024), start: since}
}

// TTFB 返回首个 data 帧到达耗时；无帧时为 0。
func (s *chatStatsReader) TTFB() time.Duration { return s.ttfb }

// Tokens 返回末帧 usage.completion_tokens 与是否缺失；无 usage 时 ok=false。
func (s *chatStatsReader) Tokens() (int, bool) { return s.tokens, s.hasUsage }

// Credit 返回末帧 usage.credit（本次真实扣费）。ok=true 要求 usage 存在**且** credit
// 字段显式出现——字段缺失时 ok=false（缺失≠0：不能把"缺观测"当"0 成本"写入账本，
// 否则收费的号可能被误判 tier0 免费层）。显式 credit:0 仍是合法免费观测（ok=true）。
func (s *chatStatsReader) Credit() (float64, bool) { return s.credit, s.hasUsage && s.hasCredit }

// Model 返回上游响应回传的实际模型名；无帧/未回传时为空。
func (s *chatStatsReader) Model() string { return s.respModel }

// TotalTokens 返回本次请求总 token 数（prompt + completion），供成本单价折算。
func (s *chatStatsReader) TotalTokens() int { return s.prompt + s.tokens }

// PromptTokens 返回末帧 usage.prompt_tokens 与是否缺失。
func (s *chatStatsReader) PromptTokens() (int, bool) { return s.prompt, s.hasUsage }

// Cache 返回末帧 usage 的 prompt_cache_* 三元组。ok=true 要求 usage 存在且
// 至少一个 cache 字段显式出现（全缺 = 上游没开缓存观测，ok=false）。
func (s *chatStatsReader) Cache() (hit, miss, write int64, ok bool) {
	return s.cacheHit, s.cacheMiss, s.cacheWrite, s.hasUsage && s.hasCache
}

// parseSSELine 解析一行 "data: {...}"：首帧记 TTFB，含 usage 时采信精确 completion_tokens。
func (s *chatStatsReader) parseSSELine(line string) {
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "data: ") {
		return
	}
	payload := strings.TrimPrefix(line, "data: ")
	if payload == "[DONE]" {
		return
	}
	if !s.seen {
		s.seen = true
		s.ttfb = time.Since(s.start)
	}
	// 实际模型：任何携带 model 的帧都采信首帧（auto 档由上游选型，这里能看到真实
	// 路由结果）。必须放在 usage 判空之前——首帧通常无 usage 但带 model。
	if s.respModel == "" {
		var probe struct {
			Model string `json:"model"`
		}
		if json.Unmarshal([]byte(payload), &probe) == nil && probe.Model != "" {
			s.respModel = probe.Model
		}
	}
	var chunk struct {
		Model string `json:"model"`
		Usage *struct {
			CompletionTokens int      `json:"completion_tokens"`
			PromptTokens     int      `json:"prompt_tokens"`
			Credit           *float64 `json:"credit"` // 指针区分「缺失」与「显式 0」
			// prompt_cache_*：上游缓存命中观测（cache_key.go：hit 直接影响 credit 量级）。
			// 指针区分「缺失」与「显式 0」，任一出现即视为缓存观测开启。
			PromptCacheHitTokens   *int64 `json:"prompt_cache_hit_tokens"`
			PromptCacheMissTokens  *int64 `json:"prompt_cache_miss_tokens"`
			PromptCacheWriteTokens *int64 `json:"prompt_cache_write_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(payload), &chunk) != nil || chunk.Usage == nil {
		return
	}
	// 实际模型：取首帧回传（auto 档由上游选型，这里能看到真实路由结果）。
	if s.respModel == "" && chunk.Model != "" {
		s.respModel = chunk.Model
	}
	s.hasUsage = true
	s.tokens = chunk.Usage.CompletionTokens
	s.prompt = chunk.Usage.PromptTokens
	if chunk.Usage.Credit != nil {
		s.hasCredit = true
		s.credit = *chunk.Usage.Credit
	}
	if u := chunk.Usage; u.PromptCacheHitTokens != nil || u.PromptCacheMissTokens != nil || u.PromptCacheWriteTokens != nil {
		s.hasCache = true
		if u.PromptCacheHitTokens != nil {
			s.cacheHit = *u.PromptCacheHitTokens
		}
		if u.PromptCacheMissTokens != nil {
			s.cacheMiss = *u.PromptCacheMissTokens
		}
		if u.PromptCacheWriteTokens != nil {
			s.cacheWrite = *u.PromptCacheWriteTokens
		}
	}
}

// Read 返回原始数据，同时解析统计 TTFB/token。
func (s *chatStatsReader) Read(p []byte) (int, error) {
	if len(s.pend) > 0 {
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	line, err := s.br.ReadString('\n')
	if line != "" {
		s.parseSSELine(line)
		s.pend = []byte(line)
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	return 0, err
}

// parseModelFromBody 从请求 JSON 取 model 字段，缺省标 "-"。
func parseModelFromBody(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &obj); err != nil || obj.Model == "" {
		return "-"
	}
	return obj.Model
}

// completionTokens 从 Aggregate 返回的响应中提取 usage.completion_tokens；缺失返回 -1。
func completionTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	v, ok := u["completion_tokens"].(float64)
	if !ok {
		return -1
	}
	return int(v)
}

// usageCreditTotal 从聚合响应提取本次真实扣费与总 token 数（供成本账本）。
// ok=false 表示 usage 缺失或字段类型不符——此时不记录观测，避免污染账本。
func usageCreditTotal(resp map[string]any) (credit float64, total int, ok bool) {
	u, isMap := resp["usage"].(map[string]any)
	if !isMap {
		return 0, 0, false
	}
	c, hasCredit := u["credit"].(float64)
	pt, hasPrompt := u["prompt_tokens"].(float64)
	ct, hasCompletion := u["completion_tokens"].(float64)
	if !hasCredit || (!hasPrompt && !hasCompletion) {
		return 0, 0, false
	}
	return c, int(pt) + int(ct), true
}

// usagePromptTokens 从聚合响应提取 usage.prompt_tokens；缺失返回 false。
func usagePromptTokens(resp map[string]any) (int64, bool) {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return 0, false
	}
	v, ok := u["prompt_tokens"].(float64)
	if !ok {
		return 0, false
	}
	return int64(v), true
}

// usageCacheTokens 从聚合响应提取 usage 的 prompt_cache_* 三元组。
// ok=true 要求 usage 存在且至少一个 cache 字段显式出现（与流式 Cache() 同口径）。
func usageCacheTokens(resp map[string]any) (hit, miss, write int64, ok bool) {
	u, isMap := resp["usage"].(map[string]any)
	if !isMap {
		return 0, 0, 0, false
	}
	h, hasHit := u["prompt_cache_hit_tokens"].(float64)
	m, hasMiss := u["prompt_cache_miss_tokens"].(float64)
	w, hasWrite := u["prompt_cache_write_tokens"].(float64)
	if !hasHit && !hasMiss && !hasWrite {
		return 0, 0, 0, false
	}
	return int64(h), int64(m), int64(w), true
}

// uidPrefix 只显示 uid 前 8 位；空 uid 显示 "-"。
func uidPrefix(uid string) string {
	if uid == "" {
		return "-"
	}
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// logChatRow 打印一行请求级表格日志（直接输出 stdout，无 log 时间戳前缀）。
// toks<0 表示 usage 缺失，显示 "-"。
func logChatRow(ttfb, total time.Duration, model, mode, uid string, status int, toks int) {
	if !chatLogEnabled {
		return
	}
	seq := chatSeq.Add(1)
	if len(model) > 11 {
		model = model[:11]
	}
	tokField := "-"
	tokpsField := "-"
	if toks >= 0 {
		tokField = fmt.Sprintf("%d", toks)
		if total > 0 {
			tokpsField = fmt.Sprintf("%.1f", float64(toks)/total.Seconds())
		} else {
			tokpsField = "0.0"
		}
	}
	ttfbMS := "-"
	if ttfb > 0 {
		ttfbMS = fmt.Sprintf("%dms", ttfb.Milliseconds())
	}
	fmt.Fprintf(os.Stdout, "| #%03d | %s | %s | %s | %d | uid=%s | TTFB=%s | tok=%s | %stok/s | total=%.1fs |\n",
		seq,
		time.Now().Format("15:04:05"),
		model,
		mode,
		status,
		uidPrefix(uid),
		ttfbMS,
		tokField,
		tokpsField,
		total.Seconds(),
	)
}
