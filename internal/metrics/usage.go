// usage.go 按账号 × 按日的积分消耗累计器（面板仪表盘「积分消耗」卡片数据源）。
//
// 与 per-model 聚合（tracker.go）互补：模型视角看成本构成，账号视角看每个号
// 每天烧了多少。数据形态是 map[uidPrefix]map[date]creditSum，随 metrics.json
// 落盘（同 flusher / 同 dirty 标记），重启不丢；保留 usageRetentionDays 天，
// load 与 Observe 时双侧裁剪。
//
// 账号主键沿用 uidPrefix 惯例（UID 前 8 位）：明细表同款脱敏口径，仪表盘
// 不需要完整 UID，前 8 位足以与「账号管理」页对上。
package metrics

import (
	"sort"
	"time"
)

// usageRetentionDays 按日消耗的保留天数：仪表盘最多展示近 7 天 + 昨日对比，
// 30 天留足余量且把 metrics.json 体积增长限制在 KB 级。
const usageRetentionDays = 30

// recordUsage 累加一条观测的积分消耗（Observe 内部调用，调用方已持 t.mu）。
// 请求数无论 CreditOK 都计（消耗为 0 的免费层请求也证明"该号跑过"）；
// credit 只在 CreditOK 时累加——缺失≠0，与聚合 CreditSum 同口径。
func (t *Tracker) recordUsage(o Observe) {
	if o.UID == "" {
		return
	}
	if t.usage == nil {
		t.usage = map[string]map[string]*accountUsageItem{}
	}
	day := usageDay(o.Now)
	m := t.usage[o.UID]
	if m == nil {
		m = map[string]*accountUsageItem{}
		t.usage[o.UID] = m
	}
	it := m[day]
	if it == nil {
		it = &accountUsageItem{}
		m[day] = it
	}
	if o.CreditOK {
		it.Credit += o.Credit
	}
	it.Requests++
	pruneUsageLocked(t.usage, o.Now)
}

// usageDay 取本地日期键（YYYY-MM-DD）。账号侧按天聚合，时区随部署机器
// （与 scheduler 的签到窗口同一口径）。
func usageDay(t time.Time) string { return t.Format("2006-01-02") }

// pruneUsageLocked 裁剪超保留期的旧日期（调用方须已持 t.mu）。load 与
// recordUsage 双侧调用：load 兜住"重启后 flusher 尚未跑"的窗口。
func pruneUsageLocked(u map[string]map[string]*accountUsageItem, now time.Time) {
	cutoff := usageDay(now.AddDate(0, 0, -usageRetentionDays))
	for uid, m := range u {
		for day := range m {
			if day < cutoff {
				delete(m, day)
			}
		}
		if len(m) == 0 {
			delete(u, uid)
		}
	}
}

// accountUsageItem 单账号单日的累计单元（落盘形态）。
type accountUsageItem struct {
	Credit   float64 `json:"credit"`
	Requests int64   `json:"requests"`
}

// AccountUsage 单账号的积分消耗视图（仪表盘「积分消耗」卡片行数据源）。
// Nickname 由 handler 从 pool.List 回填（metrics 不感知账号元数据）。
type AccountUsage struct {
	UID       string     `json:"uid"`                // 前 8 位（uidPrefix 惯例）
	Nickname  string     `json:"nickname,omitempty"` // handler 回填；查不到则空
	Today     float64    `json:"today"`              // 本地今天
	Yesterday float64    `json:"yesterday"`          // 本地昨天
	Last7d    float64    `json:"last_7d"`            // 含今天的近 7 天
	Total     float64    `json:"total"`              // 保留期内全部（30 天）
	Requests  int64      `json:"requests"`           // 保留期内请求数（含免费层）
	Daily     []UsageDay `json:"daily,omitempty"`    // 近 7 天逐日（旧→新），供迷你趋势
}

// UsageDay 单日消耗（旧→新排列）。
type UsageDay struct {
	Day      string  `json:"day"`
	Credit   float64 `json:"credit"`
	Requests int64   `json:"requests"`
}

// UsageSnapshot /v1/stats 的 usage 段：按账号聚合 + 全局汇总。
type UsageSnapshot struct {
	Accounts []AccountUsage `json:"accounts"`
	Today    float64        `json:"today"`
	Yesterday float64       `json:"yesterday"`
	Last7d   float64        `json:"last_7d"`
	Total    float64        `json:"total"`
}

// buildUsage 派生按账号 × 按日的消耗视图（Snapshot 持 RLock 时直接组装，不二次加锁）。
// days 是含今天的近 7 天日期键（旧→新）；倒数第 1 个 = 今天，倒数第 2 个 = 昨天。
func buildUsage(u map[string]map[string]*accountUsageItem, days []string) []AccountUsage {
	out := make([]AccountUsage, 0, len(u))
	daySet := map[string]int{}
	for i, d := range days {
		daySet[d] = i
	}
	for uid, m := range u {
		au := AccountUsage{UID: uid}
		for day, it := range m {
			au.Total += it.Credit
			au.Requests += it.Requests
			switch day {
			case days[len(days)-1]:
				au.Today = it.Credit
			case days[len(days)-2]:
				au.Yesterday = it.Credit
			}
			if _, ok := daySet[day]; ok {
				au.Last7d += it.Credit
			}
		}
		for _, d := range days {
			it := m[d]
			du := UsageDay{Day: d}
			if it != nil {
				du.Credit, du.Requests = it.Credit, it.Requests
			}
			au.Daily = append(au.Daily, du)
		}
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

// lastNDays 返回含 today 在内的近 n 天日期键（旧→新）。
func lastNDays(now time.Time, n int) []string {
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = usageDay(now.AddDate(0, 0, -(n - 1 - i)))
	}
	return out
}

// usageSnapshot 汇总全局指标 + 按账号视图（Snapshot 持 RLock 时调用）。
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
	return s
}
