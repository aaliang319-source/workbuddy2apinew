// 通知邮件文案渲染（中文纯文本正文）。
package notify

import (
	"fmt"
	"strings"
	"time"
)

// subjectPrefix 主题前缀（便于邮箱规则过滤）。
const subjectPrefix = "[workbuddy2api]"

// kindLabel 事件类型中文名。
func kindLabel(k EventKind) string {
	switch k {
	case KindCreditsLow:
		return "额度即将耗尽"
	case KindExhausted:
		return "额度已耗尽"
	case KindAccountSwitch:
		return "账号路由切换"
	default:
		return string(k)
	}
}

// Event 一封通知所需信息。
type Event struct {
	Kind     EventKind
	UID      string
	Nickname string
	Realm    string
	Reason   string
	Credits  int64
	Expiring int64
	At       time.Time
	// Available 事件 C 专用：当前仍可用的账号（uid8 列表）。
	Available []string
}

// subject 生成主题：`[workbuddy2api] 额度已耗尽 · 测试用户(12345678)`
func (e Event) subject() string {
	name := e.Nickname
	if name == "" {
		name = "-"
	}
	return fmt.Sprintf("%s %s · %s(%s)", subjectPrefix, kindLabel(e.Kind), name, uid8(e.UID))
}

// body 生成正文（纯文本，字段逐行）。
func (e Event) body() string {
	var b strings.Builder
	at := e.At
	if at.IsZero() {
		at = time.Now()
	}
	b.WriteString("workbuddy2api 账号通知\r\n")
	b.WriteString("────────────────────────────\r\n")
	fmt.Fprintf(&b, "事件：%s\r\n", kindLabel(e.Kind))
	fmt.Fprintf(&b, "时间：%s\r\n", at.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "账号：%s (%s)\r\n", orDash(e.Nickname), uid8(e.UID))
	fmt.Fprintf(&b, "域：%s\r\n", realmLabel(e.Realm))
	fmt.Fprintf(&b, "可用额度：%d\r\n", e.Credits)
	if e.Expiring > 0 {
		fmt.Fprintf(&b, "其中快过期：%d\r\n", e.Expiring)
	}
	if e.Reason != "" {
		fmt.Fprintf(&b, "原因：%s\r\n", e.Reason)
	}
	switch e.Kind {
	case KindCreditsLow:
		b.WriteString("\r\n建议：尽快使用或补充该账号额度，避免额度过期作废。\r\n")
	case KindExhausted:
		b.WriteString("\r\n建议：该账号已退出服务，等待签到/充值后自动恢复；期间流量由其他账号承接。\r\n")
	case KindAccountSwitch:
		if len(e.Available) > 0 {
			fmt.Fprintf(&b, "\r\n当前仍可用账号（%d）：%s\r\n", len(e.Available), strings.Join(e.Available, ", "))
		} else {
			b.WriteString("\r\n警告：该域当前已无其他可用账号，请求可能失败！\r\n")
		}
	}
	b.WriteString("\r\n（本邮件由 workbuddy2api 网关自动发送，同一账号同类事件默认 6 小时内不再重复提醒）\r\n")
	return b.String()
}

func uid8(uid string) string {
	if len(uid) >= 8 {
		return uid[:8]
	}
	if uid == "" {
		return "-"
	}
	return uid
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func realmLabel(realm string) string {
	if realm == "global" {
		return "国际 global"
	}
	return "国内 cn"
}
