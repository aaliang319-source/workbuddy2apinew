package authstore

import (
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api-gui/internal/fsutil"
)

// TestOwnerPreservedWhenUnconfigured 未显式配置 SetOwner 时，落盘必须**保留文件原属主**，
// 而不是把新文件（面板进程 uid）的属主带上去。
//
// 回归：面板以 root 运行为默认（docker-compose user: "0:0"），网关以 app(10001) 运行；
// 面板保存凭证走 rename，会把属主从 app 改成 root → 网关此后每次写该文件
// （token 刷新 / realm backfill）都 permission denied。
func TestOwnerPreservedWhenUnconfigured(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("需要 root 才能把文件 chown 成其他属主以复现")
	}
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := &Account{UID: "uid-preserve-owner", AccessToken: "at"}
	p := s.PathFor(a.UID)
	// 先建出文件并改成 10001:10001（模拟网关自建、属主为 app 的凭证）。
	if err := os.WriteFile(p, []byte(`{"account":{"uid":"uid-preserve-owner"},"auth":{"accessToken":"old"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(p, 10001, 10001); err != nil {
		t.Skipf("chown 不可用: %v", err)
	}
	// 未配置 SetOwner（默认 -1/-1）。
	if err := s.Save(a); err != nil {
		t.Fatalf("Save: %v", err)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	uid, gid, ok := fsutil.FileOwner(st)
	if !ok {
		t.Skip("非 Unix 系统")
	}
	if uid != 10001 || gid != 10001 {
		t.Errorf("原属主应被保留为 10001:10001，得到 %d:%d（rename 把属主改成了面板进程）", uid, gid)
	}
}

// TestSaveFollowsSymlink 凭证文件是符号链接时，写**链接目标**、保留符号链接本身。
func TestSaveFollowsSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.json")
	if err := os.WriteFile(real, []byte(`{"account":{"uid":"symuid123456"},"auth":{"accessToken":"old"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "workbuddy-symuid123456.json")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	a := &Account{UID: "symuid123456", AccessToken: "new", FilePath: link}
	if err := s.Save(a); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link should stay a symlink: err=%v", err)
	}
	raw, err := os.ReadFile(real)
	if err != nil {
		t.Fatalf("read real: %v", err)
	}
	if b, err := Parse(raw); err != nil || b.AccessToken != "new" {
		t.Errorf("real target not updated: %+v err=%v", b, err)
	}
}

// TestSaveFallsBackWhenDirUnwritable 目录不可写（tmp 建不出）但文件可写时，
// Save 退化为原地重写并成功。
func TestSaveFallsBackWhenDirUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 无视目录写权限")
	}
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := &Account{UID: "uid-fallback", AccessToken: "old"}
	if err := s.Save(a); err != nil {
		t.Fatalf("initial Save: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	a.AccessToken = "new"
	if err := s.Save(a); err != nil {
		t.Fatalf("Save with unwritable dir should fall back in-place: %v", err)
	}
	got, err := s.Get(a.UID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AccessToken != "new" {
		t.Errorf("fallback did not update: %q", got.AccessToken)
	}
}
