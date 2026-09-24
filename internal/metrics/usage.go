// usage.go 按账号 × 模型 × 按日的积分/token 消耗累计器（面板仪表盘「积分消耗」卡片
// 与其详情弹窗的数据源）。
//
// 与 per-model 聚合（tracker.go）互补：模型视角看全局成本构成，账号视角看每个号
// 每天在各模型上烧了多少。存储形态是三层 map：
//
//	usage[uidPrefix][day][model] = {credit, requests, prompt, completion, cacheHit, cacheMiss}
//
// 随 metrics.json 落盘（同 flusher / 同 dirty 标记），重启不丢；保留
// usageRetentionDays 天，load 与 Observe 双侧裁剪。
//
// 账号主键沿用 uidPrefix 惯例（UID 前 8 位）：面板明细表同款脱敏口径。
// token 三元组随 credit 一并存储，使面板能做账号级的官方价换算
//（「这个号相当于省了多少钱」）——只存积分是不够的，官方价按 token 计价。
package metrics

import (
	"sort"
	"time"
)

// usageRetentionDays 按日消耗的保留天数：仪表盘展示近 30 天趋势 + 昨日对比，
// 保留 30 天把 metrics.json 体积增长限制在百 KB ~ MB 级。
const usageRetentionDays = 30

// unknownModel 旧版（v3）只有账号级积分、无模型明细时的回填占位键。
const unknownModel = "(unknown)"

// recordUsage 累加一条观测的消耗（Observe 内部调用，调用方已持 t.mu）。
// 请求数与 token 无论 CreditOK 都计（免费层/usage 缺失的请求也证明"该号跑过"）；
// credit 只在 CreditOK 时累加——缺失≠0，与聚合 CreditSum 同口径。
func (t *Tracker) recordUsage(o Observe) {
	if o.UID == "" {
		return
	}
	if t.usage == nil {
		t.usage = map[string]map[string]map[string]*usageItem{}
	}
	day := usageDay(o.Now)
	byModel := t.usage[o.UID]
	if byModel == nil {
		byModel = map[string]map[string]*usageItem{}
		t.usage[o.UID] = byModel
	}
	byDay := byModel[day]
	if byDay == nil {
		byDay = map[string]*usageItem{}
		byModel[day] = byDay
	}
	model := o.Model
	if model == "" {
		model = unknownModel
	}
	it := byDay[model]
	if it == nil {
		it = &usageItem{}
		byDay[model] = it
	}
	if o.CreditOK {
		it.Credit += o.Credit
	}
	it.Requests++
	it.PromptTokens += o.PromptTokens
	it.CompletionTokens += o.CompletionTokens
	if o.CacheObserved {
		it.CacheHit += o.CacheHit
		it.CacheMiss += o.CacheMiss
	}
	pruneUsageLocked(t.usage, o.Now)
}

// usageDay 取本地日期键（YYYY-MM-DD）。账号侧按天聚合，时区随部署机器
// （与 scheduler 的签到窗口同一口径）。
func usageDay(t time.Time) string { return t.Format("2006-01-02") }

// pruneUsageLocked 裁剪超保留期的旧日期（调用方须已持 t.mu）。load 与
// recordUsage 双侧调用：load 兜住"重启后 flusher 尚未跑"的窗口。
func pruneUsageLocked(u map[string]map[string]map[string]*usageItem, now time.Time) {
	cutoff := usageDay(now.AddDate(0, 0, -usageRetentionDays))
	for uid, byDay := range u {
		for day := range byDay {
			if day < cutoff {
				delete(byDay, day)
			}
		}
		if len(byDay) == 0 {
			delete(u, uid)
		}
	}
}

// usageItem 单账号单日单模型的累计单元（落盘形态）。
type usageItem struct {
	Credit           float64 `json:"credit"`
	Requests         int64   `json:"requests"`
	PromptTokens     int64   `json:"prompt_tokens,omitempty"`
	CompletionTokens int64   `json:"completion_tokens,omitempty"`
	CacheHit         int64   `json:"cache_hit,omitempty"`
	CacheMiss        int64   `json:"cache_miss,omitempty"`
}

// UsageModel 单模型在某区间内的消耗与用量（详情弹窗「模型分解」行）。
type UsageModel struct {
	Model            string  `json:"model"`
	Credit           float64 `json:"credit"`
	Requests         int64   `json:"requests"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CacheHit         int64   `json:"cache_hit"`
	CacheMiss        int64   `json:"cache_miss"`
}

// UsageDay 单日消耗（趋势图 / 逐日表一行）。
type UsageDay struct {
	Day      string  `json:"day"`
	Credit   float64 `json:"credit"`
	Requests int64   `json:"requests"`
}

// AccountUsage 单账号的积分消耗视图（仪表盘「积分消耗」卡片行 + 详情弹窗数据源）。
// Nickname 由面板回填（metrics 不感知账号元数据）。
type AccountUsage struct {
	UID       string       `json:"uid"`                // 前 8 位（uidPrefix 惯例）
	Nickname  string       `json:"nickname,omitempty"` // 面板回填；查不到则空
	Today     float64      `json:"today"`              // 本地今天
	Yesterday float64      `json:"yesterday"`          // 本地昨天
	Last7d    float64      `json:"last_7d"`            // 含今天的近 7 天
	Total     float64      `json:"total"`              // 保留期内全部（30 天）
	Requests  int64        `json:"requests"`           // 保留期内请求数（含免费层）
	Daily     []UsageDay   `json:"daily,omitempty"`    // 近 7 天逐日（旧→新），卡片迷你趋势
	ByModel   []UsageModel `json:"by_model,omitempty"` // 保留期内按模型分解（积分降序）
}

// UsageSnapshot /v1/stats 的 usage 段：按账号聚合 + 全局汇总 + 全局逐日趋势。
type UsageSnapshot struct {
	Accounts  []AccountUsage `json:"accounts"`
	Today     float64        `json:"today"`
	Yesterday float64        `json:"yesterday"`
	Last7d    float64        `json:"last_7d"`
	Total     float64        `json:"total"`
	// Daily 全局逐日消耗（近 retention 天，旧→新），仪表盘 30 天趋势图数据源。
	Daily []UsageDay `json:"daily,omitempty"`
}

// buildUsage 派生按账号的消耗视图（Snapshot 持 RLock 时直接组装，不二次加锁）。
// days7 是含今天的近 7 天日期键（旧→新），用于计算 yesterday/last_7d 与卡片迷你趋势。
func buildUsage(u map[string]map[string]map[string]*usageItem, days7 []string) []AccountUsage {
	out := make([]AccountUsage, 0, len(u))
	daySet := map[string]bool{}
	for _, d := range days7 {
		daySet[d] = true
	}
	today, yesterday := days7[len(days7)-1], days7[len(days7)-2]
	for uid, byDay := range u {
		au := AccountUsage{UID: uid}
		models := map[string]*UsageModel{}
		for day, byModel := range byDay {
			in7 := daySet[day]
			for model, it := range byModel {
				au.Total += it.Credit
				au.Requests += it.Requests
				if day == today {
					au.Today += it.Credit
				}
				if day == yesterday {
					au.Yesterday += it.Credit
				}
				if in7 {
					au.Last7d += it.Credit
				}
				m := models[model]
				if m == nil {
					m = &UsageModel{Model: model}
					models[model] = m
				}
				m.Credit += it.Credit
				m.Requests += it.Requests
				m.PromptTokens += it.PromptTokens
				m.CompletionTokens += it.CompletionTokens
				m.CacheHit += it.CacheHit
				m.CacheMiss += it.CacheMiss
			}
		}
		for _, d := range days7 {
			var day UsageDay
			day.Day = d
			for _, it := range byDay[d] {
				day.Credit += it.Credit
				day.Requests += it.Requests
			}
			au.Daily = append(au.Daily, day)
		}
		au.ByModel = sortedModels(models)
		out = append(out, au)
	}
	// 排序：今日消耗降序（空则比近 7 天），同数按 UID 保证稳定。
	sort.Slice(out, func(i, j int) bool {
		if out[i].Today != out[j].Today {
			return out[i].Today > out[j].Today
		}
		if out[i].Last7d != out[j].Last7d {
			return out[i].Last7d > out[j].Last7d
		}
		return out[i].UID < out[j].UID
	})
	return out
}

// sortedModels 把模型 map 转成积分降序的切片（同数按模型名，保证面板稳定）。
func sortedModels(m map[string]*UsageModel) []UsageModel {
	out := make([]UsageModel, 0, len(m))
	for _, v := range m {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Credit != out[j].Credit {
			return out[i].Credit > out[j].Credit
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// lastNDays 返回含 now 在内的近 n 天日期键（旧→新）。
func lastNDays(now time.Time, n int) []string {
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = usageDay(now.AddDate(0, 0, -(n - 1 - i)))
	}
	return out
}

// usageSnapshot 汇总全局指标 + 按账号视图 + 全局逐日趋势（Snapshot 持 RLock 时调用）。
func (t *Tracker) usageSnapshot(now time.Time) UsageSnapshot {
	days := lastNDays(now, 7)
	accounts := buildUsage(t.usage, days)
	s := UsageSnapshot{Accounts: accounts}
	for _, au := range accounts {
		s.Today += au.Today
		s.Yesterday += au.Yesterday
		s.Last7d += au.Last7d
		s.Total += au.Total
	}
	// 全局逐日：保留期内全部天数（非仅 7 天），供 30 天趋势图。
	daysAll := lastNDays(now, usageRetentionDays)
	s.Daily = make([]UsageDay, 0, len(daysAll))
	for _, d := range daysAll {
		var day UsageDay
		day.Day = d
		for _, byDay := range t.usage {
			for _, it := range byDay[d] {
				day.Credit += it.Credit
				day.Requests += it.Requests
			}
		}
		s.Daily = append(s.Daily, day)
	}
	return s
}
