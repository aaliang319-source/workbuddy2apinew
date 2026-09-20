package keys

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestLoadMigratesLegacyKey(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "keys.json")
	s, err := Load(fp, "legacy-secret")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	ks := s.List()
	if len(ks) != 1 {
		t.Fatalf("want 1 migrated key, got %d", len(ks))
	}
	if ks[0].Name != "default" || ks[0].Value != "legacy-secret" || !ks[0].Enabled {
		t.Fatalf("migrated key wrong: %+v", ks[0])
	}
	// 迁移结果立即落盘。
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("keys.json not written: %v", err)
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil || len(f.Keys) != 1 {
		t.Fatalf("file content wrong: err=%v keys=%d", err, len(f.Keys))
	}
	// 再次加载：文件为准，legacy 不重复导入。
	s2, err := Load(fp, "another-legacy")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := len(s2.List()); got != 1 {
		t.Fatalf("reload want 1 key, got %d", got)
	}
	if _, ok := s2.Auth("another-legacy"); ok {
		t.Fatal("second legacy must not become a key")
	}
}

func TestLoadNoLegacyEmptyStore(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "keys.json")
	s, err := Load(fp, "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(s.List()) != 0 {
		t.Fatal("want empty store")
	}
	if _, err := os.Stat(fp); !os.IsNotExist(err) {
		t.Fatal("empty store must not write file")
	}
}

func TestLoadMalformedJSON(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "keys.json")
	if err := os.WriteFile(fp, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(fp, "x"); err == nil {
		t.Fatal("want error for malformed json")
	}
}

func TestCRUDAndAuth(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "keys.json"), "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	k, err := s.Create("team-a")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(k.Value) != len("sk-wb2-")+32 {
		t.Fatalf("value format: %q", k.Value)
	}
	// 无关联：Auth 通过但 AllowedUIDs 为空（调用方按通配处理，不过滤账号）。
	if _, ok := s.Auth(k.Value); !ok {
		t.Fatal("auth newly created key")
	}
	if got := s.AllowedUIDs(k.ID); len(got) != 0 {
		t.Fatalf("want empty allowed, got %v", got)
	}
	// 更新关联。
	if _, err := s.Update(k.ID, func(x *Key) error {
		x.Associations = []Association{
			{UID: "u1", Priority: 10, Enabled: true},
			{UID: "u2", Priority: 5, Enabled: false},
		}
		return nil
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	allowed := s.AllowedUIDs(k.ID)
	if len(allowed) != 1 || allowed["u1"] != 10 {
		t.Fatalf("allowed wrong: %v", allowed)
	}
	// 禁用后 Auth 拒绝。
	if _, err := s.Update(k.ID, func(x *Key) error { x.Enabled = false; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Auth(k.Value); ok {
		t.Fatal("disabled key must not auth")
	}
	// Regenerate 换 value。
	old := k.Value
	nk, err := s.Regenerate(k.ID)
	if err != nil {
		t.Fatalf("regen: %v", err)
	}
	if nk.Value == old {
		t.Fatal("value must change")
	}
	if _, ok := s.Auth(old); ok {
		t.Fatal("old value must be invalid after regenerate")
	}
	// Delete。
	if err := s.Delete(k.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := s.Auth(nk.Value); ok {
		t.Fatal("deleted key must not auth")
	}
	if err := s.Delete("nope"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestConcurrentCRUDAndAuth(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "keys.json"), "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	k, _ := s.Create("race")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				switch (i + j) % 4 {
				case 0:
					_, _ = s.Create("c")
				case 1:
					_, _ = s.Auth(k.Value)
				case 2:
					_, _ = s.Update(k.ID, func(x *Key) error { return nil })
				case 3:
					_ = s.AllowedUIDs(k.ID)
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestKeyModelsSortedAndContains(t *testing.T) {
	k := Key{Models: []KeyModel{
		{Name: "glm-5.3-flash", Priority: 50},
		{Name: "deepseek-v4.1-flash", Priority: 100},
		{Name: "auto", Priority: 100},
	}}
	sorted := k.ModelsSorted()
	// 同优先级按名字稳定排序（auto < deepseek…）
	if sorted[0].Name != "auto" || sorted[1].Name != "deepseek-v4.1-flash" || sorted[2].Name != "glm-5.3-flash" {
		t.Fatalf("sort wrong: %+v", sorted)
	}
	if !k.ContainsModel("auto") || k.ContainsModel("glm-5.3") {
		t.Fatalf("contains wrong")
	}
	// 空白名单 = 不限制
	empty := Key{}
	if !empty.ContainsModel("anything") {
		t.Fatal("empty whitelist must allow all")
	}
	if len(empty.ModelsSorted()) != 0 {
		t.Fatal("empty sort")
	}
}
