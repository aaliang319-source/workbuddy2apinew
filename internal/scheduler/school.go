// school.go 开学季任务与夜猫子任务的脚本类排程：从系统 crontab 迁入 Go scheduler。
//
// 背景：school（12:00）与 cat（01:00 夜猫窗口）原由系统 crontab 调
// scripts/school_open_day_cron.sh 执行——依赖外部系统 cron、容器重建可能丢失、
// 不在 config 里配置。迁入后成为第五、第六类任务，时点由 schedule.school_hours /
// schedule.cat_hours 配置，school_open_day_cron.sh 保留为手动触发入口。
//
// 输出捕获：脚本 stdout/stderr 合并后取尾部若干行随 Outcome 回传（此前被丢弃，
// 失败只剩 "exit status N"，面板无从展示原因）。捕获通过可选接口 ctxRunner 实现，
// 测试注入的 fake 不实现该接口 → 自动退化为 Run()，既有测试断言零改动。
package scheduler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// scriptTimeoutDefault 脚本子进程默认超时（可由 Config.ScriptTimeout 覆盖）。
const scriptTimeoutDefault = 10 * time.Minute

// scriptOutputTailLines / scriptOutputMaxBytes 随运行记录回传的输出上限：
// 只保留尾部 N 行且不超过 8KB，避免历史文件被脚本刷屏撑爆。
const (
	scriptOutputTailLines = 50
	scriptOutputMaxBytes  = 8 << 10
)

// repoRoot 定位仓库根（容器内 /app、宿主 /root/workbuddy2api）。
// 策略：从当前工作目录逐级向上找 scripts/school_open_day_2026.py，
// 找不到回落 os.Getwd()（此时 Run 会因脚本缺失打 WARN，不 panic）。
// 注意：Go scheduler 在 cmd/server 内以工作目录启动（容器 WORKDIR /app），
// 若进程以别的工作目录拉起（如 systemd/裸 binary），上溯穷尽后仍以
// os.Getwd() 兜底，把缺失暴露成 WARN 而非静默。
func repoRoot() string {
	start, err := os.Getwd()
	if err != nil {
		return "."
	}
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, "scripts", "school_open_day_2026.py")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return start
		}
		dir = parent
	}
}

// scriptRunner 脚本子进程的最小执行面：可被测试替换，避免测试真正拉起 python3。
type scriptRunner interface {
	SetDir(string)
	Run() error
}

// ctxRunner 可选能力：仅生产实现（ctx 取消 + 输出捕获）。测试 fake 不实现，
// runScript 自动退化为 Run()（无捕获），保证既有测试断言逐字不变。
type ctxRunner interface {
	RunContext(ctx context.Context) (string, error)
}

// scriptCmd 脚本子进程：延迟构建 exec.Cmd（Run/RunContext 时才建），目录经 SetDir 注入。
type scriptCmd struct {
	program string
	args    []string
	dir     string
}

func (c *scriptCmd) SetDir(dir string) { c.dir = dir }

func (c *scriptCmd) Run() error {
	cmd := exec.Command(c.program, c.args...)
	cmd.Dir = c.dir
	return cmd.Run()
}

// RunContext 带 ctx 执行并合并捕获 stdout+stderr（超时由调用方的 ctx 控制）。
func (c *scriptCmd) RunContext(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, c.program, c.args...)
	cmd.Dir = c.dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// newScriptCmd 构建脚本子进程。包级变量便于测试注入 fake（installFakeExec 覆盖）。
// 工作目录由调用方 SetDir 显式设置仓库根。
var newScriptCmd = func(program string, args ...string) scriptRunner {
	return &scriptCmd{program: program, args: args}
}

// pythonCmd 返回执行 scripts/*.py 的解释器名。
//
// 默认 "python3"，与容器/Linux 现状完全一致，行为零变更；WB2A_PYTHON
// 显式指定时优先，供解释器不叫 python3 的环境使用（命名对齐仓库 Go 侧
// WB2A_* env 约定，如 WB2A_AUTH_DIR / WB2A_LISTEN）。
//
// 需要该开关的原因：Windows 官方安装器只提供 python.exe，且 PATH 上常存在
// Microsoft Store 的 python3.exe App Execution Alias 存根——exec.Command 能找到
// 它却无法真正执行，脚本类任务统一报 `exit status 9009`。
// 设 WB2A_PYTHON=python 即可绕过。
func pythonCmd() string {
	if v := strings.TrimSpace(os.Getenv("WB2A_PYTHON")); v != "" {
		return v
	}
	return "python3"
}

// runScript 依次执行若干脚本命令并返回逐命令结果：任一命令失败只记一行 WARN，
// 不向上抛、不影响调度主循环继续跑下一个时点。单命令失败不中断后续命令。
//
// 生产路径带 ctx + 超时并捕获输出尾部（Outcome.Output/ExitCode）；测试注入的 fake
// 不实现 ctxRunner → 退化为 Run()，日志与返回值语义与引入前一致。
func runScript(ctx context.Context, name, root string, timeout time.Duration, commands [][]string) []Outcome {
	if timeout <= 0 {
		timeout = scriptTimeoutDefault
	}
	out := make([]Outcome, 0, len(commands))
	for _, cmdArgs := range commands {
		c := newScriptCmd(cmdArgs[0], cmdArgs[1:]...)
		c.SetDir(root)
		var (
			raw string
			err error
		)
		if cr, ok := c.(ctxRunner); ok {
			cctx, cancel := context.WithTimeout(ctx, timeout)
			raw, err = cr.RunContext(cctx)
			if cctx.Err() == context.DeadlineExceeded {
				err = fmt.Errorf("timeout after %s", timeout)
			}
			cancel()
		} else {
			// 测试 fake：无捕获能力，行为与引入前一致
			err = c.Run()
		}
		tail := tailLines(raw, scriptOutputTailLines)
		oc := Outcome{Status: "ok", OK: true, Message: "ok"}
		if err != nil {
			log.Printf("WARN: %s (%s): %v", name, cmdArgs[1], err)
			oc.OK, oc.Status, oc.Message = false, "fail", err.Error()
			oc.ExitCode = exitCodeOf(err)
		} else {
			log.Printf("%s: ok (%s)", name, cmdArgs[1])
			oc.ExitCode = intPtr(0)
		}
		oc.Output = tail
		out = append(out, oc)
	}
	return out
}

// tailLines 取文本尾部最多 n 行且不超过 scriptOutputMaxBytes 字节（超出再从头部截断）。
func tailLines(s string, n int) string {
	s = strings.TrimRight(s, "\r\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	joined := strings.Join(lines, "\n")
	if len(joined) > scriptOutputMaxBytes {
		joined = "…(截断)\n" + joined[len(joined)-scriptOutputMaxBytes:]
	}
	return joined
}

// exitCodeOf 提取子进程退出码：*exec.ExitError 有真实码，其余失败记 1。
func exitCodeOf(err error) *int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return intPtr(ee.ExitCode())
	}
	return intPtr(1)
}

func intPtr(v int) *int { return &v }

// RunSchoolNow 立即执行开学季任务：school_open_day_2026.py ALL --run --yes。
// 全量跑任务点亮 + 领奖 + 自动抽空抽奖余额。活动下线（in_period=false）时脚本
// 各段全量跳过、正常退出，不视为失败。失败只记 WARN。
func (s *Scheduler) RunSchoolNow() []Outcome {
	return s.RunSchoolCtx(context.Background())
}

// RunSchoolCtx 带 ctx 的开学季任务（ctx 取消/超时即杀子进程）。
func (s *Scheduler) RunSchoolCtx(ctx context.Context) []Outcome {
	root := repoRoot()
	return runScript(ctx, "school", root, s.cfg.ScriptTimeout, [][]string{
		{pythonCmd(), "scripts/school_open_day_2026.py", "ALL", "--run", "--yes"},
	})
}

// RunCatNow 立即执行夜猫子任务：task_runner.py ALL --yes --only black_cat。
// black_cat 时段敏感：夜猫窗口 23:00–08:00 CST 内最多补 1 次（task_runner 内部
// 判定，非窗口期打印 skip 正常退出）。失败只记 WARN。
func (s *Scheduler) RunCatNow() []Outcome {
	return s.RunCatCtx(context.Background())
}

// RunCatCtx 带 ctx 的夜猫子任务（ctx 取消/超时即杀子进程）。
func (s *Scheduler) RunCatCtx(ctx context.Context) []Outcome {
	root := repoRoot()
	return runScript(ctx, "cat", root, s.cfg.ScriptTimeout, [][]string{
		{pythonCmd(), "scripts/task_runner.py", "ALL", "--yes", "--only", "black_cat"},
	})
}
