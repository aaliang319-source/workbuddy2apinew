// store.go 运行历史持久化：data/automation.json（版本化 + ring + 原子写 + 5s flusher）。
//
// 与 internal/pool/persist.go、internal/metrics/persist.go 同款骨架：
// 变更标 dirty → 后台 5s 落盘 → tmp+rename 原子写（0600）→ 失败节流告警。
// 文件损坏/版本不识别一律"告警 + 空启动"，绝不阻断网关启动（历史可弃）。
package automation

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const (
	storeVersion = 1

	// maxItemsPerRun 单次运行记录保留的条目上限（超出截断并标记 Truncated）。
	maxItemsPerRun = 500
	// maxFileBytes 序列化上限：超过则从最旧 run 丢起，防止历史文件无限膨胀。
	maxFileBytes = 4 << 20
	// storeFlushInterval 后台落盘周期。
	storeFlushInterval = 5 * time.Second
	// storeLogEvery 连续落盘失败每 N 次复报一条。
	storeLogEvery = 100
)

// storeFile 落盘结构。
type storeFile struct {
	Version   int       `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
	Runs      []Run     `json:"runs"` // 旧 → 新
}

// Store 运行历史（内存 ring + 落盘）。
type Store struct {
	mu       sync.Mutex
	file     string
	maxRuns  int
	runs     []Run // 旧 → 新
	dirty    atomic.Bool
	stopCh   chan struct{}
	closeOne sync.Once

	persistFails int
}

// newStore 构造并加载历史；file 为空 = 纯内存（测试用）。
func newStore(file string, maxRuns int) *Store {
	if maxRuns <= 0 {
		maxRuns = 100
	}
	st := &Store{file: file, maxRuns: maxRuns}
	st.load()
	if file != "" {
		st.startFlusher()
	}
	return st
}

// Upsert 写入/更新一条运行记录（按 ID 原地替换，保持时间顺序）。
func (st *Store) Upsert(r Run) {
	st.mu.Lock()
	replaced := false
	for i := range st.runs {
		if st.runs[i].ID == r.ID {
			st.runs[i] = r
			replaced = true
			break
		}
	}
	if !replaced {
		st.runs = append(st.runs, r)
		// ring：超出上限丢最旧
		if len(st.runs) > st.maxRuns {
			st.runs = st.runs[len(st.runs)-st.maxRuns:]
		}
	}
	st.mu.Unlock()
	st.dirty.Store(true)
}

// List 返回最近 limit 条（新 → 旧），不带明细（summary）。
func (st *Store) List(limit int) []Run {
	st.mu.Lock()
	defer st.mu.Unlock()
	if limit <= 0 || limit > len(st.runs) {
		limit = len(st.runs)
	}
	out := make([]Run, 0, limit)
	for i := len(st.runs) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, st.runs[i].summary())
	}
	return out
}

// Get 取完整记录。
func (st *Store) Get(id string) (Run, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for i := len(st.runs) - 1; i >= 0; i-- {
		if st.runs[i].ID == id {
			return st.runs[i], true
		}
	}
	return Run{}, false
}

// LastByKind 取某类任务最近一条完整记录（status 里的 last_run）。
func (st *Store) LastByKind(kind string) (Run, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for i := len(st.runs) - 1; i >= 0; i-- {
		if st.runs[i].Kind == kind {
			return st.runs[i].summary(), true
		}
	}
	return Run{}, false
}

// Flush 同步落盘（幂等：无变更不写）。
func (st *Store) Flush() {
	st.mu.Lock()
	if st.dirty.Swap(false) {
		st.saveLocked()
	}
	st.mu.Unlock()
}

// Close 停后台 flusher 并做最后一次落盘（幂等）。
func (st *Store) Close() {
	st.closeOne.Do(func() {
		if st.stopCh != nil {
			close(st.stopCh)
		}
	})
	st.Flush()
}

// startFlusher 后台周期落盘。
func (st *Store) startFlusher() {
	st.stopCh = make(chan struct{})
	go func() {
		t := time.NewTicker(storeFlushInterval)
		defer t.Stop()
		for {
			select {
			case <-st.stopCh:
				return
			case <-t.C:
				st.Flush()
			}
		}
	}()
}

// load 读回历史；文件缺失静默跳过，解析失败/版本不符告警后空启动。
func (st *Store) load() {
	if st.file == "" {
		return
	}
	raw, err := os.ReadFile(st.file)
	if err != nil {
		return
	}
	var f storeFile
	if json.Unmarshal(raw, &f) != nil {
		log.Printf("WARN: [automation] %s 解析失败，历史空启动", st.file)
		return
	}
	if f.Version != 0 && f.Version != storeVersion {
		log.Printf("WARN: [automation] %s 版本不识别（version=%d），历史空启动", st.file, f.Version)
		return
	}
	st.runs = f.Runs
	if len(st.runs) > st.maxRuns {
		st.runs = st.runs[len(st.runs)-st.maxRuns:]
	}
}

// saveLocked 原子落盘。调用方须持锁。
func (st *Store) saveLocked() {
	if st.file == "" {
		return
	}
	runs := st.trimToSizeLocked()
	f := storeFile{Version: storeVersion, UpdatedAt: time.Now(), Runs: runs}
	raw, err := json.Marshal(&f)
	if err != nil {
		log.Printf("WARN: [automation] 历史序列化失败: %v", err)
		return
	}
	if dir := filepath.Dir(st.file); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := st.file + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		st.notePersistFail(err)
		return
	}
	if err := os.Rename(tmp, st.file); err != nil {
		st.notePersistFail(err)
		return
	}
	if st.persistFails > 0 {
		log.Printf("[automation] 历史落盘恢复（此前连续失败 %d 次）", st.persistFails)
		st.persistFails = 0
	}
}

// trimToSizeLocked 序列化超限时从最旧丢起（返回副本，不修改内存 ring）。调用方须持锁。
func (st *Store) trimToSizeLocked() []Run {
	runs := st.runs
	for len(runs) > 1 {
		raw, err := json.Marshal(runs)
		if err != nil || len(raw) <= maxFileBytes {
			return runs
		}
		runs = runs[1:]
	}
	return runs
}

// notePersistFail 落盘失败节流日志（首败详报 + 每 N 次复报）。
func (st *Store) notePersistFail(err error) {
	if st.persistFails == 0 {
		log.Printf("WARN: [automation] 历史落盘失败（首次详报）: path=%s err=%v", st.file, err)
	} else if st.persistFails%storeLogEvery == 0 {
		log.Printf("ERR: [automation] 历史连续落盘失败 %d 次: path=%s err=%v", st.persistFails, st.file, err)
	}
	st.persistFails++
}
