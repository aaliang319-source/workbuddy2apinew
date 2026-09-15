// outcome_test.go 导出契约与脚本捕获的回归测试。
package scheduler

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestNextWakeExportedEquivalence 导出纯函数 NextWake 与内部 nextWake 行为一致
// （排程语义单一来源的发布门槛：automation 模块依赖 NextWake，绝不能与网关
// 真实循环漂移）。
func TestNextWakeExportedEquivalence(t *testing.T) {
	specs := []Config{
		{}, // 全默认
		{CheckinHours: []int{9}, TravelHours: []int{9}, ActivityDisabled: true, KeepaliveDisabled: true, SchoolHours: []int{12}, CatHours: []int{1}},
		{CheckinDisabled: true, TravelDisabled: true, ActivityDisabled: true, KeepaliveDisabled: true, SchoolDisabled: true, CatDisabled: true}, // 全禁用
		{CheckinHours: []int{0}, TravelHours: []int{23}, ActivityHours: []int{23}},                                                              // 跨日 + 同槽合并
		{CheckinHours: []int{5}, TravelHours: []int{7}, ActivityHours: []int{9}, KeepaliveHours: []int{11}, SchoolHours: []int{13}, CatHours: []int{15}},
	}
	times := []time.Time{
		time.Date(2026, 9, 18, 8, 30, 0, 0, time.Local),
		time.Date(2026, 9, 18, 23, 59, 59, 0, time.Local),
		time.Date(2026, 9, 18, 0, 0, 0, 0, time.Local),
	}
	for i, cfg := range specs {
		s := New(cfg)
		for j, now := range times {
			wantAt, wantKinds := s.nextWake(now)
			// 口径：运行时 spec 一定经过 New 归一（main.go 用 sch.Spec()），故用 s.Spec() 对比。
			gotAt, gotKinds := NextWake(now, s.Spec())
			if !wantAt.Equal(gotAt) {
				t.Errorf("case %d/%d: at=%v want %v", i, j, gotAt, wantAt)
				continue
			}
			if len(wantKinds) != len(gotKinds) {
				t.Errorf("case %d/%d: kinds=%v want %v", i, j, gotKinds, wantKinds)
				continue
			}
			for idx, tk := range wantKinds {
				if gotKinds[idx] != taskToKind(tk) {
					t.Errorf("case %d/%d: kinds[%d]=%v want %v", i, j, idx, gotKinds[idx], tk)
				}
			}
		}
	}
}

// captureFake 实现 ctxRunner（生产路径）的脚本 fake：可配置输出/错误/执行耗时。
type captureFake struct {
	dir    string
	output string
	err    error
	sleep  time.Duration
}

func (f *captureFake) SetDir(d string) { f.dir = d }
func (f *captureFake) Run() error {
	_, err := f.RunContext(context.Background())
	return err
}
func (f *captureFake) RunContext(ctx context.Context) (string, error) {
	if f.sleep > 0 {
		select {
		case <-ctx.Done():
			return f.output, ctx.Err()
		case <-time.After(f.sleep):
		}
	}
	return f.output, f.err
}

// installCaptureFake 替换 newScriptCmd 为可捕获 fake，测试结束还原。
func installCaptureFake(t *testing.T, f *captureFake) {
	t.Helper()
	orig := newScriptCmd
	newScriptCmd = func(name string, args ...string) scriptRunner {
		return f
	}
	t.Cleanup(func() { newScriptCmd = orig })
}

func TestRunScriptCaptureSuccess(t *testing.T) {
	f := &captureFake{output: "line1\nline2\nline3"}
	installCaptureFake(t, f)
	s := New(Config{ScriptTimeout: time.Second})
	outs := s.RunSchoolCtx(context.Background())
	if len(outs) != 1 {
		t.Fatalf("outcomes=%d want 1", len(outs))
	}
	oc := outs[0]
	if !oc.OK || oc.Status != "ok" || oc.ExitCode == nil || *oc.ExitCode != 0 {
		t.Errorf("outcome=%+v want ok/exit 0", oc)
	}
	if !strings.Contains(oc.Output, "line3") {
		t.Errorf("output tail missing: %q", oc.Output)
	}
}

func TestRunScriptCaptureFailure(t *testing.T) {
	f := &captureFake{output: "boom stdout", err: errors.New("boom")}
	installCaptureFake(t, f)
	s := New(Config{ScriptTimeout: time.Second})
	outs := s.RunCatCtx(context.Background())
	oc := outs[0]
	if oc.OK || oc.Status != "fail" {
		t.Errorf("outcome=%+v want fail", oc)
	}
	if oc.ExitCode == nil || *oc.ExitCode != 1 {
		t.Errorf("exit_code=%v want 1（非 ExitError 的失败统一记 1）", oc.ExitCode)
	}
	if !strings.Contains(oc.Output, "boom stdout") {
		t.Errorf("output tail missing: %q", oc.Output)
	}
}

// TestRunScriptTailTruncation 输出超行数时只保留尾部 n 行。
func TestRunScriptTailTruncation(t *testing.T) {
	var lines []string
	for i := 1; i <= 120; i++ {
		lines = append(lines, "line-"+strconv.Itoa(i))
	}
	f := &captureFake{output: strings.Join(lines, "\n")}
	installCaptureFake(t, f)
	s := New(Config{ScriptTimeout: time.Second})
	outs := s.RunCatCtx(context.Background())
	out := outs[0].Output
	got := strings.Count(out, "\n") + 1
	if got > scriptOutputTailLines {
		t.Errorf("tail lines=%d want <= %d", got, scriptOutputTailLines)
	}
	if !strings.Contains(out, "line-120") {
		t.Error("tail 应保留最后一行")
	}
	if strings.Contains(out, "line-1\n") {
		t.Error("tail 不应保留最旧的行")
	}
}

// TestRunScriptTimeout 超时杀进程：错误带 timeout 语义、结果记 fail。
func TestRunScriptTimeout(t *testing.T) {
	f := &captureFake{sleep: 2 * time.Second, output: "partial"}
	installCaptureFake(t, f)
	s := New(Config{ScriptTimeout: 50 * time.Millisecond})
	start := time.Now()
	outs := s.RunSchoolCtx(context.Background())
	if time.Since(start) > time.Second {
		t.Errorf("超时未生效（耗时 %v）", time.Since(start))
	}
	oc := outs[0]
	if oc.OK || oc.Status != "fail" {
		t.Errorf("outcome=%+v want fail", oc)
	}
	if oc.ExitCode == nil || *oc.ExitCode != 1 {
		t.Errorf("exit_code=%v want 1", oc.ExitCode)
	}
	if !strings.Contains(oc.Message, "timeout") {
		t.Errorf("message=%q want timeout", oc.Message)
	}
}

// TestRunScriptLegacyFakeFallback 旧版 fake（不实现 ctxRunner）回退 Run() 不 panic，
// 日志与返回值语义与引入前一致。
func TestRunScriptLegacyFakeFallback(t *testing.T) {
	installFakeExec(t)
	s := New(Config{ScriptTimeout: time.Second})
	outs := s.RunCatCtx(context.Background())
	if len(outs) != 1 || !outs[0].OK {
		t.Fatalf("outcome=%+v want ok", outs)
	}
	if outs[0].Output != "" {
		t.Errorf("legacy fake 不应有捕获输出: %q", outs[0].Output)
	}
	if outs[0].ExitCode == nil || *outs[0].ExitCode != 0 {
		t.Errorf("exit_code=%v want 0", outs[0].ExitCode)
	}
}
