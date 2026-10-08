// Package logfmt 统一网关日志的 uid 截断与模块前缀约定。
//
// 约定：
//   - uid 统一截 8 位：与 chat 流水行（internal/server/logging.go uidPrefix）对齐，
//     日志行只留 uid 前 8 位。全量 uid 可从 data/state.json 查（54 个号无 8 位前缀碰撞）。
//   - 模块前缀：调度四类已有天然前缀（travel/activity/checkin/keepalive）保持；
//     其他补 [pool]/[auth]/[server] 等 [mod] 方括号前缀，redisstore/session 已有保持。
//   - 级别语义：正常流转不打级别字样（保持简洁）；可疑/降级/失败行加 WARN:/ERR: 前缀。
//
// 本包不引入日志库，只提供 UID8 截断 helper，供各包替代裸写 [:8]（防 uid 短于 8 越界）。
package logfmt

import (
	"log"
	"strings"
	"sync"
	"unicode/utf8"
)

// onceKeys 记录已告警过的 key，使 WarnOnce 每个 key 在**进程生命周期内只输出一次**。
// 用于持续性故障——例如 bind mount 属主错配导致每次运行期重扫 auths（每 30s）都写失败：
// 首次告警足以定位，重复刷屏只会淹没真正信号（实测 8 账号 × 每 30s 的行）。
var (
	onceMu   sync.Mutex
	onceKeys = map[string]bool{}
)

// WarnOnce 每个 key 只打印一次（进程级去重），首次命中才写日志。
// format/args 交给 log.Printf（调用方自行带 "WARN:" 前缀，与全仓日志约定一致）。
func WarnOnce(key, format string, args ...any) {
	onceMu.Lock()
	seen := onceKeys[key]
	onceKeys[key] = true
	onceMu.Unlock()
	if seen {
		return
	}
	log.Printf(format, args...)
}

// Truncate 截断字符串到 n 字节上限（先 TrimSpace，与旧 upstream/内部实现口径
// 一致），切点落在多字节字符中间时回退到 UTF-8 rune 边界——错误 body 多为中文
// （"将在 … 重置"），按字节切会出半截序列乱码。短于 n 原样返回；n<=0 返回空串。
func Truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		// s[n] 是切点后的首字节：是 rune 的后续字节（continuation）说明切点落在
		// 多字节字符中间，逐字节回退到 rune 边界（该字符整个让出）。
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		return s[:n]
	}
	return s
}

// UID8 返回 uid 的前 8 位；空 uid 返回 "-"（与 server.uidPrefix 对齐）。
//
// 用于调度类与非调度类日志行，把 <task> <full-uid>: ... 改为 <task> <uid8>: ...
// 全量 uid 留在 state.json 供排查，日志里 8 位足够唯一定位。
func UID8(uid string) string {
	if uid == "" {
		return "-"
	}
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}
