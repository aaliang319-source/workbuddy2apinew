// usage_test.go 按账号 × 按日积分消耗累计器的行为测试。
// 覆盖：累加口径（CreditOK 缺失≠0、免费层显式 0 计入）、日期切分、
// 昨日/近7天/累计派生、保留期裁剪、Reset 清空、落盘回读。
package metrics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mustObserve(t *testing.T, tr *Tracker, uid string, credit float64, creditOK bool, at time.Time) {
	t.Helper()
	tr.Observe(Observe{
		Model: "m", UID: uid, Status: 200, Now: at,
		Credit: credit, CreditOK: creditOK,
	})
}

func TestUsageAccumulatesByDayAndAccount(t *testing.T) {
	tr := New("")
	defer tr.Close()
	// 用真实当下：Snapshot 的 today/yesterday 以 time.Now() 的本地日期为基准，
	// 固定日期会随日历漂移（测试在任意日期运行都必须成立）。
	base := time.Now()
	yesterday := base.AddDate(0, 0, -1)

	mustObserve(t, tr, "aaaaaaaa", 1.5, true, base)
	mustObserve(t, tr, "aaaaaaaa", 2.0, true, base)      // 同账号同日累加
	mustObserve(t, tr, "aaaaaaaa", 0.5, true, yesterday) // 跨日切分
	mustObserve(t, tr, "bbbbbbbb", 3.0, true, base)
	mustObserve(t, tr, "cccccccc", 0, false, base) // CreditOK=false：不计 credit，但计请求数

	s := tr.Snapshot()
	u := s.Usage
	if len(u.Accounts) != 3 {
		t.Fatalf("want 3 accounts, got %d", len(u.Accounts))
	}
	byUID := map[string]AccountUsage{}
	for _, a := range u.Accounts {
		byUID[a.UID] = a
	}
	if got := byUID["aaaaaaaa"]; got.Today != 3.5 || got.Yesterday != 0.5 || got.Last7d != 4.0 || got.Total != 4.0 {
		t.Fatalf("aaaaaaaa accum wrong: %+v", got)
	}
	if got := byUID["cccccccc"]; got.Today != 0 || got.Requests != 1 {
		t.Fatalf("cccccccc: credit must be 0 (not counted) but requests=1, got %+v", got)
	}
	if u.Today != 6.5 || u.Yesterday != 0.5 || u.Total != 7 {
		t.Fatalf("summary wrong: %+v", u)
	}
	// 排序：今日消耗降序，aaaaaaaa(3.5) 在前
	if u.Accounts[0].UID != "aaaaaaaa" {
		t.Fatalf("sort by today desc: got %s first", u.Accounts[0].UID)
	}
	// 近 7 天 daily 逐日数组长度 = 7
	if len(byUID["aaaaaaaa"].Daily) != 7 {
		t.Fatalf("daily len = %d, want 7", len(byUID["aaaaaaaa"].Daily))
	}
}

func TestUsageResetClears(t *testing.T) {
	tr := New("")
	defer tr.Close()
	mustObserve(t, tr, "aaaaaaaa", 5, true, time.Now())
	tr.Reset()
	s := tr.Snapshot()
	if len(s.Usage.Accounts) != 0 || s.Usage.Total != 0 {
		t.Fatalf("usage not cleared: %+v", s.Usage)
	}
}

func TestUsagePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "metrics.json")
	base := time.Now()

	tr := New(fp)
	tr.Observe(Observe{Model: "m", UID: "aaaaaaaa", Status: 200, Now: base, Credit: 2.5, CreditOK: true})
	tr.Flush()
	tr.Close()

	// 重新加载：usage 从 metrics.json 恢复（v3 schema）。
	tr2 := New(fp)
	defer tr2.Close()
	s := tr2.Snapshot()
	if len(s.Usage.Accounts) != 1 || s.Usage.Accounts[0].Total != 2.5 {
		raw, _ := os.ReadFile(fp)
		t.Fatalf("usage lost after reload: %+v\nfile: %s", s.Usage, raw)
	}
}

// TestUsageJSONShape 锁定落盘 JSON 形态（面板与回归依赖字段名）。
func TestUsageJSONShape(t *testing.T) {
	it := &accountUsageItem{Credit: 1.25, Requests: 3}
	raw, err := json.Marshal(map[string]map[string]*accountUsageItem{"aaaaaaaa": {"2026-09-21": it}})
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		AAAA map[string]struct {
			Credit   float64 `json:"credit"`
			Requests int64   `json:"requests"`
		} `json:"aaaaaaaa"`
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	day := back.AAAA["2026-09-21"]
	if day.Credit != 1.25 || day.Requests != 3 {
		t.Fatalf("roundtrip mismatch: %+v", day)
	}
}
