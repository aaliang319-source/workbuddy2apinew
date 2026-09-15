// recent_test.go 单条请求明细（recent 环形缓冲）测试：
// 追加/淘汰/排序/Reset/持久化往返/v1 文件容忍。
package metrics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// obsAt 构造带指定时刻的观测（区别于 metrics_test.go 的 obs：这里要控制时间序）。
func obsAt(model string, status int, now time.Time) Observe {
	return Observe{Model: model, Status: status, Now: now, Wall: 10 * time.Millisecond}
}

// TestRecentAppendAndOrder 明细按新→旧输出；Snapshot 不改动内部升序。
func TestRecentAppendAndOrder(t *testing.T) {
	tr := New("")
	base := time.Unix(1758000000, 0)
	for i := 0; i < 3; i++ {
		tr.Observe(obsAt("m1", 200, base.Add(time.Duration(i)*time.Second)))
	}
	got := tr.Snapshot().Recent
	if len(got) != 3 {
		t.Fatalf("len=%d want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Time <= got[i].Time {
			t.Errorf("not newest-first: [%d]=%s [%d]=%s", i-1, got[i-1].Time, i, got[i].Time)
		}
	}
	// 再取一次快照：顺序稳定（Snapshot 不翻转内部存储）。
	if again := tr.Snapshot().Recent; again[0].Time != got[0].Time {
		t.Errorf("snapshot unstable: %s vs %s", again[0].Time, got[0].Time)
	}
}

// TestRecentEvict 超过 recentCap 淘汰最旧。
func TestRecentEvict(t *testing.T) {
	tr := New("")
	base := time.Unix(1758000000, 0)
	for i := 0; i < recentCap+10; i++ {
		tr.Observe(obsAt("m1", 200, base.Add(time.Duration(i)*time.Second)))
	}
	got := tr.Snapshot().Recent
	if len(got) != recentCap {
		t.Fatalf("len=%d want %d", len(got), recentCap)
	}
	// Snapshot 是新→旧：got[0] 最新（第 total-1 条），got 尾部是最旧幸存记录。
	total := recentCap + 10
	newest, _ := time.Parse(time.RFC3339, got[0].Time)
	oldest, _ := time.Parse(time.RFC3339, got[len(got)-1].Time)
	if wantNewest := base.Add(time.Duration(total-1) * time.Second); !newest.Equal(wantNewest) {
		t.Errorf("newest=%s want %s", newest, wantNewest)
	}
	if wantOldest := base.Add(time.Duration(total-recentCap) * time.Second); !oldest.Equal(wantOldest) {
		t.Errorf("oldest surviving=%s want %s", oldest, wantOldest)
	}
}

// TestRecentFields 观测字段逐项落进记录（credit 仅显式观测记录、err 透传、
// proto/key/tries 诊断维度透传）。
func TestRecentFields(t *testing.T) {
	tr := New("")
	now := time.Unix(1758000000, 0)
	tr.Observe(Observe{
		Model: "glm-5.3", Stream: true, Status: 200, TTFB: 1500 * time.Microsecond,
		Wall: 2 * time.Second, PromptTokens: 42, CompletionTokens: 7,
		CacheHit: 30, CacheMiss: 12, CacheObserved: true,
		Credit: 0.05, CreditOK: true, Now: now,
		Proto: "openai", KeyName: "default", Tries: 2,
	})
	tr.Observe(Observe{Model: "glm-5.3", Status: 402, Wall: 500 * time.Millisecond, Err: "hard_credit(402)", Now: now.Add(time.Second)})
	got := tr.Snapshot().Recent
	if len(got) != 2 {
		t.Fatalf("len=%d want 2", len(got))
	}
	latest, older := got[0], got[1]
	if latest.Err != "hard_credit(402)" || latest.Status != 402 || latest.Stream {
		t.Errorf("failed record=%+v", latest)
	}
	// 失败记录未设置诊断维度：零值 + omitempty 不出现。
	if latest.Proto != "" || latest.KeyName != "" || latest.Tries != 0 {
		t.Errorf("failed record diag fields should be zero: %+v", latest)
	}
	if older.TTFBMS != 1 || older.WallMS != 2000 || older.PromptTokens != 42 ||
		older.CompletionTokens != 7 || older.CacheHit != 30 || older.CacheMiss != 12 {
		t.Errorf("success record=%+v", older)
	}
	if older.Credit != 0.05 {
		t.Errorf("credit=%v want 0.05（显式观测）", older.Credit)
	}
	if older.Proto != "openai" || older.KeyName != "default" || older.Tries != 2 {
		t.Errorf("diag fields=%s/%s/%d want openai/default/2", older.Proto, older.KeyName, older.Tries)
	}
	if latest.Credit != 0 {
		t.Errorf("credit=%v want 0（未观测，缺失≠0）", latest.Credit)
	}
}

// TestRecentResetWithStats Reset 清空明细（与聚合同语义）。
func TestRecentResetWithStats(t *testing.T) {
	tr := New("")
	tr.Observe(obsAt("m1", 200, time.Unix(1758000000, 0)))
	tr.Reset()
	if got := tr.Snapshot().Recent; len(got) != 0 {
		t.Errorf("after reset len=%d want 0", len(got))
	}
}

// TestRecentPersistRoundTrip 落盘→重载后明细保留且顺序不变。
func TestRecentPersistRoundTrip(t *testing.T) {
	file := filepath.Join(t.TempDir(), "metrics.json")
	tr := New(file)
	base := time.Unix(1758000000, 0)
	for i := 0; i < 5; i++ {
		tr.Observe(obsAt("m1", 200, base.Add(time.Duration(i)*time.Second)))
	}
	tr.Flush()
	want := tr.Snapshot().Recent

	tr2 := New(file)
	got := tr2.Snapshot().Recent
	if len(got) != len(want) {
		t.Fatalf("reload len=%d want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("record[%d] mismatch: %+v vs %+v", i, got[i], want[i])
		}
	}
}

// TestLoadV1Tolerated v1 旧文件（无 Recent）：聚合历史保留、明细为空、可继续写入。
func TestLoadV1Tolerated(t *testing.T) {
	file := filepath.Join(t.TempDir(), "metrics.json")
	v1 := map[string]any{
		"version": 1,
		"since":   time.Unix(1758000000, 0).Format(time.RFC3339),
		"models": map[string]any{
			"glm-5.3": map[string]any{"requests": 7, "success": 5, "failed": 2},
		},
	}
	raw, _ := json.Marshal(v1)
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	tr := New(file)
	snap := tr.Snapshot()
	if len(snap.Models) != 1 || snap.Models[0].Requests != 7 {
		t.Fatalf("v1 aggregates lost: %+v", snap.Models)
	}
	if len(snap.Recent) != 0 {
		t.Errorf("v1 recent=%d want 0", len(snap.Recent))
	}
	// 继续写入：明细与聚合共存。
	tr.Observe(obsAt("glm-5.3", 200, time.Unix(1758000100, 0)))
	tr.Flush()
	if got := tr.Snapshot().Recent; len(got) != 1 {
		t.Errorf("after write recent=%d want 1", len(got))
	}
}

// TestLoadV3Rejected 未来版本拒绝（防字段语义漂移）。
func TestLoadV3Rejected(t *testing.T) {
	file := filepath.Join(t.TempDir(), "metrics.json")
	if err := os.WriteFile(file, []byte(`{"version":99,"since":"2026-01-01T00:00:00Z","models":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tr := New(file)
	snap := tr.Snapshot()
	if len(snap.Models) != 0 {
		t.Errorf("v99 should start zero state, got models=%d", len(snap.Models))
	}
}
