// 通知事件注入点：pool 侧只定义事件结构并回调，绝不导入通知/邮件实现
// （依赖方向：notify → pool 单向）。
//
// 回调契约（重要）：notifyLocked 在**持有 p.mu 写锁**的状态下被调用——
// 注入的 NotifierFn 必须立即返回（入队用 select+default，禁止网络 I/O / 阻塞 / 加锁），
// 否则会拖慢整个选号热路径。
package pool

import "time"

// NoticeEvent 账号状态变更事件（供邮件/IM 通知模块消费）。
type NoticeEvent struct {
	// Kind 事件类型：
	//   "cooling"  账号进入（软/硬）冷却，流量转移到其他账号
	//   "breaker"  连续失败触发熔断
	//   "disabled" 账号被禁用（会话失效达阈值等）
	//   "exhausted" 余额耗尽（CoolHard，等签到恢复）
	Kind     string
	UID      string
	Nickname string
	Realm    string
	Reason   string
	// Credits/Expiring 事件发生时的余额快照（可用 / 其中快过期）。
	Credits  int64
	Expiring int64
	// Until 冷却/熔断截止时刻（Kind=cooling/breaker 时有效）。
	Until time.Time
	// At 事件发生时刻。
	At time.Time
}

// NotifierFn 通知回调。必须在持锁路径上非阻塞返回（见包注释）。
type NotifierFn func(NoticeEvent)

// SetNotifier 注入通知回调（nil = 关闭通知）。与 SetStore/SetBreaker 同为运行期注入。
func (p *Pool) SetNotifier(fn NotifierFn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.notifier = fn
}

// SetAvailability 注入"某 realm 当前可用账号"查询（main 注入 p.AvailableUIDsForRealm）。
// 通知模块用它渲染"路由切换后仍有哪些账号可用"。
func (p *Pool) SetAvailability(fn func(realm string) []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.availability = fn
}

// Availability 查询某 realm 当前可用账号（uid8 列表）；未注入时返回 nil。
// 供通知模块在**不加锁**的上下文中调用（内部自行加读锁）。
func (p *Pool) Availability(realm string) []string {
	p.mu.RLock()
	fn := p.availability
	p.mu.RUnlock()
	if fn == nil {
		return nil
	}
	return fn(realm)
}

// notifyLocked 触发一次通知。调用方必须已持有 p.mu；回调契约见包注释。
func (p *Pool) notifyLocked(ev NoticeEvent) {
	if p.notifier == nil {
		return
	}
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	p.notifier(ev)
}
