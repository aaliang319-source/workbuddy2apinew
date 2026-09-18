// notify 包测试：节流、事件开关、队列满不阻塞、ScanCredits 触发条件、模板内容、
// 配置归一化与 SMTP 模式错误信息。
package notify

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

// fakeSender 记录发送内容，可注入失败。
type fakeSender struct {
	mu   sync.Mutex
	got  []sentMail
	fail error
}

type sentMail struct {
	To      []string
	Subject string
	Body    string
}

func (f *fakeSender) Send(to []string, subject, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, sentMail{To: to, Subject: subject, Body: body})
	return f.fail
}

func (f *fakeSender) mails() []sentMail {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentMail(nil), f.got...)
}

// waitMail 轮询等待第 n 封邮件（worker 异步）。
func waitMail(t *testing.T, f *fakeSender, n int) []sentMail {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := f.mails(); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting %d mail(s), got %d", n, len(f.mails()))
	return nil
}

func testNotifier(t *testing.T, mut func(*Config)) (*Notifier, *fakeSender) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.Host = "smtp.example.com"
	cfg.From = "bot@example.com"
	cfg.To = []string{"ops@example.com"}
	if mut != nil {
		mut(&cfg)
	}
	f := &fakeSender{}
	n := New(cfg, f, func(realm string) []string { return []string{"alive1"} })
	n.Start(context.Background())
	t.Cleanup(n.Close)
	return n, f
}

func TestThrottleSuppressesDuplicate(t *testing.T) {
	n, f := testNotifier(t, nil)
	n.NotifyExhausted("u1", "甲", "cn", "余额不足", 0, 0)
	waitMail(t, f, 1)
	// 同 (事件, 账号) 窗口内第二次应被节流。
	n.NotifyExhausted("u1", "甲", "cn", "余额不足", 0, 0)
	time.Sleep(50 * time.Millisecond)
	if got := len(f.mails()); got != 1 {
		t.Fatalf("throttle failed: got %d mails, want 1", got)
	}
	// 不同账号应放行。
	n.NotifyExhausted("u2", "乙", "cn", "余额不足", 0, 0)
	if got := waitMail(t, f, 2); len(got) != 2 {
		t.Fatalf("different uid should bypass throttle, got %d", len(got))
	}
}

func TestEventToggles(t *testing.T) {
	n, f := testNotifier(t, func(c *Config) {
		c.Events.CreditsLow = false
		c.Events.Exhausted = true
		c.Events.AccountSwitch = false
	})
	// 账号路由切换关闭 → 不发送。
	n.OnPoolEvent(pool.NoticeEvent{Kind: "cooling", UID: "u1", Reason: "429"})
	// 额度耗尽开启 → 发送。
	n.OnPoolEvent(pool.NoticeEvent{Kind: "exhausted", UID: "u2", Reason: "余额不足"})
	got := waitMail(t, f, 1)
	if len(got) != 1 {
		t.Fatalf("want exactly 1 mail (only exhausted), got %d", len(got))
	}
	if !strings.Contains(got[0].Subject, "额度已耗尽") {
		t.Fatalf("wrong subject: %s", got[0].Subject)
	}
}

func TestScanCreditsLowAndExpiring(t *testing.T) {
	n, f := testNotifier(t, func(c *Config) {
		c.CreditsThreshold = 100
		c.Events.AccountSwitch = false
	})
	p := pool.New("")
	p.Add(&auth.Auth{UID: "low0000001", Nickname: "低额", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "rich000001", Nickname: "充足", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "exp0000001", Nickname: "快过期", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "dis0000001", Nickname: "已禁用", Domain: "www.codebuddy.cn"})
	p.SetCreditsDetailed("low0000001", 50, 0)
	p.SetCreditsDetailed("rich000001", 5000, 0)
	p.SetCreditsDetailed("exp0000001", 5000, 400)
	p.SetCreditsDetailed("dis0000001", 10, 0)
	p.Disable("dis0000001", "manual")

	n.ScanCredits(p)
	got := waitMail(t, f, 2)
	if len(got) != 2 {
		t.Fatalf("want 2 mails (low + expiring), got %d", len(got))
	}
	subjects := got[0].Subject + " | " + got[1].Subject
	if !strings.Contains(subjects, "低额") || !strings.Contains(subjects, "快过期") {
		t.Fatalf("subjects missing accounts: %s", subjects)
	}
	if strings.Contains(subjects, "充足") || strings.Contains(subjects, "已禁用") {
		t.Fatalf("should not notify rich/disabled: %s", subjects)
	}
}

func TestTemplateContainsKeyFields(t *testing.T) {
	n, f := testNotifier(t, nil)
	n.OnPoolEvent(pool.NoticeEvent{
		Kind: "cooling", UID: "00000000-0000-4000-8000-000000000001", Nickname: "test-user",
		Realm: "global", Reason: "429 rate limit", Credits: 2364, Expiring: 100,
	})
	got := waitMail(t, f, 1)
	body := got[0].Body
	for _, want := range []string{"账号路由切换", "test-user", "00000000", "国际 global", "2364", "429 rate limit", "alive1"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	if !strings.HasPrefix(got[0].Subject, subjectPrefix) {
		t.Fatalf("subject prefix missing: %s", got[0].Subject)
	}
}

func TestQueueFullDoesNotBlock(t *testing.T) {
	// 队列 1 + 不发 worker（不 Start）：热路径入队必须立即返回。
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.QueueSize = 1
	f := &fakeSender{fail: nil}
	n := New(cfg, f, nil)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			n.OnPoolEvent(pool.NoticeEvent{Kind: "cooling", UID: "u" + string(rune('a'+i%26)) + string(rune('0'+i/26)), Reason: "x"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("enqueue blocked when queue full")
	}
}

func TestDisabledNotifierIsNoop(t *testing.T) {
	cfg := DefaultConfig() // Enabled=false
	f := &fakeSender{}
	n := New(cfg, f, nil)
	n.Start(context.Background())
	t.Cleanup(n.Close)
	n.NotifyExhausted("u1", "甲", "cn", "x", 0, 0)
	n.OnPoolEvent(pool.NoticeEvent{Kind: "disabled", UID: "u1"})
	time.Sleep(30 * time.Millisecond)
	if got := len(f.mails()); got != 0 {
		t.Fatalf("disabled notifier sent %d mails", got)
	}
	if err := n.SendTest(); err == nil {
		t.Fatal("SendTest on disabled notifier should error")
	}
}

func TestSendTestBypassesThrottle(t *testing.T) {
	n, f := testNotifier(t, nil)
	if err := n.SendTest(); err != nil {
		t.Fatalf("SendTest: %v", err)
	}
	if err := n.SendTest(); err != nil {
		t.Fatalf("SendTest twice: %v", err)
	}
	got := f.mails()
	if len(got) != 2 {
		t.Fatalf("SendTest should bypass throttle, got %d", len(got))
	}
	if !strings.Contains(got[0].Subject, "测试邮件") {
		t.Fatalf("test subject wrong: %s", got[0].Subject)
	}
}

func TestConfigNormalize(t *testing.T) {
	// 缺省回填
	c := Config{Enabled: true, Host: "h", From: "f@x", To: []string{"t@x"}}
	if err := c.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Port != 587 || c.TLSMode != "starttls" || c.CreditsThreshold != 100 || c.ExpiringDays != 7 ||
		c.Throttle != 6*time.Hour || c.QueueSize != 256 {
		t.Fatalf("defaults not applied: %+v", c)
	}
	if !c.Events.CreditsLow || !c.Events.Exhausted || !c.Events.AccountSwitch {
		t.Fatalf("events should default open: %+v", c.Events)
	}
	// Enabled 但缺字段 → fail-fast
	bad := Config{Enabled: true, Host: ""}
	if err := bad.Normalize(); err != ErrMissingSMTP {
		t.Fatalf("want ErrMissingSMTP, got %v", err)
	}
	// 非 Enabled 缺字段不报错（模板段可留空）
	off := Config{}
	if err := off.Normalize(); err != nil {
		t.Fatalf("disabled config should not error: %v", err)
	}
	// 非法 tls/端口回填
	odd := Config{Enabled: false, Port: 99999, TLSMode: "ssl"}
	_ = odd.Normalize()
	if odd.Port != 587 || odd.TLSMode != "starttls" {
		t.Fatalf("invalid values not normalized: %+v", odd)
	}
}

func TestSenderNoneModeWithAuthRejected(t *testing.T) {
	// 明文 + 认证：必须在连接前给出可读错误（不发起网络）。
	s := NewSMTPSender(Config{Host: "127.0.0.1", Port: 1, TLSMode: "none", Username: "u", Password: "p"})
	err := s.Send([]string{"a@b"}, "s", "body")
	if err == nil {
		t.Fatal("want error for none+auth")
	}
	if !strings.Contains(err.Error(), "starttls") && !strings.Contains(err.Error(), "连接") {
		t.Fatalf("error should be actionable, got: %v", err)
	}
}

func TestBuildMessageEncoding(t *testing.T) {
	raw := buildMessage("bot@x.com", []string{"ops@x.com"}, "额度已耗尽 · 甲", "正文内容")
	if !strings.Contains(raw, "Subject: =?utf-8?") {
		t.Fatalf("subject should be RFC2047 encoded: %s", raw)
	}
	if !strings.Contains(raw, "Content-Transfer-Encoding: base64") {
		t.Fatal("body should be base64 encoded")
	}
	if strings.Contains(raw, "正文内容") {
		t.Fatal("body must not be raw UTF-8 when base64-encoded")
	}
}

func TestScanCreditsZeroEmitsExhausted(t *testing.T) {
	n, f := testNotifier(t, nil)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "zero000001", Nickname: "归零", Domain: "www.codebuddy.cn"})
	p.SetCreditsDetailed("zero000001", 0, 0)
	n.ScanCredits(p)
	got := waitMail(t, f, 1)
	if len(got) != 1 || !strings.Contains(got[0].Subject, "额度已耗尽") {
		t.Fatalf("zero credits should emit exhausted, got %+v", got)
	}
	if !strings.Contains(got[0].Body, "归零") {
		t.Fatalf("body missing nickname: %s", got[0].Body)
	}
}

func TestNextScanTime(t *testing.T) {
	loc := time.Local
	// 08:00 时，[10,22] 的下一扫描点应为当天 10:00。
	now := time.Date(2026, 9, 18, 8, 0, 0, 0, loc)
	if got := nextScanTime(now, []int{10, 22}); got.Hour() != 10 || got.Day() != 18 {
		t.Fatalf("want today 10:00, got %v", got)
	}
	// 23:00 时，应为次日 10:00。
	now = time.Date(2026, 9, 18, 23, 0, 0, 0, loc)
	if got := nextScanTime(now, []int{10, 22}); got.Hour() != 10 || got.Day() != 19 {
		t.Fatalf("want next day 10:00, got %v", got)
	}
	// 10:00 整点：应算下一次（严格 After），避免同一时刻重复扫描。
	now = time.Date(2026, 9, 18, 10, 0, 0, 0, loc)
	if got := nextScanTime(now, []int{10, 22}); got.Day() != 18 || got.Hour() != 22 {
		t.Fatalf("want today 22:00, got %v", got)
	}
}
