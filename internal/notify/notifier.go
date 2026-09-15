// Notifier 通知调度：事件过滤 → 节流 → 入队 → 单 worker 发送。
//
// 热路径契约：OnPoolEvent 由 pool 在**持写锁**状态下调用，只做 map 查节流 + 非阻塞入队，
// 绝不发起网络 I/O；队列满则丢弃并节流打日志（丢弃优于拖慢网关）。
package notify

import (
	"context"
	"errors"
	"log"
	"strconv"
	"sync"
	"time"

	"workbuddy2api/internal/pool"
)

// errNotEnabled 通知未启用（管理端点据此返回 400）。
var errNotEnabled = errors.New("notify disabled: 请在网关配置中启用并填写 SMTP 后重启")

// Notifier 通知器。Enabled=false 时所有方法为安全空操作。
type Notifier struct {
	cfg    Config
	sender Sender
	avail  func(realm string) []string

	ch     chan Event
	mu     sync.Mutex
	lastAt map[string]time.Time // 节流表：kind|uid → 上次发送时刻
	drops  int                  // 连续丢弃计数（日志节流）
	closed bool
	stopCh chan struct{} // Close 时关闭：让扫描 goroutine 与 worker 一起退出
	wg     sync.WaitGroup
}

// New 构建通知器。avail 可 nil（不渲染可用账号列表）。
func New(cfg Config, sender Sender, avail func(realm string) []string) *Notifier {
	size := cfg.QueueSize
	if size <= 0 {
		size = 256
	}
	return &Notifier{
		cfg:    cfg,
		sender: sender,
		avail:  avail,
		ch:     make(chan Event, size),
		lastAt: map[string]time.Time{},
		stopCh: make(chan struct{}),
	}
}

// Start 启动后台发送 worker（ctx 结束或 Close 时退出）。
func (n *Notifier) Start(ctx context.Context) {
	if n == nil || !n.cfg.Enabled || n.sender == nil {
		return
	}
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-n.ch:
				if !ok {
					return
				}
				n.send(ev)
			}
		}
	}()
}

// Close 关闭队列与扫描并等待 goroutine 退出（幂等）。
func (n *Notifier) Close() {
	if n == nil {
		return
	}
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return
	}
	n.closed = true
	close(n.stopCh)
	close(n.ch)
	n.mu.Unlock()
	n.wg.Wait()
}

// StartScan 启动额度周期扫描（本地时区整点，由 config notify.scan_hours 指定）。
// hours 为空 = 不启动（只保留实时事件）。扫描在 checkin 之后（默认 10/22 点，
// 签到为 9/21 点），因此读到的是刷新后的余额。
func (n *Notifier) StartScan(ctx context.Context, p *pool.Pool, hours []int) {
	if !n.Enabled() || p == nil || len(hours) == 0 {
		return
	}
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		for {
			next := nextScanTime(time.Now(), hours)
			timer := time.NewTimer(time.Until(next))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-n.stopCh:
				timer.Stop()
				return
			case <-timer.C:
			}
			n.ScanCredits(p)
		}
	}()
}

// nextScanTime 返回 now 之后最近的扫描时刻（hours 中当天尚未到的最近整点，否则次日最早）。
func nextScanTime(now time.Time, hours []int) time.Time {
	best := time.Time{}
	for _, h := range hours {
		cand := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !cand.After(now) {
			cand = cand.Add(24 * time.Hour)
		}
		if best.IsZero() || cand.Before(best) {
			best = cand
		}
	}
	return best
}

// Enabled 是否启用。
func (n *Notifier) Enabled() bool { return n != nil && n.cfg.Enabled && n.sender != nil }

// send 实际发送（worker goroutine 内，串行 → 天然限流）。
func (n *Notifier) send(ev Event) {
	if err := n.sender.Send(n.cfg.To, ev.subject(), ev.body()); err != nil {
		log.Printf("WARN: [notify] send failed kind=%s uid=%s: %v", ev.Kind, uid8(ev.UID), err)
		return
	}
	log.Printf("INFO: [notify] sent kind=%s uid=%s recipients=%d", ev.Kind, uid8(ev.UID), len(n.cfg.To))
}

// enqueue 非阻塞入队（满则丢弃 + 节流日志）。热路径安全。
func (n *Notifier) enqueue(ev Event) {
	n.mu.Lock()
	closed := n.closed
	n.mu.Unlock()
	if closed {
		return
	}
	select {
	case n.ch <- ev:
	default:
		n.mu.Lock()
		n.drops++
		drops := n.drops
		n.mu.Unlock()
		if drops == 1 || drops%50 == 0 {
			log.Printf("WARN: [notify] queue full, dropped %d event(s) (kind=%s uid=%s)", drops, ev.Kind, uid8(ev.UID))
		}
	}
}

// allow 节流判定：同一 (事件, 账号) 在 Throttle 窗口内只放行一次。
func (n *Notifier) allow(kind EventKind, uid string) bool {
	key := string(kind) + "|" + uid
	now := time.Now()
	n.mu.Lock()
	defer n.mu.Unlock()
	// 顺带清理陈旧项，避免长期运行 map 无限增长。
	if len(n.lastAt) > 512 {
		for k, t := range n.lastAt {
			if now.Sub(t) > n.cfg.Throttle {
				delete(n.lastAt, k)
			}
		}
	}
	if t, ok := n.lastAt[key]; ok && now.Sub(t) < n.cfg.Throttle {
		return false
	}
	n.lastAt[key] = now
	return true
}

// emit 事件开关 + 节流 + 入队（统一入口）。
func (n *Notifier) emit(ev Event) {
	if !n.Enabled() {
		return
	}
	on := false
	switch ev.Kind {
	case KindCreditsLow:
		on = n.cfg.Events.CreditsLow
	case KindExhausted:
		on = n.cfg.Events.Exhausted
	case KindAccountSwitch:
		on = n.cfg.Events.AccountSwitch
	}
	if !on {
		return
	}
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	if !n.allow(ev.Kind, ev.UID) {
		return
	}
	if ev.Kind == KindAccountSwitch && n.avail != nil {
		ev.Available = n.avail(ev.Realm)
	}
	n.enqueue(ev)
}

// OnPoolEvent pool 状态转换回调（持锁路径：仅做开关/节流/非阻塞入队）。
// pool 的事件类型映射：cooling/breaker/disabled → 账号路由切换；exhausted → 额度已耗尽。
func (n *Notifier) OnPoolEvent(pe pool.NoticeEvent) {
	if !n.Enabled() {
		return
	}
	kind := KindAccountSwitch
	if pe.Kind == "exhausted" {
		kind = KindExhausted
	}
	n.emit(Event{
		Kind: kind, UID: pe.UID, Nickname: pe.Nickname, Realm: pe.Realm,
		Reason: pe.Reason, Credits: pe.Credits, Expiring: pe.Expiring, At: pe.At,
	})
}

// NotifyExhausted 事件 B：余额刷新（签到）后确认 remain==0 时由调度器调用。
func (n *Notifier) NotifyExhausted(uid, nickname, realm, reason string, credits, expiring int64) {
	n.emit(Event{
		Kind: KindExhausted, UID: uid, Nickname: nickname, Realm: realm,
		Reason: reason, Credits: credits, Expiring: expiring,
	})
}

// ScanCredits 事件 A：扫描全池账号，额度低于阈值或存在快过期额度时通知。
// 注意：pool 只保留"快过期额度总量"（expiring_soon 窗口聚合），无法给出每批的
// 精确到期时刻，故这里按"存在快过期额度"触发（窗口语义与 pool.expiring_soon 一致，
// 两者不一致时启动期会打 WARN）。
func (n *Notifier) ScanCredits(p *pool.Pool) {
	if !n.Enabled() || p == nil {
		return
	}
	for _, st := range p.List() {
		if st.Disabled {
			continue
		}
		if st.Credits <= 0 {
			// 余额为 0：已耗尽（与 402 硬冷却路径互补——这里覆盖"没请求所以没撞 402"的账号）。
			n.emit(Event{
				Kind: KindExhausted, UID: st.UID, Nickname: st.Nickname, Realm: st.Realm,
				Reason: "可用额度为 0（等待签到/充值恢复）", Credits: st.Credits, Expiring: st.CreditsExpiring,
			})
			continue
		}
		if st.Credits < n.cfg.CreditsThreshold {
			n.emit(Event{
				Kind: KindCreditsLow, UID: st.UID, Nickname: st.Nickname, Realm: st.Realm,
				Reason:  "可用额度低于阈值(" + strconv.FormatInt(n.cfg.CreditsThreshold, 10) + ")",
				Credits: st.Credits, Expiring: st.CreditsExpiring,
			})
			continue
		}
		if st.CreditsExpiring > 0 {
			n.emit(Event{
				Kind: KindCreditsLow, UID: st.UID, Nickname: st.Nickname, Realm: st.Realm,
				Reason:  "存在即将过期的额度",
				Credits: st.Credits, Expiring: st.CreditsExpiring,
			})
		}
	}
}

// SendTest 发送一封测试邮件（同步、绕过节流与队列，供管理端点验证配置）。
func (n *Notifier) SendTest() error {
	if !n.Enabled() {
		return errNotEnabled
	}
	ev := Event{
		Kind: KindAccountSwitch, UID: "test", Nickname: "测试账号",
		Realm: "cn", Reason: "配置验证（测试邮件）",
		Credits: n.cfg.CreditsThreshold, Available: []string{"test0001", "test0002"},
		At: time.Now(),
	}
	return n.sender.Send(n.cfg.To, subjectPrefix+" 测试邮件", ev.body())
}
