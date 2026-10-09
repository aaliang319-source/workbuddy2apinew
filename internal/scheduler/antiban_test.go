// antiban_test.go 反封控：抖动后的限速等待、签到账号间距、以及不可预测的上报 ID。
//
// 起因：六类任务原本都在**整点** HH:00:00 触发，账号间固定间隔 800ms/1.5s，
// 上报的 conversationId 是 wb2api-<UnixMilli>——三样都是白送的脚本指纹。
// 本文件锁定修复后的不变量（抖动区间、纯函数性、ID 不含时间戳/品牌前缀）。
package scheduler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// ---------------------------------------------------------------------------
// jitterDuration / pacedSleep
// ---------------------------------------------------------------------------

// TestJitterDurationInRange 抖动后的等待落在 ±20% 区间内，且确实有抖动（不是定值）。
func TestJitterDurationInRange(t *testing.T) {
	const d = 1000 * time.Millisecond
	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		got := jitterDuration(d)
		if got < 800*time.Millisecond || got > 1200*time.Millisecond {
			t.Fatalf("jitterDuration(%v)=%v 超出 ±20%% 区间", d, got)
		}
		seen[got] = true
	}
	if len(seen) < 50 {
		t.Errorf("200 次只出现 %d 个不同取值——抖动不足（仍是近似定长节奏）", len(seen))
	}
}

// TestJitterDurationZeroPassthrough d<=0 原样返回：既有测试"置 0 即不等待"的语义不能变。
func TestJitterDurationZeroPassthrough(t *testing.T) {
	if got := jitterDuration(0); got != 0 {
		t.Errorf("jitterDuration(0)=%v want 0", got)
	}
	if got := jitterDuration(-time.Second); got != -time.Second {
		t.Errorf("负值应原样返回，got=%v", got)
	}
}

// TestPacedSleepZeroImmediate d=0 立即放行，不白等。
func TestPacedSleepZeroImmediate(t *testing.T) {
	start := time.Now()
	if !pacedSleep(context.Background(), 0) {
		t.Error("d=0 应返回 true")
	}
	if e := time.Since(start); e > 50*time.Millisecond {
		t.Errorf("d=0 不应等待，耗时 %v", e)
	}
}

// TestPacedSleepCancellable 抖动等待仍可被 ctx 取消：抖动不能把可取消语义弄丢
// （否则优雅停机要等满一个随机时长）。
func TestPacedSleepCancellable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if pacedSleep(ctx, 3*time.Second) {
		t.Error("取消后应返回 false")
	}
	if e := time.Since(start); e > time.Second {
		t.Errorf("取消后应快速返回，实际 %v", e)
	}
}

// ---------------------------------------------------------------------------
// 签到账号间距
// ---------------------------------------------------------------------------

// TestCheckinAllPacesAccounts 签到账号之间要有限速：此前签到是唯一没有账号间距的
// 任务族，N 个账号在几百毫秒内密集打 /daily-checkin。
func TestCheckinAllPacesAccounts(t *testing.T) {
	old := checkinAccountDelay
	checkinAccountDelay = 120 * time.Millisecond
	t.Cleanup(func() { checkinAccountDelay = old })

	stub := &checkinStub{checkinBody: `{"code":0,"msg":"ok","data":{}}`, resourceRemain: 100}
	srv := stub.server()
	defer srv.Close()

	p := pool.New("")
	for _, uid := range []string{"u1", "u2", "u3"} {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	}
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	start := time.Now()
	out, err := s.CheckinAll()
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(out) != 3 {
		t.Fatalf("outcomes=%d want 3", len(out))
	}
	// 3 个账号 → 2 次账号间距（抖动下限 0.8×120ms=96ms）→ 至少 ~190ms。
	if elapsed < 190*time.Millisecond {
		t.Errorf("3 账号签到耗时 %v——账号间限速未生效", elapsed)
	}
}

// TestCheckinAllCancelsDuringAccountDelay ctx 取消后剩余账号不再签（限速等待可取消）。
func TestCheckinAllCancelsDuringAccountDelay(t *testing.T) {
	old := checkinAccountDelay
	checkinAccountDelay = 2 * time.Second
	t.Cleanup(func() { checkinAccountDelay = old })

	var checkins atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			checkins.Add(1)
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":100,"CycleCapacityRemain":100,"CycleCapacityUsed":0}]}}}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	p := pool.New("")
	for _, uid := range []string{"u1", "u2", "u3"} {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	}
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan []CheckinOutcome, 1)
	go func() {
		out, _ := s.checkinAll(ctx)
		done <- out
	}()

	if !waitCalls(&checkins, 1, 3*time.Second) {
		t.Fatal("3s 内未观察到第 1 个账号签到")
	}
	cancel()
	select {
	case out := <-done:
		if len(out) != 1 {
			t.Errorf("取消后应只剩 1 条回执，got=%d", len(out))
		}
	case <-time.After(1500 * time.Millisecond):
		t.Error("checkinAll 未在取消后退出（账号间限速不可取消）")
	}
}

// ---------------------------------------------------------------------------
// 上报 ID 不可预测
// ---------------------------------------------------------------------------

var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

// TestActivityConversationIDOpaque conversationId 必须是不可预测的 32 hex：
// 不含 wb2api 品牌前缀，也不把触发时刻编码进去。
func TestActivityConversationIDOpaque(t *testing.T) {
	fastActivity(t)

	var mu = make(chan map[string]any, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/report":
			var arr []map[string]any
			_ = json.NewDecoder(r.Body).Decode(&arr)
			if len(arr) > 0 {
				select {
				case mu <- arr[0]:
				default:
				}
			}
			w.Write([]byte(`{"code":0,"msg":"OK"}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, ActivityReportCount: 2})

	s.RunActivityNow()

	first := <-mu
	cid, _ := first["conversationId"].(string)
	if !hex32.MatchString(cid) {
		t.Errorf("conversationId=%q want 32 位 hex（不可预测、无品牌前缀）", cid)
	}
	if cid == "" || len(cid) < 32 {
		t.Errorf("conversationId=%q 过短", cid)
	}
	// 同一批 N 条共用同一会话（同会话多轮语义不变）。
	second := <-mu
	if second["conversationId"] != cid {
		t.Errorf("同批 conversationId 应一致：%v vs %v", second["conversationId"], cid)
	}
	if second["requestId"] == cid {
		t.Error("requestId 应与 conversationId 不同（多轮独立）")
	}
}

// TestNewOpaqueIDUnique newOpaqueID 每次不同（不退化到固定值/时间戳碰撞）。
func TestNewOpaqueIDUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := newOpaqueID()
		if !hex32.MatchString(id) {
			t.Fatalf("id=%q want 32 hex", id)
		}
		if seen[id] {
			t.Fatalf("id=%q 重复", id)
		}
		seen[id] = true
	}
}
