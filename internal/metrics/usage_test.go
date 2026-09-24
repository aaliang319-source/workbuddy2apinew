// usage_test.go 账号 × 模型 × 按日消耗累计器的行为测试。
// 覆盖：三维累加、token 三元组、模型分解、全局逐日趋势、日期切分、
// 保留期裁剪、Reset 清空、v3→v4 迁移、落盘回读。
package metrics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mustObserve(t *testing.T, tr *Tracker, o Observe) {
	t.Helper()
	if o.Status == 0 {
		o.Status = 200
	}
	tr.Observe(o)
}

func TestUsageAccumulatesByDayModelAccount(t *testing.T) {
	tr := New("")
	defer tr.Close()
	base := time.Now() // 真实当下：today/yesterday 以 time.Now() 本地日期为基准
	yesterday := base.AddDate(0, 0, -1)

	// 同账号同日同模型累加。
	mustObserve(t, tr, Observe{Model: "m1", UID: "aaaaaaaa", Now: base, Credit: 1.5, CreditOK: true,
		PromptTokens: 100, CompletionTokens: 10, CacheHit: 60, CacheMiss: 40, CacheObserved: true})
	mustObserve(t, tr, Observe{Model: "m1", UID: "aaaaaaaa", Now: base, Credit: 2.0, CreditOK: true,
		PromptTokens: 200, CompletionTokens: 20, CacheObserved: true})
	// 同账号同日不同模型。
	mustObserve(t, tr, Observe{Model: "m2", UID: "aaaaaaaa", Now: base, Credit: 0.5, CreditOK: true,
		PromptTokens: 50, CompletionTokens: 5})
	// 跨日切分。
	mustObserve(t, tr, Observe{Model: "m1", UID: "aaaaaaaa", Now: yesterday, Credit: 0.5, CreditOK: true})
	// 另一账号。
	mustObserve(t, tr, Observe{Model: "m1", UID: "bbbbbbbb", Now: base, Credit: 3.0, CreditOK: true})
	// CreditOK=false：不计 credit，但计请求数与 token。
	mustObserve(t, tr, Observe{Model: "m1", UID: "cccccccc", Now: base, Credit: 9, CreditOK: false,
		PromptTokens: 7, CompletionTokens: 3})

	s := tr.Snapshot().Usage
	if len(s.Accounts) != 3 {
		t.Fatalf("want 3 accounts, got %d", len(s.Accounts))
	}
	byUID := map[string]AccountUsage{}
	for _, a := range s.Accounts {
		byUID[a.UID] = a
	}
	aa := byUID["aaaaaaaa"]
	if aa.Today != 4.0 || aa.Yesterday != 0.5 || aa.Last7d != 4.5 || aa.Total != 4.5 {
		t.Fatalf("aaaaaaaa accum wrong: %+v", aa)
	}
	if aa.Requests != 4 {
		t.Fatalf("aaaaaaaa requests = %d, want 4", aa.Requests)
	}
	// 模型分解：m1 积分 4.0（1.5+2.0+0.5）、m2 0.5，降序。
	if len(aa.ByModel) != 2 || aa.ByModel[0].Model != "m1" || aa.ByModel[0].Credit != 4.0 {
		t.Fatalf("aaaaaaaa by_model wrong: %+v", aa.ByModel)
	}
	if aa.ByModel[0].PromptTokens != 300 || aa.ByModel[0].CompletionTokens != 30 {
		t.Fatalf("m1 tokens wrong: %+v", aa.ByModel[0])
	}
	if aa.ByModel[0].CacheHit != 60 || aa.ByModel[0].CacheMiss != 40 {
		t.Fatalf("m1 cache wrong: %+v", aa.ByModel[0])
	}
	// CreditOK=false：credit 0 但请求数与 token 可见。
	cc := byUID["cccccccc"]
	if cc.Today != 0 || cc.Requests != 1 || len(cc.ByModel) != 1 || cc.ByModel[0].PromptTokens != 7 {
		t.Fatalf("cccccccc wrong: %+v", cc)
	}
	// 全局汇总。
	if s.Today != 7.0 || s.Yesterday != 0.5 || s.Total != 7.5 {
		t.Fatalf("summary wrong: %+v", s)
	}
	// 全局逐日趋势：保留期长度（30），末位为今天。
	if len(s.Daily) != usageRetentionDays {
		t.Fatalf("global daily len = %d, want %d", len(s.Daily), usageRetentionDays)
	}
	if s.Daily[len(s.Daily)-1].Credit != 7.0 {
		t.Fatalf("today in daily = %v, want 7.0", s.Daily[len(s.Daily)-1].Credit)
	}
}

func TestUsageEmptyModelBucketedUnknown(t *testing.T) {
	tr := New("")
	defer tr.Close()
	mustObserve(t, tr, Observe{Model: "", UID: "aaaaaaaa", Now: time.Now(), Credit: 1, CreditOK: true})
	s := tr.Snapshot().Usage
	if len(s.Accounts[0].ByModel) != 1 || s.Accounts[0].ByModel[0].Model != unknownModel {
		t.Fatalf("empty model should bucket to %q: %+v", unknownModel, s.Accounts[0].ByModel)
	}
}

func TestUsageResetClears(t *testing.T) {
	tr := New("")
	defer tr.Close()
	mustObserve(t, tr, Observe{Model: "m1", UID: "aaaaaaaa", Now: time.Now(), Credit: 5, CreditOK: true})
	tr.Reset()
	s := tr.Snapshot().Usage
	if len(s.Accounts) != 0 || s.Total != 0 || len(s.Daily) != usageRetentionDays {
		t.Fatalf("usage not cleared: %+v", s)
	}
}

func TestUsagePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "metrics.json")
	base := time.Now()

	tr := New(fp)
	tr.Observe(Observe{Model: "m1", UID: "aaaaaaaa", Status: 200, Now: base,
		Credit: 2.5, CreditOK: true, PromptTokens: 123, CompletionTokens: 45})
	tr.Flush()
	tr.Close()

	tr2 := New(fp)
	defer tr2.Close()
	au := tr2.Snapshot().Usage.Accounts
	if len(au) != 1 || au[0].Total != 2.5 {
		raw, _ := os.ReadFile(fp)
		t.Fatalf("usage lost after reload: %+v\nfile: %s", au, raw)
	}
	if len(au[0].ByModel) != 1 || au[0].ByModel[0].Model != "m1" || au[0].ByModel[0].PromptTokens != 123 {
		t.Fatalf("model detail lost after reload: %+v", au[0].ByModel)
	}
}

// TestUsageMigratesV3 锁定 v3（账号×按日二维）文件迁移到 v4 三维的行为：
// 积分/请求数无损，模型维度回填 unknownModel，token 为 0。
func TestUsageMigratesV3(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "metrics.json")
	day := usageDay(time.Now())
	v3 := map[string]any{
		"version": 3,
		"since":   time.Now(),
		"models":  map[string]any{},
		"usage": map[string]any{
			"aaaaaaaa": map[string]any{
				day: map[string]any{"credit": 4.5, "requests": 3},
			},
		},
	}
	raw, _ := json.Marshal(v3)
	if err := os.WriteFile(fp, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	tr := New(fp)
	defer tr.Close()
	s := tr.Snapshot().Usage
	if len(s.Accounts) != 1 {
		t.Fatalf("v3 migration lost account: %+v", s.Accounts)
	}
	au := s.Accounts[0]
	if au.Total != 4.5 || au.Requests != 3 || au.Today != 4.5 {
		t.Fatalf("v3 migration wrong: %+v", au)
	}
	if len(au.ByModel) != 1 || au.ByModel[0].Model != unknownModel || au.ByModel[0].Credit != 4.5 {
		t.Fatalf("v3 migration model bucket wrong: %+v", au.ByModel)
	}
}

// TestUsageFutureVersionRejected 未知未来版本（v99）零状态启动，不误读。
func TestUsageFutureVersionRejected(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "metrics.json")
	_ = os.WriteFile(fp, []byte(`{"version":99,"usage":{"a":{"d":{"m":{"credit":9}}}}}`), 0o600)
	tr := New(fp)
	defer tr.Close()
	if got := tr.Snapshot().Usage.Total; got != 0 {
		t.Fatalf("future version must start zero, got %v", got)
	}
}

// TestUsageJSONShape 锁定落盘 JSON 形态（面板与回归依赖字段名）。
func TestUsageJSONShape(t *testing.T) {
	raw, err := json.Marshal(map[string]map[string]map[string]*usageItem{
		"aaaaaaaa": {"2026-09-21": {"m1": {Credit: 1.25, Requests: 3, PromptTokens: 10}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]map[string]map[string]struct {
		Credit       float64 `json:"credit"`
		Requests     int64   `json:"requests"`
		PromptTokens int64   `json:"prompt_tokens"`
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	it := back["aaaaaaaa"]["2026-09-21"]["m1"]
	if it.Credit != 1.25 || it.Requests != 3 || it.PromptTokens != 10 {
		t.Fatalf("roundtrip mismatch: %+v", it)
	}
}
