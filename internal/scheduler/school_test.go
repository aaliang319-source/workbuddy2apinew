package scheduler

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeScriptExec 记录命令构建参数并按需模拟执行失败，替代真实 exec 拉起 python3 子进程。
type fakeScriptExec struct {
	lastName string
	lastArgs []string
	lastDir  string
	runN     int
	err      error
}

func (f *fakeScriptExec) SetDir(dir string) { f.lastDir = dir }
func (f *fakeScriptExec) Run() error        { f.runN++; return f.err }

// installFakeExec 替换 newScriptCmd，测试结束还原。
func installFakeExec(t *testing.T) *fakeScriptExec {
	t.Helper()
	f := &fakeScriptExec{}
	orig := newScriptCmd
	newScriptCmd = func(name string, args ...string) scriptRunner {
		f.lastName, f.lastArgs = name, args
		return f
	}
	t.Cleanup(func() { newScriptCmd = orig })
	return f
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestNextWakeSchoolSlot 开学季任务在 school_hours（默认 12 点）处有独立时点。
func TestNextWakeSchoolSlot(t *testing.T) {
	s := New(Config{
		CheckinHours:      []int{9},
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		SchoolHours:       []int{12},
	})
	now := time.Date(2026, 9, 14, 11, 0, 0, 0, time.Local)
	at, kinds := s.nextWake(now)
	assertJittered(t, "school 12 点独立时点", at, now, 12)
	if len(kinds) != 1 || kinds[0] != taskSchool {
		t.Errorf("kinds=%v want [school]", kinds)
	}
}

// TestNextWakeCatSlot 夜猫子任务在 cat_hours（默认 1 点）处有独立时点。
func TestNextWakeCatSlot(t *testing.T) {
	s := New(Config{
		CheckinHours:      []int{9},
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		CatHours:          []int{1},
	})
	now := time.Date(2026, 9, 14, 23, 0, 0, 0, time.Local)
	at, kinds := s.nextWake(now)
	assertJittered(t, "cat 次日 01 点独立时点", at, now.AddDate(0, 0, 1), 1)
	if len(kinds) != 1 || kinds[0] != taskCat {
		t.Errorf("kinds=%v want [cat]", kinds)
	}
	// 夜猫的硬约束：落点必须在 23:00-08:00 CST 窗口内，否则脚本侧 skip pending 白跑。
	assertCatInNightWindow(t, at)
}

// assertCatInNightWindow 断言 t 落在夜猫窗口 [23:00, 08:00) 内——与
// scripts/task_runner.py 的 within_night_window() 同口径。抖动一旦放宽到跨小时，
// 这里会先炸，避免问题只在运行时的 "skip pending" 日志里暴露。
func assertCatInNightWindow(t *testing.T, at time.Time) {
	t.Helper()
	h := at.Hour()
	if !(h >= catWindowStartHour || h < catWindowEndHour) {
		t.Errorf("cat 触发时刻 %v（%d 点）出夜猫窗口 [23:00,08:00)——脚本会 skip pending", at, h)
	}
}

// TestNextFireCatStaysInNightWindow 夜猫窗口不变量：任何 cat_hours 配置下，
// 抖动后的落点都不得越窗。这是本次加抖动最容易踩坏的一条约束。
func TestNextFireCatStaysInNightWindow(t *testing.T) {
	for day := 1; day <= 28; day++ {
		for _, h := range []int{1, 3, 23} {
			// 从窗口内的前一个小时起算。
			prev := h - 1
			if h == 23 {
				prev = 22
			}
			now := time.Date(2026, 10, day, prev, 30, 0, 0, time.Local)
			at := testRnd().nextFire(now, []int{h})
			if at.IsZero() {
				t.Fatalf("day=%d h=%d: 无 cat 时点", day, h)
			}
			if at.Hour() != h {
				t.Fatalf("day=%d h=%d: next=%v 不在目标小时", day, h, at)
			}
			assertCatInNightWindow(t, at)
		}
	}
}

// TestNextWakeSchoolCatDisabled 显式禁用 school/cat 后排程只剩签到时点（互不影响）。
func TestNextWakeSchoolCatDisabled(t *testing.T) {
	s := New(Config{
		CheckinHours:      []int{21},
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		SchoolHours:       []int{12},
		CatHours:          []int{1},
		SchoolDisabled:    true,
		CatDisabled:       true,
	})
	now := time.Date(2026, 9, 14, 8, 0, 0, 0, time.Local)
	at, kinds := s.nextWake(now)
	assertJittered(t, "school/cat 禁用 → 只有签到 21 点", at, now, 21)
	if len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Errorf("kinds=%v want [checkin]", kinds)
	}
}

// TestRunSchoolNowBuildsCommand RunSchoolNow 构造
// python3 scripts/school_open_day_2026.py ALL --run --yes，工作目录设为仓库根。
func TestRunSchoolNowBuildsCommand(t *testing.T) {
	// 防环境泄漏：WB2A_PYTHON 若在测试机已设置会改写 pythonCmd()，使默认值断言失败。
	t.Setenv("WB2A_PYTHON", "")
	f := installFakeExec(t)
	s := New(Config{})
	s.RunSchoolNow()
	if f.lastName != "python3" {
		t.Errorf("name=%q want python3", f.lastName)
	}
	want := []string{"scripts/school_open_day_2026.py", "ALL", "--run", "--yes"}
	if !equalArgs(f.lastArgs, want) {
		t.Errorf("args=%v want %v", f.lastArgs, want)
	}
	if f.lastDir != repoRoot() {
		t.Errorf("dir=%q want repo root %q", f.lastDir, repoRoot())
	}
	if _, err := os.Stat(filepath.Join(f.lastDir, "scripts", "school_open_day_2026.py")); err != nil {
		t.Errorf("仓库根 %q 内应有 scripts/school_open_day_2026.py: %v", f.lastDir, err)
	}
}

// TestRunCatNowBuildsCommand RunCatNow 构造
// python3 scripts/task_runner.py ALL --yes --only black_cat，工作目录为仓库根。
func TestRunCatNowBuildsCommand(t *testing.T) {
	t.Setenv("WB2A_PYTHON", "")
	f := installFakeExec(t)
	s := New(Config{})
	s.RunCatNow()
	want := []string{"scripts/task_runner.py", "ALL", "--yes", "--only", "black_cat"}
	if f.lastName != "python3" || !equalArgs(f.lastArgs, want) {
		t.Errorf("cmd=%s %v want python3 %v", f.lastName, f.lastArgs, want)
	}
	if f.lastDir != repoRoot() {
		t.Errorf("dir=%q want repo root %q", f.lastDir, repoRoot())
	}
}

// TestDispatchSchoolCatAndFailureWarnsOnly dispatch 把 school/cat 分发给对应脚本；
// 脚本失败只记 WARN（不 panic/不向上抛），且不影响后续任务继续分发。
func TestDispatchSchoolCatAndFailureWarnsOnly(t *testing.T) {
	t.Setenv("WB2A_PYTHON", "")
	f := installFakeExec(t)
	f.err = errors.New("boom boom")
	s := New(Config{})

	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})

	s.dispatch(context.Background(), taskSchool)
	if f.runN != 1 || f.lastArgs[0] != "scripts/school_open_day_2026.py" {
		t.Errorf("dispatch(school) 未执行: runN=%d last=%v", f.runN, f.lastArgs)
	}
	s.dispatch(context.Background(), taskCat)
	if f.runN != 2 || f.lastArgs[0] != "scripts/task_runner.py" {
		t.Errorf("dispatch(cat) 未执行: runN=%d last=%v", f.runN, f.lastArgs)
	}
	out := buf.String()
	if !strings.Contains(out, "WARN") || !strings.Contains(out, "scripts/school_open_day_2026.py") {
		t.Errorf("school 失败未按 WARN 记录:\n%s", out)
	}
	if !strings.Contains(out, "scripts/task_runner.py") {
		t.Errorf("cat 失败未按 WARN 记录:\n%s", out)
	}
}

// TestPythonCmd WB2A_PYTHON 覆盖解释器名：缺省/空白回落 "python3"（保持
// 容器与既有测试的行为不变），显式设置时取其值（Windows 等仅有 python 的环境）。
func TestPythonCmd(t *testing.T) {
	t.Setenv("WB2A_PYTHON", "")
	if got := pythonCmd(); got != "python3" {
		t.Errorf("default pythonCmd()=%q want python3", got)
	}

	t.Setenv("WB2A_PYTHON", "   ")
	if got := pythonCmd(); got != "python3" {
		t.Errorf("blank pythonCmd()=%q want python3", got)
	}

	t.Setenv("WB2A_PYTHON", "python")
	if got := pythonCmd(); got != "python" {
		t.Errorf("override pythonCmd()=%q want python", got)
	}

	t.Setenv("WB2A_PYTHON", "  /usr/bin/python3.10  ")
	if got := pythonCmd(); got != "/usr/bin/python3.10" {
		t.Errorf("trim pythonCmd()=%q want /usr/bin/python3.10", got)
	}
}
