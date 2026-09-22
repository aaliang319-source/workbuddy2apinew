// Package metrics 按模型聚合的请求统计（/v1/stats）：
// handler 出口观测（Observe）→ 累计（ModelAccum）→ 派生快照（Snapshot）→
// 本地落盘（data/metrics.json，重启不丢）。JSON 契约与控制面板
// workbuddy2api-gui/internal/gateway/client.go 的 ModelStat/Stats 逐字段对应。
package metrics

import "time"

// Observe 单次请求的原始观测（handler 两个汇合点填入 chatStat，出口统一上报一次；
// 账号旋转循环整个只记 1 次，口径是"请求级"而非"尝试级"）。
type Observe struct {
	Model  string // bareModel（realm 前缀已剥），聚合主键
	Stream bool
	Status int           // 网关最终响应码（旋转循环结束后的 st.status）
	TTFB   time.Duration // 流式首字延迟；非流式为 0（不参与 avg_ttfb 分母）
	Wall   time.Duration // 端到端耗时（handler 出口 time.Since(start)）

	PromptTokens     int64
	CompletionTokens int64 // usage 缺失时为 0（toks<0 映射为 0）
	CacheHit         int64 // 上游 usage.prompt_cache_hit_tokens；缺失 0
	CacheMiss        int64 // 上游 usage.prompt_cache_miss_tokens；缺失 0
	CacheWrite       int64 // 上游 usage.prompt_cache_write_tokens；缺失 0
	CacheObserved    bool  // 上游是否出现过任一 cache 字段（决定 hit_rate 口径）

	Credit   float64 // 上游 usage.credit（本次真实扣费，账号积分）
	CreditOK bool    // 缺失 ≠ 显式 0（与 chatStatsReader.Credit() 同口径）
	Now      time.Time

	// Err 失败原因（仅非 200 请求填）：权威分类字符串，如 "hard_credit(402)"。
	// 刻意不含上游原始 Msg——那可能携带账号 UID 与内部错误码（见 handler.go 尾部
	// 注释），单条明细会展示给面板用户，不能透传。
	Err string

	// UID 账号 UID 前 8 位（uidPrefix 惯例）；未选到号（如 503 no_account_available）
	// 时为空串，明细表显示 "-"。
	UID string

	// Proto 响应协议："openai" / "anthropic"；Key 业务 Key 名；Tries 轮转尝试次数。
	Proto   string
	KeyName string
	Tries   int
	// RespModel 上游响应回传的实际模型（auto 档路由结果；失败尝试/无帧为空）。
	RespModel string
	// ReqModel 原始请求模型名（客户端发来的名字；与最终服务模型可能不同）。
	ReqModel string
}

// ModelAccum 单模型累计量（落盘形态，只存和与计数；派生值在 Snapshot 时算）。
type ModelAccum struct {
	Requests  int64 `json:"requests"`
	Success   int64 `json:"success"`
	Failed    int64 `json:"failed"`
	Streaming int64 `json:"streaming"`

	// TTFObsCount TTFB 被观测到的流式请求数（avg_ttfb_ms 的分母；
	// 非流式请求不摊薄 avg_ttfb）。
	TTFObsCount int64   `json:"ttf_obs_count"`
	SumTTFBMS   float64 `json:"sum_ttfb_ms"`
	SumWallMS   float64 `json:"sum_wall_ms"`

	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`

	CacheHit      int64 `json:"cache_hit"`
	CacheMiss     int64 `json:"cache_miss"`
	CacheWrite    int64 `json:"cache_write"`
	CacheObserved bool  `json:"cache_observed"`

	CreditSum float64   `json:"credit_sum"`
	LastSeen  time.Time `json:"last_seen"`
}

// ModelStat 派生统计（与面板 client.go ModelStat 逐字段对应；面板据此渲染 +
// 做官方价换算——prompt/hit/miss/completion 四个 token 数是换算的输入）。
type ModelStat struct {
	Model     string `json:"model"`
	Requests  int64  `json:"requests"`
	Success   int64  `json:"success"`
	Failed    int64  `json:"failed"`
	Streaming int64  `json:"streaming"`

	AvgTTFBMS    float64 `json:"avg_ttfb_ms"`
	AvgLatencyMS float64 `json:"avg_latency_ms"`
	TokensPerSec float64 `json:"tokens_per_sec"`

	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`

	CacheHitTokens   int64   `json:"cache_hit_tokens"`
	CacheMissTokens  int64   `json:"cache_miss_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CacheHitRate     float64 `json:"cache_hit_rate"`

	Credit       float64 `json:"credit"`
	CreditPerReq float64 `json:"credit_per_req"`

	LastSeen *time.Time `json:"last_seen,omitempty"`
}

// RequestRecord 单条请求明细（recent 环形缓冲的元素，面板「请求明细」表的数据源）。
// 与 ModelAccum 聚合口径互补：聚合看趋势，明细看个案（哪条请求慢了/失败了/扣了多少）。
type RequestRecord struct {
	Time   string `json:"time"`  // RFC3339 请求出口时刻
	Model  string `json:"model"` // bareModel（与聚合主键同口径）
	Stream bool   `json:"stream"`
	Status int    `json:"status"`            // 200=成功
	UID    string `json:"uid,omitempty"`     // 账号 UID 前 8 位（uidPrefix 惯例）；未选到号时为空
	TTFBMS int64  `json:"ttfb_ms,omitempty"` // 流式首字；非流式 0
	WallMS int64  `json:"wall_ms"`           // 端到端耗时

	PromptTokens     int64 `json:"prompt_tokens,omitempty"`
	CompletionTokens int64 `json:"completion_tokens,omitempty"`
	CacheHit         int64 `json:"cache_hit_tokens,omitempty"`
	CacheMiss        int64 `json:"cache_miss_tokens,omitempty"`

	Credit float64 `json:"credit,omitempty"` // 本次真实扣费（CreditOK 才非零）
	Err    string  `json:"err,omitempty"`    // 失败原因（权威分类，不含上游原始 Msg）

	// Proto 协议来源："openai"（/v1/chat/completions 等）/ "anthropic"（/v1/messages）；
	// KeyName 命中的业务 Key 名（多 Key 模式，便于在明细里区分是哪个 Key 的流量）；
	// Tries 轮转尝试次数（>1 表示换过号，用于排查限流/失效）。
	Proto   string `json:"proto,omitempty"`
	KeyName string `json:"key_name,omitempty"`
	Tries   int    `json:"tries,omitempty"`
	// RespModel 上游响应回传的实际模型（auto 档可见真实路由；旧记录缺失）。
	RespModel string `json:"resp_model,omitempty"`
	// ReqModel 原始请求模型名（客户端发来的名字；旧记录缺失）。
	ReqModel string `json:"req_model,omitempty"`
}

// Stats /v1/stats 响应体（对应面板 client.go Stats）。Models 空时为 [] 非 nil
// （面板 types 是 ModelStat[] | null 且渲染端有兜底，但数组更稳）。
type Stats struct {
	Enabled   bool        `json:"enabled"`
	Message   string      `json:"message,omitempty"`
	Since     time.Time   `json:"since"`
	Now       time.Time   `json:"now"`
	UptimeSec int64       `json:"uptime_sec"`
	Total     ModelStat   `json:"total"`
	Models    []ModelStat `json:"models"`
	// Recent 单条请求明细（最近 recentCap 条，新→旧）；统计关闭/被 Reset 后为 nil。
	Recent []RequestRecord `json:"recent,omitempty"`
}
