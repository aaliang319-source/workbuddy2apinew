// pool_addmissing_test.go AddMissing（运行期自动发现）语义：只追加新 uid，
// 既不覆盖既有凭证，也不删除消失账号。
package pool

import (
	"testing"

	"workbuddy2api/internal/auth"
)

func TestAddMissingOnlyAddsNewUIDs(t *testing.T) {
	p := New("")
	existing := &auth.Auth{UID: "u1", AccessToken: "at-memory"}
	p.Add(existing)

	// 同 uid 但磁盘凭证不同：AddMissing 必须跳过（不覆盖内存凭证）。
	fresh := []*auth.Auth{
		{UID: "u1", AccessToken: "at-disk-stale"},
		{UID: "u2", AccessToken: "at-new"},
	}
	if n := p.AddMissing(fresh); n != 1 {
		t.Fatalf("added=%d want 1（u1 已存在跳过）", n)
	}

	// u1 凭证保持内存版本；u2 已进池。
	p1, ok := p.byUID["u1"]
	if !ok || p1.a.AccessToken != "at-memory" {
		t.Errorf("existing credential overwritten: %+v", p1.a)
	}
	if p2, ok := p.byUID["u2"]; !ok || p2.a.AccessToken != "at-new" {
		t.Errorf("new account not added: ok=%v", ok)
	}

	// 重复扫描：零新增（幂等）。
	if n := p.AddMissing(fresh); n != 0 {
		t.Errorf("second scan added=%d want 0", n)
	}
}

func TestAddMissingEmpty(t *testing.T) {
	p := New("")
	if n := p.AddMissing(nil); n != 0 {
		t.Errorf("added=%d want 0", n)
	}
}
