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

// metricsVersion 落盘 schema 版本。v3：新增 Usage 按账号 × 按日积分消耗；
// v2：新增 Recent 单条请求明细（per-request 环形缓冲）。
// 旧版本文件容忍加载：v1/v2 的 Usage 为空（从加载时刻起逐步回填）。
const metricsVersion = 3

var flushInterval = 5 * time.Second

// persistLogEvery 连续落盘失败每 N 次复报一条（flusher 5s 一把），避免刷屏。
const persistLogEvery = 100

// metricsFile 落盘形态：只有和与计数（ModelAccum）+ 最近请求明细（Recent）+
// 按账号×按日消耗（Usage），派生值不落盘。
type metricsFile struct {
	Version int                                      `json:"version"`
	Since   time.Time                                `json:"since"`
	Models  map[string]ModelAccum                    `json:"models"`
	Recent  []RequestRecord                          `json:"recent,omitempty"`
	Usage   map[string]map[string]*accountUsageItem  `json:"usage,omitempty"`
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
	var mf metricsFile
	if json.Unmarshal(raw, &mf) != nil {
		log.Printf("WARN: [metrics] %s 解析失败，零状态启动", t.file)
		return
	}
	// version 容忍矩阵：缺失（0）= 手编文件按当前版处理；v1 = 历史聚合保留
	// （Recent 为空，重启后逐步回填）；仅 > 当前版本的未知未来版才拒绝
	//（防止新版文件被旧二进制误读产生字段语义漂移）。
	if mf.Version > metricsVersion {
		log.Printf("WARN: [metrics] %s 版本不识别（version=%d），零状态启动", t.file, mf.Version)
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !mf.Since.IsZero() {
		t.since = mf.Since
	}
	for m, a := range mf.Models {
		acc := a // 拷贝，避免 range 变量别名
		t.models[m] = &acc
	}
	// v1 文件无 Recent（nil），零值加载即正确；v2 文件恢复明细并保持时间升序。
	if len(mf.Recent) > recentCap {
		mf.Recent = mf.Recent[len(mf.Recent)-recentCap:]
	}
	t.recent = mf.Recent
	// v1/v2 文件无 Usage（nil），零值加载即正确；v3 恢复并按保留期裁剪。
	if mf.Usage != nil {
		t.usage = mf.Usage
		pruneUsageLocked(t.usage, time.Now())
	}
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
