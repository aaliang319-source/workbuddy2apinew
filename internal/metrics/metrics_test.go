package metrics

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// obs 构造便利：填固定值，测试按需覆盖。
func obs(model string, status int, stream bool, ttfb, wall time.Duration) Observe {
	return Observe{
		Model: model, Status: status, Stream: stream,
		TTFB: ttfb, Wall: wall,
		PromptTokens: 10, CompletionTokens: 100,
		Now: time.Now(),
	}
}

func TestObserveAggregation(t *testing.T) {
	tr := New("")
	tr.Observe(func() Observe { o := obs("m", 200, true, 100*time.Millisecond, 1*time.Second); return o }())
	tr.Observe(func() Observe {
		o := obs("m", 200, true, 300*time.Millisecond, 2*time.Second)
		o.CompletionTokens = 200
		return o
	}())
	tr.Observe(func() Observe { o := obs("m", 502, false, 0, 3*time.Second); return o }())

	s := tr.Snapshot()
	if s.Models[0].Model != "m" {
		t.Fatalf("model=%q want m", s.Models[0].Model)
	}
	m := s.Models[0]
	if m.Requests != 3 || m.Success != 2 || m.Failed != 1 || m.Streaming != 2 {
		t.Errorf("counts req=%d ok=%d fail=%d stream=%d, want 3/2/1/2", m.Requests, m.Success, m.Failed, m.Streaming)
	}
	// avg_ttfb 分母 = 有首帧的流式请求数（2），非流式不摊薄：(100+300)/2 = 200ms。
	if m.AvgTTFBMS != 200 {
		t.Errorf("avg_ttfb_ms=%v want 200", m.AvgTTFBMS)
	}
	// avg_latency 分母 = 全部请求：(1000+2000+3000)/3 = 2000ms。
	if m.AvgLatencyMS != 2000 {
		t.Errorf("avg_latency_ms=%v want 2000", m.AvgLatencyMS)
	}
	// tokens_per_sec = completion / 墙钟秒 = 400 / 6s。
	if got := m.TokensPerSec; got < 66.6 || got > 66.7 {
		t.Errorf("tokens_per_sec=%v want ~66.67", got)
	}
	if m.PromptTokens != 30 || m.CompletionTokens != 400 || m.TotalTokens != 430 {
		t.Errorf("tokens p=%d c=%d t=%d want 30/400/430", m.PromptTokens, m.CompletionTokens, m.TotalTokens)
	}
	// credit：缺失 ≠ 0——两次显式观测 1.0 + 3.0 = 4.0；credit_per_req = 4/3。
	tr.Observe(func() Observe { o := obs("m", 200, false, 0, time.Second); o.Credit = 1.0; o.CreditOK = true; return o }())
	tr.Observe(func() Observe { o := obs("m", 200, false, 0, time.Second); o.Credit = 3.0; o.CreditOK = true; return o }())
	m = tr.Snapshot().Models[0]
	if m.Credit != 4.0 {
		t.Errorf("credit=%v want 4.0（缺失观测不计入）", m.Credit)
	}
	if got := m.CreditPerReq; got < 0.79 || got > 0.81 {
		t.Errorf("credit_per_req=%v want ~0.8（4.0/全部 5 次请求）", got)
	}
	if m.LastSeen == nil {
		t.Error("last_seen should be set")
	}
}

func TestCacheHitRate(t *testing.T) {
	t.Run("hit+miss 双观测", func(t *testing.T) {
		tr := New("")
		o := obs("m", 200, true, 0, time.Second)
		o.CacheHit, o.CacheMiss, o.CacheWrite, o.CacheObserved = 80, 20, 5, true
		tr.Observe(o)
		if got := tr.Snapshot().Models[0].CacheHitRate; got != 0.8 {
			t.Errorf("cache_hit_rate=%v want 0.8", got)
		}
	})
	t.Run("只报 hit 不报 miss → hit/prompt", func(t *testing.T) {
		tr := New("")
		o := obs("m", 200, true, 0, time.Second)
		o.PromptTokens = 100
		o.CacheHit, o.CacheObserved = 80, true
		tr.Observe(o)
		if got := tr.Snapshot().Models[0].CacheHitRate; got != 0.8 {
			t.Errorf("cache_hit_rate=%v want 0.8（hit/prompt，不是 hit/(hit+0)=1.0）", got)
		}
	})
	t.Run("全零观测", func(t *testing.T) {
		tr := New("")
		o := obs("m", 200, true, 0, time.Second)
		o.CacheObserved = true
		tr.Observe(o)
		if got := tr.Snapshot().Models[0].CacheHitRate; got != 0 {
			t.Errorf("cache_hit_rate=%v want 0", got)
		}
	})
	t.Run("未观测", func(t *testing.T) {
		tr := New("")
		tr.Observe(obs("m", 200, true, 0, time.Second))
		if got := tr.Snapshot().Models[0].CacheHitRate; got != 0 {
			t.Errorf("cache_hit_rate=%v want 0", got)
		}
	})
}

func TestSnapshotTotalAndEmpty(t *testing.T) {
	tr := New("")
	s := tr.Snapshot()
	if s.Models == nil {
		t.Error("models must be [] not nil")
	}
	if !s.Since.IsZero() == false && s.UptimeSec < 0 {
		t.Errorf("uptime_sec=%d want >=0", s.UptimeSec)
	}
	tr.Observe(obs("a", 200, true, 0, time.Second))
	tr.Observe(obs("b", 500, false, 0, 2*time.Second))
	s = tr.Snapshot()
	tot := s.Total
	if tot.Requests != 2 || tot.Success != 1 || tot.Failed != 1 || tot.Streaming != 1 {
		t.Errorf("total req=%d ok=%d fail=%d stream=%d want 2/1/1/1", tot.Requests, tot.Success, tot.Failed, tot.Streaming)
	}
	if tot.PromptTokens != 20 || tot.TotalTokens != 220 {
		t.Errorf("total tokens p=%d t=%d want 20/220", tot.PromptTokens, tot.TotalTokens)
	}
	// 排序：requests 相同按模型名升序。
	if s.Models[0].Model != "a" || s.Models[1].Model != "b" {
		t.Errorf("models order: %q,%q want a,b", s.Models[0].Model, s.Models[1].Model)
	}
}

func TestResetClearsAndBumpsSince(t *testing.T) {
	tr := New("")
	first := tr.Snapshot().Since
	time.Sleep(2 * time.Millisecond)
	tr.Observe(obs("m", 200, true, 0, time.Second))
	tr.Reset()
	s := tr.Snapshot()
	if s.Total.Requests != 0 || len(s.Models) != 0 {
		t.Errorf("after reset: req=%d models=%d want 0/0", s.Total.Requests, len(s.Models))
	}
	if !s.Since.After(first) {
		t.Errorf("since not bumped: %v -> %v", first, s.Since)
	}
	// reset 后继续累计从零开始。
	tr.Observe(obs("m", 200, true, 0, time.Second))
	if got := tr.Snapshot().Total.Requests; got != 1 {
		t.Errorf("post-reset requests=%d want 1", got)
	}
}

func TestPersistLoadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.json")
	tr := New(path)
	tr.Observe(func() Observe {
		o := obs("m", 200, true, 250*time.Millisecond, time.Second)
		o.CacheHit, o.CacheMiss, o.CacheObserved = 7, 3, true
		o.Credit, o.CreditOK = 1.5, true
		return o
	}())
	tr.Flush()
	tr.Close()

	tr2 := New(path)
	defer tr2.Close()
	s := tr2.Snapshot()
	if len(s.Models) != 1 {
		t.Fatalf("models=%d want 1", len(s.Models))
	}
	m := s.Models[0]
	if m.Requests != 1 || m.AvgTTFBMS != 250 || m.CacheHitTokens != 7 || m.Credit != 1.5 {
		t.Errorf("roundtrip mismatch: %+v", m)
	}
	if s.Since.IsZero() {
		t.Error("since not restored")
	}
}

func TestLoadCorruptFileStartsZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.json")
	if err := writeFile(path, []byte("not json{{{")); err != nil {
		t.Fatal(err)
	}
	tr := New(path)
	defer tr.Close()
	if got := tr.Snapshot().Total.Requests; got != 0 {
		t.Errorf("requests=%d want 0 (corrupt file tolerated)", got)
	}
}

func TestConcurrentObserve(t *testing.T) {
	tr := New("")
	const workers, per = 8, 200
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < per; j++ {
				tr.Observe(obs("m", 200, true, time.Millisecond, time.Millisecond))
			}
		}()
	}
	wg.Wait()
	if got := tr.Snapshot().Total.Requests; got != workers*per {
		t.Errorf("requests=%d want %d", got, workers*per)
	}
}

func TestFlusherWritesAndCloseStops(t *testing.T) {
	old := flushInterval
	flushInterval = 20 * time.Millisecond
	t.Cleanup(func() { flushInterval = old })

	path := filepath.Join(t.TempDir(), "metrics.json")
	tr := New(path)
	tr.Observe(obs("m", 200, true, 0, time.Second))
	// 等 flusher 周期落盘（20ms 间隔，给 1s 上限裕量）。
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		tr2 := New(path)
		ok := tr2.Snapshot().Total.Requests == 1
		tr2.Close()
		if ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Close 后再 Observe 不再被周期落盘（goroutine 已停；最终 Flush 由 Close 完成）。
	tr.Close()
	tr.Observe(obs("m", 200, true, 0, time.Second)) // dirty 但 flusher 已停
	tr3 := New(path)
	defer tr3.Close()
	// Close 的最终 Flush 已把第一次 observe 写盘；第二次 observe 只在内存（或 Close 时补写）。
	// 这里只验证不 panic 且文件可读：requests 为 1 或 2 均合法（取决于 Close 后是否再次 Flush）。
	if got := tr3.Snapshot().Total.Requests; got < 1 || got > 2 {
		t.Errorf("requests=%d want 1..2", got)
	}
}
