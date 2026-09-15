// SMTP 发送实现（stdlib net/smtp + crypto/tls，保持零第三方依赖）。
// 支持三种连接模式：starttls（587 提交口）、tls（465 隐式 TLS）、none（明文，仅内网中继）。
package notify

import (
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// Sender 发送抽象（测试注入假实现）。
type Sender interface {
	Send(to []string, subject, body string) error
}

// SMTPSender 基于 stdlib 的 SMTP 发送器。
type SMTPSender struct {
	cfg    Config
	dialer *net.Dialer
}

// NewSMTPSender 构建发送器（cfg 应已 Normalize）。
func NewSMTPSender(cfg Config) *SMTPSender {
	return &SMTPSender{cfg: cfg, dialer: &net.Dialer{Timeout: 10 * time.Second}}
}

// Send 发送一封邮件（含 RFC5322 头与 UTF-8 正文）。
func (s *SMTPSender) Send(to []string, subject, body string) error {
	addr := net.JoinHostPort(s.cfg.Host, fmt.Sprintf("%d", s.cfg.Port))
	c, err := s.dial(addr)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	// 认证：仅在配置了用户名时进行。PlainAuth 拒绝非 TLS 连接——显式给出可读错误。
	if s.cfg.Username != "" {
		if s.cfg.TLSMode == "none" {
			return fmt.Errorf("smtp_tls=none 时无法安全认证：请改为 starttls 或 tls（明文提交密码会被拒绝）")
		}
		if err := c.Auth(smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)); err != nil {
			return fmt.Errorf("SMTP 认证失败：%w", err)
		}
	}
	if err := c.Mail(s.cfg.From); err != nil {
		return fmt.Errorf("SMTP MAIL FROM 失败：%w", err)
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return fmt.Errorf("SMTP RCPT TO(%s) 失败：%w", rcpt, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("SMTP DATA 失败：%w", err)
	}
	raw := buildMessage(s.cfg.From, to, subject, body)
	if _, err := w.Write([]byte(raw)); err != nil {
		_ = w.Close()
		return fmt.Errorf("SMTP 写入正文失败：%w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("SMTP 提交失败：%w", err)
	}
	if err := c.Quit(); err != nil {
		// Quit 失败不视为发送失败：DATA 已被接受（部分服务端不优雅收尾）。
		return nil
	}
	return nil
}

// dial 按 TLSMode 建立连接。
func (s *SMTPSender) dial(addr string) (*smtp.Client, error) {
	switch s.cfg.TLSMode {
	case "tls":
		conn, err := tls.DialWithDialer(s.dialer, "tcp", addr, &tls.Config{ServerName: s.cfg.Host})
		if err != nil {
			return nil, fmt.Errorf("SMTP TLS 连接(%s)失败：%w", addr, err)
		}
		_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
		c, err := smtp.NewClient(conn, s.cfg.Host)
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("SMTP 握手失败：%w", err)
		}
		return c, nil
	default: // starttls / none
		c, err := smtp.Dial(addr)
		if err != nil {
			return nil, fmt.Errorf("SMTP 连接(%s)失败：%w", addr, err)
		}
		if s.cfg.TLSMode == "starttls" {
			if err := c.StartTLS(&tls.Config{ServerName: s.cfg.Host}); err != nil {
				_ = c.Close()
				return nil, fmt.Errorf("SMTP STARTTLS 失败：%w", err)
			}
		}
		return c, nil
	}
}

// buildMessage 组装 RFC5322 报文（UTF-8 主题用 RFC2047 B 编码，正文 base64 编码避免
// 8bit 兼容问题；不引入第三方库，手写最小实现）。
func buildMessage(from string, to []string, subject, body string) string {
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + strings.Join(to, ", ") + "\r\n")
	b.WriteString("Subject: " + encodeSubject(subject) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	b.WriteString("\r\n")
	b.WriteString(base64Wrap(body))
	return b.String()
}

// encodeSubject 中文主题按 RFC2047 B 编码（stdlib mime.WordEncoder）。
func encodeSubject(s string) string {
	return mime.BEncoding.Encode("utf-8", s)
}

// base64Wrap 按 76 列折行的 base64（RFC2045 要求）。
func base64Wrap(s string) string {
	enc := base64.StdEncoding.EncodeToString([]byte(s))
	var b strings.Builder
	for i := 0; i < len(enc); i += 76 {
		end := i + 76
		if end > len(enc) {
			end = len(enc)
		}
		b.WriteString(enc[i:end])
		b.WriteString("\r\n")
	}
	return b.String()
}
