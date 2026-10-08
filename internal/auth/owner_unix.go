//go:build !windows

package auth

import (
	"io/fs"
	"syscall"
)

// fileOwner 返回文件属主的 uid/gid（Unix）。ok=false 表示底层 stat 信息不可得
// （非 Unix 的 Sys() 类型），调用方应跳过 Chown。
func fileOwner(fi fs.FileInfo) (uid, gid int, ok bool) {
	st, isStat := fi.Sys().(*syscall.Stat_t)
	if !isStat {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
