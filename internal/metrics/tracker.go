// Tracker：按模型聚合的线程安全累计器 + 派生快照。并发风格与 pool 一致：
// RWMutex 护 map、atomic dirty 标记、后台 flusher 周期落盘。
package metrics

import (
	"sync"
	"sync/atomic"
	"time"
)

// recentCap 单条请求明细环形缓冲容量：最近 N 条（面板「请求明细」表展示上限）。
// 200 条对 200 元素 slice 平移的维护成本可忽略（<µs 级）；将来加量再换真环形索引。
const recentCap = 200

// Tracker 请求统计累计器。nil *Tracker 视为"统计关闭"（handler 侧判空）。
type Tracker struct {
	mu     sync.RWMutex
	dirty  atomic.Bool
	file   string // 落盘路径（空 = 不落盘，仅内存）
	since  time.Time
	models map[string]*ModelAccum
	recent []RequestRecord // 单条请求明细，时间升序存放（尾部最新），cap recentCap

	// usage 按账号 × 模型 × 按日的积分/token 消耗
	// （uidPrefix → date → model → item），随 metrics.json 落盘。
	// 消耗为 0 的免费层请求也计请求数与 token，保证"该号跑过"可见。见 usage.go。
	usage map[string]map[string]map[string]*usageItem

	stopCh    chan struct{}
	closeOnce sync.Once

	persistFails int // 连续落盘失败计数（节流日志用，仅 save/notePersistFail 触碰）
}

// New 构造 Tracker：读回持久化状态并启动后台 flusher。filePath 为空时
// 不落盘（纯内存，测试用）。
func New(filePath string) *Tracker {
	t := &Tracker{
		file:   filePath,
		since:  time.Now(),
		models: map[string]*ModelAccum{},
		usage:  map[string]map[string]map[string]*usageItem{},
	}
	t.load()
	if filePath != "" {
		t.startFlusher()
	}
	return t
}

// Observe 记录一次请求观测。success 口径：Status==200（旋转循环结束后的
// 最终响应码；上游错误/网关 4xx5xx 均计 failed）。
func (t *Tracker) Observe(o Observe) {
	t.mu.Lock()
	defer t.mu.Unlock()
	a := t.models[o.Model]
	if a == nil {
		a = &ModelAccum{}
		t.models[o.Model] = a
	}
	a.Requests++
	if o.Status == 200 {
		a.Success++
	} else {
		a.Failed++
	}
	if o.Stream {
		a.Streaming++
	}
	if o.TTFB > 0 {
		a.TTFObsCount++
		a.SumTTFBMS += float64(o.TTFB.Microseconds()) / 1000.0
	}
	a.SumWallMS += float64(o.Wall.Microseconds()) / 1000.0
	a.PromptTokens += o.PromptTokens
	a.CompletionTokens += o.CompletionTokens
	if o.CacheObserved {
		a.CacheObserved = true
		a.CacheHit += o.CacheHit
		a.CacheMiss += o.CacheMiss
		a.CacheWrite += o.CacheWrite
	}
	if o.CreditOK {
		a.CreditSum += o.Credit
	}
	a.LastSeen = o.Now
	t.recordUsage(o)
	t.appendRecent(o)
	t.dirty.Store(true)
}

// appendRecent 追加单条请求明细（调用方必须已持 t.mu）：超限丢最旧。
// 用时间升序 slice + 头部淘汰（copy 平移），Snapshot 时反向输出（新→旧）。
func (t *Tracker) appendRecent(o Observe) {
	rec := RequestRecord{
		Time:             o.Now.Format(time.RFC3339),
		Model:            o.Model,
		Stream:           o.Stream,
		Status:           o.Status,
		UID:              o.UID,
		TTFBMS:           o.TTFB.Milliseconds(),
		WallMS:           o.Wall.Milliseconds(),
		PromptTokens:     o.PromptTokens,
		CompletionTokens: o.CompletionTokens,
		CacheHit:         o.CacheHit,
		CacheMiss:        o.CacheMiss,
		Err:              o.Err,
		Proto:            o.Proto,
		KeyName:          o.KeyName,
		Tries:            o.Tries,
		RespModel:        o.RespModel,
		ReqModel:         o.ReqModel,
	}
	// Credit 只在显式观测时记录（缺失≠0 口径，与聚合的 CreditSum 一致）；
	// 显式 0（免费层命中）对明细同样有意义，不滤零。
	if o.CreditOK {
		rec.Credit = o.Credit
	}
	if len(t.recent) >= recentCap {
		copy(t.recent, t.recent[1:])
		t.recent[len(t.recent)-1] = rec
		return
	}
	t.recent = append(t.recent, rec)
}

// Snapshot 派生当前统计：per-model 派生 + total 归并，models 按 requests 降序
// （同数按模型名，保证面板排序稳定）。 uptime_sec 自 since 起算。
func (t *Tracker) Snapshot() Stats {
	now := time.Now()
	t.mu.RLock()
	models := make([]ModelStat, 0, len(t.models))
	total := ModelAccum{LastSeen: time.Time{}}
	for model, a := range t.models {
		ms := derive(a)
		ms.Model = model // map key 即模型名（ModelAccum 只存累计量，不带名）
		models = append(models, ms)
		total.Requests += a.Requests
		total.Success += a.Success
		total.Failed += a.Failed
		total.Streaming += a.Streaming
		total.TTFObsCount += a.TTFObsCount
		total.SumTTFBMS += a.SumTTFBMS
		total.SumWallMS += a.SumWallMS
		total.PromptTokens += a.PromptTokens
		total.CompletionTokens += a.CompletionTokens
		if a.CacheObserved {
			total.CacheObserved = true
			total.CacheHit += a.CacheHit
			total.CacheMiss += a.CacheMiss
			total.CacheWrite += a.CacheWrite
		}
		total.CreditSum += a.CreditSum
		if a.LastSeen.After(total.LastSeen) {
			total.LastSeen = a.LastSeen
		}
	}
	since := t.since
	recent := make([]RequestRecord, len(t.recent))
	// 新→旧输出（面板明细表首行是最新请求）：recent 内部是时间升序，倒序拷贝。
	for i, r := range t.recent {
		recent[len(t.recent)-1-i] = r
	}
	usage := t.usageSnapshot(now)
	t.mu.RUnlock()

	sortModels(models)
	totStat := derive(&total)
	totStat.Model = "total" // 面板 summary 卡不显示该名，但语义完整
	return Stats{
		Enabled:   true,
		Since:     since,
		Now:       now,
		UptimeSec: int64(now.Sub(since).Seconds()),
		Total:     totStat,
		Models:    models,
		Recent:    recent,
		Usage:     usage,
	}
}

// Reset 清空全部累计并把 since 重置为当前时刻（面板"重置统计"语义：
// 之后看到的是干净增量）。单条请求明细与按账号消耗同属统计范畴，一并清空。
func (t *Tracker) Reset() {
	t.mu.Lock()
	t.models = map[string]*ModelAccum{}
	t.recent = nil
	t.usage = map[string]map[string]map[string]*usageItem{}
	t.since = time.Now()
	t.mu.Unlock()
	t.dirty.Store(true)
}

// Flush 同步落盘（幂等：无变更不写盘）。进程退出前调用。
func (t *Tracker) Flush() {
	t.mu.Lock()
	if t.dirty.Swap(false) {
		t.saveLocked()
	}
	t.mu.Unlock()
}

// Close 停止后台 flusher 并做最后一次落盘（幂等）。
func (t *Tracker) Close() {
	t.closeOnce.Do(func() {
		if t.stopCh != nil {
			close(t.stopCh)
		}
	})
	t.Flush()
}

// derive 从累计和算派生统计。口径：
//   - avg_ttfb_ms 分母 = 有首帧的流式请求数（非流式不摊薄）；
//   - avg_latency_ms 分母 = 全部请求；
//   - tokens_per_sec = completion / 墙钟秒（与 logChatRow 的 tok/s 同口径，
//     含首字等待——是"端到端吞吐"而非"纯生成速度"）；
//   - cache_hit_rate = hit/(hit+miss)（双方都观测到）；只观测到 hit 时回退
//     hit/prompt（DeepSeek 风格 API 的 prompt 含缓存命中 token，hit ≤ prompt）。
func derive(a *ModelAccum) ModelStat {
	ms := ModelStat{
		Model:            "",
		Requests:         a.Requests,
		Success:          a.Success,
		Failed:           a.Failed,
		Streaming:        a.Streaming,
		PromptTokens:     a.PromptTokens,
		CompletionTokens: a.CompletionTokens,
		TotalTokens:      a.PromptTokens + a.CompletionTokens,
		CacheHitTokens:   a.CacheHit,
		CacheMissTokens:  a.CacheMiss,
		CacheWriteTokens: a.CacheWrite,
		Credit:           a.CreditSum,
	}
	if a.TTFObsCount > 0 {
		ms.AvgTTFBMS = a.SumTTFBMS / float64(a.TTFObsCount)
	}
	if a.Requests > 0 {
		ms.AvgLatencyMS = a.SumWallMS / float64(a.Requests)
		ms.CreditPerReq = a.CreditSum / float64(a.Requests)
	}
	if a.SumWallMS > 0 {
		ms.TokensPerSec = float64(a.CompletionTokens) / (a.SumWallMS / 1000.0)
	}
	switch {
	// 分母需要 miss 观测：miss 从未出现过（只报 hit）时 hit/(hit+0)=1.0 会把
	// "未上报" 高估成全命中，此时回退 hit/prompt（prompt 含缓存命中 token）。
	case a.CacheObserved && a.CacheMiss > 0:
		ms.CacheHitRate = float64(a.CacheHit) / float64(a.CacheHit+a.CacheMiss)
	case a.CacheObserved && a.CacheHit > 0 && a.PromptTokens > 0:
		ms.CacheHitRate = float64(a.CacheHit) / float64(a.PromptTokens)
	}
	if !a.LastSeen.IsZero() {
		ls := a.LastSeen
		ms.LastSeen = &ls
	}
	return ms
}

// sortModels 按 requests 降序，同数按模型名升序（稳定排序便于面板与测试）。
func sortModels(ms []ModelStat) {
	for i := 1; i < len(ms); i++ {
		for j := i; j > 0; j-- {
			a, b := ms[j-1], ms[j]
			if a.Requests > b.Requests || (a.Requests == b.Requests && a.Model <= b.Model) {
				break
			}
			ms[j-1], ms[j] = b, a
		}
	}
}
