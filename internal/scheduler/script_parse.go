// script_parse.go 脚本类任务（school/cat）的输出 → 逐账号结构化结果。
//
// 为什么需要：脚本子进程在 runScript 里此前只产出「一条
// Outcome{Status:ok, Message:ok} + 输出尾部文本」，于是
//   - 运行历史详情的"账号"列只有一行 "—/ok/ok"，逐账号的
//     「执行了 / 未执行 / 失败 / 为什么」全埋在自由文本里；
//   - 列表页计数退化成"子进程退出码"级别：实测 cat 一次运行真实为
//     1 跳过 + 2 失败，历史却记成 成功 1 / 失败 0 / 跳过 0，完全误导。
//
// 做法：在输出被 tailLines 截断**之前**，解析脚本既有的行格式（不要求脚本改代码）。
// 解析依赖脚本的两个既有约定（见 scripts/school_open_day_2026.py、scripts/task_runner.py）：
//  1) 账号级行都带 8 位 uid 前缀：`[school2026] <uid8> ...` / `[task_runner] <uid8> ...` /
//     `[skip] <uid8> ...`；失败行可能再带 `ERR: ` 前缀（正则只看中括号部分，不受影响）；
//  2) 结束时打印汇总行 `<name> done: accounts=N ok=.. already=.. skipped=.. pending=.. fail=..`。
//
// 降级策略：解析不出任何账号级行（脚本改了格式 / 输出为空）→ 返回 nil，
// 调用方退回"整批一条"的旧行为：退化而非变差。
//
// 汇总行只用于交叉校验，不用于生成结果：脚本的计数是"每次任务动作一行"级别的，
// 而面板要求的是"每账号一条"的汇总，两者口径不同不能直接对账；能对的是
// 「失败有没有被漏掉」与「账号数够不够」——不一致时给 parse_warning 提示明细可能
// 被输出上限截断，绝不篡改逐账号结果本身。
package scheduler

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// 账号级类别权重：一个账号可能有多行结果（school 每账号 7 个任务），
// 汇总时取权重最高者作为该账号结论——"有没有失败"优先于"有没有做成"，
// 优先于"已领过"，最后才是"跳过"。权重相等取首次出现的行。
const (
	wSkipped = 1
	wAlready = 2
	wOK      = 3
	wFail    = 4
)

var (
	// 账号级行：`[school2026] 1a2b3c4d 后面是说明` / `[task_runner] ...` / `[skip] ...`
	reScriptAcctLine = regexp.MustCompile(`\[(?:school2026|task_runner|skip)\]\s+([0-9a-fA-F]{8})\b(.*)$`)
	// task_runner 的账号分节头：`== 1a2b3c4d (昵称) ==`（昵称是此处唯一的来源）
	reScriptSection = regexp.MustCompile(`^==\s+([0-9a-fA-F]{8})\s+\(([^)]*)\)\s+==`)
	// 脚本声明的账号清单（task_runner 首行 `accounts=['1a2b3c4d', ...]`）：
	// 用于识别"声明了但一条结果都没有"的账号 → unreached。
	reScriptDeclared = regexp.MustCompile(`accounts=\[([^\]]*)\]`)
	// 汇总行 `... done: accounts=N ok=.. already=.. skipped=.. pending=.. fail=..`
	reScriptSummary = regexp.MustCompile(`\bdone:\s*accounts=(\d+)\s*(.*)$`)
	reScriptKV      = regexp.MustCompile(`([a-z]+)=(\d+)`)
	// 收益：task_runner 的 `claim 200 ok(credit=+5 energy=+2)`
	reScriptCredit = regexp.MustCompile(`credit=\+(\d+)`)
	// 单账号说明的展示上限：详情表里一行能看全，完整原文仍在 run.output。
	scriptItemMsgMax = 160
)

// scriptAcct 解析中的单账号累计结果。
type scriptAcct struct {
	uid8 string
	nick string
	// 各行类别计数（同一账号多任务时用于拼 "另有 N 项…" 的说明）
	nOK, nAlready, nSkipped, nFail int
	// 首个命中各类别的原始行（去前缀、截断后即 Message）；
	// firstOK 取"最像结论"的那条（见 scriptOKLineScore），故配一个得分。
	firstOK, firstAlready, firstSkipped, firstFail string
	okScore                                        int
	reward                                         int64
	declared                                       bool // 出现在脚本声明的账号清单里
}

// class 该账号当前的汇总类别（按权重取最高；全无结果时返回 0）。
func (a *scriptAcct) class() int {
	switch {
	case a.nFail > 0:
		return wFail
	case a.nOK > 0:
		return wOK
	case a.nAlready > 0:
		return wAlready
	case a.nSkipped > 0:
		return wSkipped
	}
	return 0
}

// message 汇总说明：命中类别的首行 + 其它类别的数量提示（信息密度优先，但仍是一行）。
func (a *scriptAcct) message() string {
	var base string
	switch a.class() {
	case wFail:
		return clipMsg(a.firstFail)
	case wOK:
		base = a.firstOK
	case wAlready:
		base = a.firstAlready
	case wSkipped:
		base = a.firstSkipped
	}
	var extra []string
	switch a.class() {
	case wOK:
		if a.nOK > 1 {
			extra = append(extra, fmt.Sprintf("成功 %d 项", a.nOK))
		}
		if a.nAlready > 0 {
			extra = append(extra, fmt.Sprintf("已领 %d 项", a.nAlready))
		}
		if a.nSkipped > 0 {
			extra = append(extra, fmt.Sprintf("跳过 %d 项", a.nSkipped))
		}
	case wAlready:
		if a.nSkipped > 0 {
			extra = append(extra, fmt.Sprintf("跳过 %d 项", a.nSkipped))
		}
	}
	if len(extra) == 0 {
		return clipMsg(base)
	}
	return clipMsg(base) + "（本次 " + strings.Join(extra, "、") + "）"
}

// classifyScriptLine 按脚本既有措辞判定单行类别；无法判定时返回 0（中性行，
// 例如 `viewed 激活 pending->in_progress`、`lottery balance=...`，不改变账号结论）。
//
// 顺序有意义：先判失败（"回读任务失败，跳过" 同时含 失败 与 跳过），
// 再判已领（"已完成/已领，跳过" 同时含 已领 与 跳过），最后才判跳过。
func classifyScriptLine(body string) int {
	upper := strings.ToUpper(body)
	switch {
	case strings.Contains(body, "失败"),
		strings.Contains(upper, "ERR"),
		strings.Contains(upper, "TRACEBACK"),
		strings.Contains(body, "http=-1"):
		return wFail
	case strings.Contains(body, "已完成/已领"),
		strings.Contains(body, "已领"),
		strings.Contains(body, "already"):
		return wAlready
	case strings.Contains(body, "跳过"),
		strings.Contains(body, "skip"),
		strings.Contains(body, "人工环节"),
		strings.Contains(body, "非进行期"),
		strings.Contains(body, "不适用"),
		strings.Contains(body, "未变化"),
		strings.Contains(body, "无分类证据"):
		return wSkipped
	// 成功只认"有实际动作/结论"的措辞：`点亮`、`claim`、`report`、`share-complete`、
	// `draw`、以及带 200/code=0 的上报行。过程性提示（viewed 激活 pending->in_progress、
	// lottery balance=…）刻意不认，返回 0——它们出现在成功行之前，认了会让
	// Message 变成"看起来什么都没做成"的过程描述。
	case strings.Contains(body, "点亮"),
		strings.Contains(body, "claim"),
		strings.Contains(body, "report"),
		strings.Contains(body, "share-complete"),
		strings.Contains(body, "draw"),
		strings.Contains(body, "http=200"),
		strings.Contains(body, "code=0"):
		return wOK
	}
	return 0
}

// scriptOKLineScore 在若干成功行里挑"最能当结论"的那条写进 Message：
// 点亮 / 领奖 / 上报结果才是结果，`viewed 激活 pending->in_progress` 只是过程性提示
// （消息里先出现过程行会让"这个账号到底做成没有"更难判断）。
func scriptOKLineScore(body string) int {
	switch {
	case strings.Contains(body, "点亮"):
		return 4
	case strings.Contains(body, "claim"):
		return 3
	case strings.Contains(body, "share-complete"),
		strings.Contains(body, "report"),
		strings.Contains(body, "draw"):
		return 2
	}
	return 1
}

// parseScriptOutput 解析脚本输出为逐账号结果。
//
// 返回值：items 为按脚本出现顺序排列的逐账号结果（含 unreached 未执行账号）；
// warn 非空表示与脚本自己的汇总行对不上（明细可能被输出上限截断）。
// 无任何账号级线索时返回 (nil, "")，调用方据此退回旧行为。
func parseScriptOutput(raw string) ([]Outcome, string) {
	if strings.TrimSpace(raw) == "" {
		return nil, ""
	}
	byUID := map[string]*scriptAcct{}
	var order []string
	summary := map[string]int{}
	summaryDeclared := 0

	// touch 取（或登记）账号槽位；登记顺序即输出顺序——脚本声明了账号清单时
	// 以清单顺序为准（未执行账号因此留在它本该出现的位置，而不是被甩到末尾）。
	touch := func(uid8 string) *scriptAcct {
		if a, ok := byUID[uid8]; ok {
			return a
		}
		a := &scriptAcct{uid8: uid8}
		byUID[uid8] = a
		order = append(order, uid8)
		return a
	}

	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		if m := reScriptSection.FindStringSubmatch(line); m != nil {
			touch(m[1]).nick = strings.TrimSpace(m[2])
			continue
		}
		if m := reScriptDeclared.FindStringSubmatch(line); m != nil {
			for _, tok := range strings.Split(m[1], ",") {
				tok = strings.Trim(strings.TrimSpace(tok), `'"`)
				if len(tok) >= 8 {
					tok = tok[:8]
				}
				if reScriptUID8.MatchString(tok) {
					touch(tok).declared = true
				}
			}
			// 声明行同时可能是汇总行以外的普通首行，继续往下走（不做 continue）。
		}
		if m := reScriptSummary.FindStringSubmatch(line); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				summaryDeclared = n
			}
			for _, kv := range reScriptKV.FindAllStringSubmatch(m[2], -1) {
				if v, err := strconv.Atoi(kv[2]); err == nil {
					summary[kv[1]] = v
				}
			}
			continue
		}
		m := reScriptAcctLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		body := strings.TrimSpace(m[2])
		a := touch(m[1])
		if c := reScriptCredit.FindStringSubmatch(body); c != nil {
			if v, err := strconv.ParseInt(c[1], 10, 64); err == nil {
				a.reward += v
			}
		}
		switch classifyScriptLine(body) {
		case wFail:
			a.nFail++
			if a.firstFail == "" {
				a.firstFail = body
			}
		case wOK:
			a.nOK++
			if s := scriptOKLineScore(body); a.firstOK == "" || s > a.okScore {
				a.firstOK, a.okScore = body, s
			}
		case wAlready:
			a.nAlready++
			if a.firstAlready == "" {
				a.firstAlready = body
			}
		case wSkipped:
			a.nSkipped++
			if a.firstSkipped == "" {
				a.firstSkipped = body
			}
		}
	}

	if len(order) == 0 {
		return nil, ""
	}

	items := make([]Outcome, 0, len(order))
	var nOK, nAlready, nSkipped, nFail, nUnreached int
	for _, uid8 := range order {
		a := byUID[uid8]
		it := Outcome{UID: uid8, Nickname: a.nick, Reward: a.reward}
		switch cls := a.class(); cls {
		case wFail:
			it.OK, it.Status, it.Message = false, StatusFail, a.message()
			nFail++
		case wOK:
			it.OK, it.Status, it.Message = true, StatusOK, a.message()
			nOK++
		case wAlready:
			it.OK, it.Status, it.Message = true, StatusAlready, a.message()
			nAlready++
		case wSkipped:
			it.OK, it.Status, it.Message = false, StatusSkipped, a.message()
			nSkipped++
		default:
			// 声明了账号（或只出现在分节头里）但一条结果都没有：脚本没跑到它。
			it.OK, it.Status = false, StatusUnreached
			if a.declared {
				it.Message = "脚本声明要处理该账号，但输出里没有任何结果（可能超出输出显示上限，或执行中途中断）"
			} else {
				it.Message = "脚本输出中没有该账号的结果（可能超出输出显示上限，或执行中途中断）"
			}
			nUnreached++
		}
		items = append(items, it)
	}

	// 汇总行交叉校验：只查"漏没漏"。
	//
	// 刻意不比对每类动作计数：脚本按"每次任务动作"计数（school 一个账号 7 个任务
	// 就有 7 条），本解析按"每账号一条"汇总，两个口径天然对不上，硬比会天天误报。
	// 可靠的两个信号是：①脚本说有失败但解析不出失败账号；②声明处理的账号数 > 解析出的
	// 账号数（输出被 tailLines 之外的展示上限截断、或中途退出）。
	var warns []string
	if f := summary["fail"]; f > 0 && nFail == 0 {
		warns = append(warns, fmt.Sprintf("脚本汇总 fail=%d，但未解析出失败账号", f))
	}
	wantAccounts := summaryDeclared
	if wantAccounts == 0 {
		wantAccounts = summary["accounts"]
	}
	if wantAccounts > 0 && len(items) < wantAccounts {
		warns = append(warns, fmt.Sprintf("脚本处理 %d 个账号，仅解析出 %d 个（逐账号明细可能被输出显示上限截断）",
			wantAccounts, len(items)))
	} else if nUnreached > 0 {
		warns = append(warns, fmt.Sprintf("%d 个账号未返回结果", nUnreached))
	}
	return items, strings.Join(dedupStrings(warns), "；")
}

// clipMsg 截断单条说明（详情表一行可读；完整原文仍在 run.output）。
func clipMsg(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len([]rune(s)) <= scriptItemMsgMax {
		return s
	}
	return string([]rune(s)[:scriptItemMsgMax]) + "…"
}

// dedupStrings 去重保序（交叉校验可能从多个角度得出同一条警告）。
func dedupStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// reScriptUID8 判定 token 是否长得像 uid 前缀（8 位十六进制）。
var reScriptUID8 = regexp.MustCompile(`^[0-9a-fA-F]{8}$`)
