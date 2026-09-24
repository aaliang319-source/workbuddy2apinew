// 持久化：data/metrics.json 落盘/加载 + 后台 flusher。
// 骨架与 internal/pool/persist.go 同款（dirty 标记 + 周期 flusher + tmp/rename 原子写）。
package metrics

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"
)

// metricsVersion 落盘 schema 版本。v4：Usage 升级为账号 × 模型 × 按日（三维，
// 含 token 三元组）；v3：Usage 账号 × 按日积分消耗；v2：Recent 单条请求明细。
// 旧版本文件容忍加载：v1/v2 的 Usage 为空（从加载时刻起逐步回填）；
// v3 的账号级积分平移为 v4 三维（模型标 unknownModel，token 缺失为 0）。
const metricsVersion = 4

var flushInterval = 5 * time.Second

// persistLogEvery 连续落盘失败每 N 次复报一条（flusher 5s 一把），避免刷屏。
const persistLogEvery = 100

// metricsFile 落盘形态：只有和与计数（ModelAccum）+ 最近请求明细（Recent）+
// 账号×模型×按日消耗（Usage），派生值不落盘。
type metricsFile struct {
	Version int                                            `json:"version"`
	Since   time.Time                                      `json:"since"`
	Models  map[string]ModelAccum                          `json:"models"`
	Recent  []RequestRecord                                `json:"recent,omitempty"`
	Usage   map[string]map[string]map[string]*usageItem    `json:"usage,omitempty"`
}

// metricsFileV3 v3 落盘形态（账号×按日二维），仅供旧文件迁移读取。
type metricsFileV3 struct {
	Version int                                 `json:"version"`
	Since   time.Time                           `json:"since"`
	Models  map[string]ModelAccum               `json:"models"`
	Recent  []RequestRecord                     `json:"recent,omitempty"`
	Usage   map[string]map[string]*usageItemV3  `json:"usage,omitempty"`
}

// usageItemV3 v3 的单账号单日累计单元（无模型/token 维度）。
type usageItemV3 struct {
	Credit   float64 `json:"credit"`
	Requests int64   `json:"requests"`
}

// startFlusher 启动后台周期落盘 goroutine（每 flushInterval 检查 dirty），
// Close 关闭 stopCh 时退出。
func (t *Tracker) startFlusher() {
	interval := flushInterval // 启动前同步读取，避免与测试改 interval 竞争
	t.stopCh = make(chan struct{})
	go func() {
		tk := time.NewTicker(interval)
		defer tk.Stop()
		for {
			select {
			case <-t.stopCh:
				return
			case <-tk.C:
				t.mu.Lock()
				if t.dirty.Swap(false) {
					t.saveLocked()
				}
				t.mu.Unlock()
			}
		}
	}()
}

// load 从 metrics.json 读回状态。文件不存在静默跳过（首次启动）；
// 解析失败或版本不识别打一条 WARN 后零状态启动（统计可弃，不阻断网关）。
func (t *Tracker) load() {
	if t.file == "" {
		return
	}
	raw, err := os.ReadFile(t.file)
	if err != nil {
		return
	}
	// 先探测版本：v3 的 Usage 是二维结构，字段与 v4 不兼容，需走独立迁移路径
	// （直接 Unmarshal 到 v4 结构会把 v3 的 {credit,requests} 读成 v4 的
	//  day→model 映射，产生语义漂移）。
	var probe struct {
		Version int `json:"version"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		log.Printf("WARN: [metrics] %s 解析失败，零状态启动", t.file)
		return
	}
	if probe.Version > metricsVersion {
		log.Printf("WARN: [metrics] %s 版本不识别（version=%d），零状态启动", t.file, probe.Version)
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if probe.Version == 3 {
		var mf3 metricsFileV3
		if json.Unmarshal(raw, &mf3) != nil {
			log.Printf("WARN: [metrics] %s v3 解析失败，零状态启动", t.file)
			return
		}
		t.loadCommon(mf3.Since, mf3.Models, mf3.Recent)
		t.usage = migrateUsageV3(mf3.Usage)
		pruneUsageLocked(t.usage, time.Now())
		return
	}
	// v0（缺失/手编）、v1、v2、v4：主结构兼容，直接反序列化（v1/v2 无 Usage 字段）。
	var mf metricsFile
	if json.Unmarshal(raw, &mf) != nil {
		log.Printf("WARN: [metrics] %s 解析失败，零状态启动", t.file)
		return
	}
	t.loadCommon(mf.Since, mf.Models, mf.Recent)
	if mf.Usage != nil {
		t.usage = mf.Usage
		pruneUsageLocked(t.usage, time.Now())
	}
}

// loadCommon 载入各版本共有的字段（since / models / recent，调用方须持 t.mu）。
func (t *Tracker) loadCommon(since time.Time, models map[string]ModelAccum, recent []RequestRecord) {
	if !since.IsZero() {
		t.since = since
	}
	for m, a := range models {
		acc := a // 拷贝，避免 range 变量别名
		t.models[m] = &acc
	}
	// v1 文件无 Recent（nil），零值加载即正确；v2+ 恢复明细并保持时间升序。
	if len(recent) > recentCap {
		recent = recent[len(recent)-recentCap:]
	}
	t.recent = recent
}

// migrateUsageV3 把 v3 的账号×按日积分迁移为 v4 的账号×模型×按日结构：
// 模型维度缺失，统一回填 unknownModel 占位（面板展示为"(unknown)"），token 为 0。
// 积分与请求数无损保留，历史趋势不断档。
func migrateUsageV3(in map[string]map[string]*usageItemV3) map[string]map[string]map[string]*usageItem {
	if in == nil {
		return nil
	}
	out := make(map[string]map[string]map[string]*usageItem, len(in))
	for uid, byDay := range in {
		days := make(map[string]map[string]*usageItem, len(byDay))
		for day, it := range byDay {
			days[day] = map[string]*usageItem{
				unknownModel: {Credit: it.Credit, Requests: it.Requests},
			}
		}
		out[uid] = days
	}
	return out
}

// saveLocked 原子落盘（tmp + rename）。调用方必须已持 t.mu。
func (t *Tracker) saveLocked() {
	if t.file == "" {
		return
	}
	mf := metricsFile{Version: metricsVersion, Since: t.since, Models: map[string]ModelAccum{}, Recent: t.recent, Usage: t.usage}
	for m, a := range t.models {
		mf.Models[m] = *a
	}
	raw, err := json.MarshalIndent(mf, "", "  ")
	if err != nil {
		log.Printf("WARN: [metrics] 统计序列化失败: %v", err)
		return
	}
	if dir := filepath.Dir(t.file); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := t.file + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		t.notePersistFail(err)
		return
	}
	if err := os.Rename(tmp, t.file); err != nil {
		t.notePersistFail(err)
		return
	}
	if t.persistFails > 0 {
		log.Printf("[metrics] 统计落盘恢复（此前连续失败 %d 次）", t.persistFails)
		t.persistFails = 0
	}
}

// notePersistFail 落盘失败节流日志：首败详报（含路径与 err），此后每
// persistLogEvery 次复报一条。统计落盘失败不影响请求路径，WARN 即可。
func (t *Tracker) notePersistFail(err error) {
	if t.persistFails == 0 {
		log.Printf("WARN: [metrics] 统计落盘失败（首次详报）: path=%s err=%v", t.file, err)
	} else if t.persistFails%persistLogEvery == 0 {
		log.Printf("ERR: [metrics] 统计连续落盘失败 %d 次: path=%s err=%v", t.persistFails, t.file, err)
	}
	t.persistFails++
}
