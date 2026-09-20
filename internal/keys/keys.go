// Package keys 多业务 API Key 存储：Key ↔ 账号多对多关联、优先级、启用开关，
// 热生效（管理 API 变更即落盘，无需重启）。
//
// 持久化 data/keys.json（config keys_file 可覆盖）。首次启动文件不存在时把
// config api_key（legacy 单 Key）迁移为名为 "default" 的业务 Key，保证存量
// 客户端零感知平滑升级；此后 legacy 值不再重复导入（文件为准）。
//
// 并发模型：RWMutex + byID/byVal 双索引；byVal 是派生索引，任何变更后在锁内
// 重建。落盘为原子写（tmp + rename），在持锁状态下同步执行（变更频率低，
// 文件小，同步写足够且避免后台协程生命周期问题）。
package keys

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Association Key 与账号的一条关联：uid → 优先级 + 启用开关。
type Association struct {
	UID      string `json:"uid"`
	Priority int    `json:"priority"` // 越大越优先（分层调度：先耗尽最高层）
	Enabled  bool   `json:"enabled"`
}

// KeyModel Key 级模型限制项：Name 允许使用的模型（裸名），Priority 越大越优先
// （请求模型不可用时的回退顺序；请求模型不在白名单内时改路由到优先级最高的可用项）。
type KeyModel struct {
	Name     string `json:"name"`
	Priority int    `json:"priority"`
}

// Key 一个业务 API Key。
type Key struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Value        string        `json:"value"` // sk-wb2- + 32 hex
	Enabled      bool          `json:"enabled"`
	Associations []Association `json:"associations"`
	CreatedAt    time.Time     `json:"created_at"`
	// Models 模型白名单 + 优先级（可选）。空 = 不限制（全模型可用，回退走全局
	// model_fallback 白名单）；非空 = 该 Key 只能用列表内模型，回退按 Priority 降序。
	Models []KeyModel `json:"models,omitempty"`
	// Wildcard 通配标记：true 时未关联账号也可用全池（不过滤账号）。
	// 仅首启从 legacy api_key 迁移的 default Key 置位——存量客户端零感知；
	// 新建 Key 不允许通配（未关联账号即 403 key_not_provisioned，必须先关联）。
	Wildcard bool `json:"wildcard,omitempty"`
}

// file 落盘结构（带版本号，向前兼容）。
type file struct {
	Version int    `json:"version"`
	Keys    []*Key `json:"keys"`
}

const fileVersion = 1

// Store Key 存储。零值不可用，须经 Load 构建。
type Store struct {
	mu    sync.RWMutex
	fp    string          // keys.json 路径
	byID  map[string]*Key // 主索引
	byVal map[string]*Key // 派生索引（value → key），变更后重建
}

// Load 从 fp 加载；文件不存在时若 legacyAPIKey 非空则迁移为 default Key 并
// 立即落盘。legacy 为空且文件缺失 → 空 Store。
func Load(fp, legacyAPIKey string) (*Store, error) {
	s := &Store{
		fp:    fp,
		byID:  make(map[string]*Key),
		byVal: make(map[string]*Key),
	}
	raw, err := os.ReadFile(fp)
	if err == nil {
		var f file
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("parse %s: %w", fp, err)
		}
		for _, k := range f.Keys {
			if k == nil || k.ID == "" || k.Value == "" {
				continue // 容错：跳过残缺条目
			}
			s.byID[k.ID] = k
		}
		s.rebuildValueIndexLocked()
		return s, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", fp, err)
	}
	// 首次运行：迁移 legacy api_key 为 default 业务 Key（存量客户端零感知）。
	// 迁移 Key 带 Wildcard 标记（未关联 = 全池通配），保持 legacy 语义不变。
	if legacyAPIKey != "" {
		k := &Key{
			ID:        newID(),
			Name:      "default",
			Value:     legacyAPIKey,
			Enabled:   true,
			CreatedAt: time.Now(),
			Wildcard:  true,
		}
		s.byID[k.ID] = k
		s.rebuildValueIndexLocked()
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Auth 按 value 查找启用中的 Key。未命中或已禁用均返回 false。
func (s *Store) Auth(value string) (*Key, bool) {
	k, ok := s.Lookup(value)
	if !ok || !k.Enabled {
		return nil, false
	}
	return k, true
}

// Lookup 按 value 查找 Key（无论启用与否）。供鉴权层区分"禁用"与"不存在"。
func (s *Store) Lookup(value string) (*Key, bool) {
	if value == "" {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.byVal[value]
	if !ok {
		return nil, false
	}
	return k, true
}

// AllowedUIDs 返回该 Key 所有启用关联的 uid→priority。无关联/Key 不存在返回空表
// （调用方据此拒绝：未关联账号的 Key 不可用）。
func (s *Store) AllowedUIDs(keyID string) map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.byID[keyID]
	if !ok {
		return nil
	}
	out := make(map[string]int, len(k.Associations))
	for _, a := range k.Associations {
		if a.Enabled {
			out[a.UID] = a.Priority
		}
	}
	return out
}

// ModelsSorted 返回按 Priority 降序的模型白名单（Priority 相同按名字稳定排序）。
func (k *Key) ModelsSorted() []KeyModel {
	out := make([]KeyModel, len(k.Models))
	copy(out, k.Models)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// ContainsModel 报告模型是否在 Key 的白名单内（白名单为空 = 不限制，恒 true）。
func (k *Key) ContainsModel(name string) bool {
	if len(k.Models) == 0 {
		return true
	}
	for _, m := range k.Models {
		if m.Name == name {
			return true
		}
	}
	return false
}

// List 返回全部 Key（含完整 value——面板可信内网场景需要生成深链）。
// 按 CreatedAt 升序，保证列表稳定。
func (s *Store) List() []Key {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Key, 0, len(s.byID))
	for _, k := range s.byID {
		out = append(out, *k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Get 按 ID 查单个 Key。
func (s *Store) Get(id string) (Key, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.byID[id]
	if !ok {
		return Key{}, false
	}
	return *k, true
}

// Create 新建 Key（自动生成 value），返回创建结果。
func (s *Store) Create(name string) (Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := &Key{
		ID:        newID(),
		Name:      name,
		Value:     newValue(),
		Enabled:   true,
		CreatedAt: time.Now(),
	}
	s.byID[k.ID] = k
	s.rebuildValueIndexLocked()
	if err := s.saveLocked(); err != nil {
		delete(s.byID, k.ID)
		s.rebuildValueIndexLocked()
		return Key{}, err
	}
	return *k, nil
}

// Update 更新 Key（fn 内修改副本字段），落盘失败回滚内存。
func (s *Store) Update(id string, fn func(*Key) error) (Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.byID[id]
	if !ok {
		return Key{}, ErrNotFound
	}
	backup := *k
	if err := fn(k); err != nil {
		*k = backup
		return Key{}, err
	}
	s.rebuildValueIndexLocked()
	if err := s.saveLocked(); err != nil {
		*k = backup
		s.rebuildValueIndexLocked()
		return Key{}, err
	}
	return *k, nil
}

// Delete 删除 Key。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[id]; !ok {
		return ErrNotFound
	}
	backup := *s.byID[id]
	delete(s.byID, id)
	s.rebuildValueIndexLocked()
	if err := s.saveLocked(); err != nil {
		s.byID[backup.ID] = &backup
		s.rebuildValueIndexLocked()
		return err
	}
	return nil
}

// Regenerate 重置 Key 的 value（旧 value 立即失效）。
func (s *Store) Regenerate(id string) (Key, error) {
	return s.Update(id, func(k *Key) error { k.Value = newValue(); return nil })
}

// ErrNotFound Key 不存在。
var ErrNotFound = errors.New("key not found")

// rebuildValueIndexLocked 重建 value→key 派生索引。须持写锁。
func (s *Store) rebuildValueIndexLocked() {
	s.byVal = make(map[string]*Key, len(s.byID))
	for _, k := range s.byID {
		s.byVal[k.Value] = k
	}
}

// saveLocked 原子落盘（tmp + rename）。须持写锁。
func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.fp), 0o755); err != nil {
		return fmt.Errorf("mkdir keys dir: %w", err)
	}
	f := file{Version: fileVersion, Keys: make([]*Key, 0, len(s.byID))}
	for _, k := range s.byID {
		f.Keys = append(f.Keys, k)
	}
	raw, err := json.MarshalIndent(&f, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.fp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write keys tmp: %w", err)
	}
	if err := os.Rename(tmp, s.fp); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename keys file: %w", err)
	}
	return nil
}

// newID 8 字节 hex 短 ID。
func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// newValue 生成 "sk-wb2-" + 16 字节 hex（32 字符）。
func newValue() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "sk-wb2-" + hex.EncodeToString(b)
}
