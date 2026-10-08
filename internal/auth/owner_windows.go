//go:build windows

package auth

import "io/fs"

// fileOwner 在 Windows 上恒不可得（无 Unix uid/gid 语义，也不做 Chown）。
func fileOwner(fs.FileInfo) (uid, gid int, ok bool) { return 0, 0, false }
