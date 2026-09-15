// Package notify 账号事件邮件提醒：额度即将耗尽、额度已耗尽、账号路由切换。
//
// 依赖方向：notify → pool（单向）；pool 只持有一个非阻塞回调，不感知邮件实现。
// SMTP 发送全部在单一后台 worker 上进行（有界队列，满即丢弃），
// 保证注入到选号热路径（持 pool 写锁）的回调永不阻塞。
//
// 配置为启动期加载（与其他配置一致）：修改 SMTP 后需重启网关。
// 节流表为内存态：重启清零（避免旧状态导致重启后该报的事件被吞）。
package notify

import (
	"errors"
	"time"
)

// EventKind 通知事件类型。
type EventKind string

const (
	// KindCreditsLow 额度即将耗尽（低于阈值，或有额度即将过期）。
	KindCreditsLow EventKind = "credits_low"
	// KindExhausted 额度已耗尽（余额为 0 / 402 硬额度冷却）。
	KindExhausted EventKind = "exhausted"
	// KindAccountSwitch 账号路由切换（账号进入冷却/熔断/被禁用，流量转移）。
	KindAccountSwitch EventKind = "account_switch"
)

// Events 事件开关。
type Events struct {
	CreditsLow    bool
	Exhausted     bool
	AccountSwitch bool
}

// Config 通知运行期配置（由 cmd/server 从 config.json 的 notify 段映射）。
type Config struct {
	Enabled bool

	Host     string
	Port     int
	Username string
	Password string
	From     string
	To       []string
	// TLSMode "starttls"（587，默认）/"tls"（465 隐式 TLS）/"none"（明文，仅内网中继）。
	TLSMode string

	// CreditsThreshold 事件 A：可用额度低于该值即告警。
	CreditsThreshold int64
	// ExpiringDays 事件 A：存在快过期额度即告警（窗口语义，见 ScanCredits 注释）。
	ExpiringDays int
	// Throttle 同一 (事件, 账号) 的最小通知间隔。
	Throttle time.Duration
	// QueueSize 待发队列容量（满则丢弃并记日志）。
	QueueSize int

	Events Events
}

// ErrMissingSMTP Enabled 时 SMTP 必需项缺失（fail-fast 用）。
var ErrMissingSMTP = errors.New("notify.enabled=true 但缺少 smtp_host / smtp_from / smtp_to（请补全后重启网关）")

// DefaultConfig 默认配置（与 config.json 缺省值一致）。
func DefaultConfig() Config {
	return Config{
		Enabled:          false,
		Port:             587,
		TLSMode:          "starttls",
		CreditsThreshold: 100,
		ExpiringDays:     7,
		Throttle:         6 * time.Hour,
		QueueSize:        256,
		Events:           Events{CreditsLow: true, Exhausted: true, AccountSwitch: true},
	}
}

// Normalize 回填缺省值并做边界钳制（幂等）。Enabled 但缺少 SMTP 必需项时返回
// ErrMissingSMTP——宁可启动失败，也不要静默不通知。
func (c *Config) Normalize() error {
	if c.Port <= 0 || c.Port > 65535 {
		c.Port = 587
	}
	switch c.TLSMode {
	case "starttls", "tls", "none":
	default:
		c.TLSMode = "starttls"
	}
	if c.CreditsThreshold <= 0 {
		c.CreditsThreshold = 100
	}
	if c.ExpiringDays <= 0 {
		c.ExpiringDays = 7
	}
	if c.Throttle <= 0 {
		c.Throttle = 6 * time.Hour
	}
	if c.QueueSize <= 0 {
		c.QueueSize = 256
	}
	if !c.Events.CreditsLow && !c.Events.Exhausted && !c.Events.AccountSwitch {
		// JSON 三字段全缺省（false）与"显式全关"不可区分：按缺省全开处理。
		c.Events = Events{CreditsLow: true, Exhausted: true, AccountSwitch: true}
	}
	if !c.Enabled {
		return nil
	}
	if c.Host == "" || c.From == "" || len(c.To) == 0 {
		return ErrMissingSMTP
	}
	return nil
}
