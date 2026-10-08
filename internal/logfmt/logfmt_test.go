package logfmt

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// TestWarnOnce 同一 key 只输出一次；不同 key 各自独立输出。
func TestWarnOnce(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() { log.SetOutput(old); log.SetFlags(oldFlags) }()

	WarnOnce("k-a", "WARN: first %d", 1)
	WarnOnce("k-a", "WARN: first %d", 1) // 重复：应被吞掉
	WarnOnce("k-b", "WARN: other")

	got := buf.String()
	if n := strings.Count(got, "WARN: first"); n != 1 {
		t.Errorf("key k-a printed %d times want 1; out=%q", n, got)
	}
	if !strings.Contains(got, "WARN: other") {
		t.Errorf("key k-b should print; out=%q", got)
	}
}

func TestUID8(t *testing.T) {
	tests := []struct {
		name string
		uid  string
		want string
	}{
		{"full uuid truncated to 8", "5895a56c-449f-49e7-b723-8ddf10a97cdb", "5895a56c"},
		{"exactly 8 chars unchanged", "00e26541", "00e26541"},
		{"short uid unchanged", "abc", "abc"},
		{"empty uid returns dash", "", "-"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange — tc.uid
			// Act
			got := UID8(tc.uid)
			// Assert
			if got != tc.want {
				t.Errorf("UID8(%q) = %q, want %q", tc.uid, got, tc.want)
			}
		})
	}
}
