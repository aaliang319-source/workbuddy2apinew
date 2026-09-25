// script_parse_test.go 脚本输出解析的回归测试。
//
// 用**假 uid**（aaaaaaaa…）构造脚本真实输出形态，避免真实账号 UID/昵称进版本库
// （仓库历史上有过"移除测试 fixture 中真实账号 UID 与昵称"的清理提交，别再来一次）。
package scheduler

import (
	"strings"
	"testing"
)

// cat 形态：分节头带昵称、[skip] 行、ERR 失败行、汇总行齐全。
const catOutputSample = `mode=REAL accounts=['aaaaaaaa', 'bbbbbbbb', 'cccccccc'] only=['black_cat'] only_claim=False gap=1.0
[skip] cccccccc global realm 不适用 CN 任务
== aaaaaaaa (甲) ==
[task_runner] aaaaaaaa black_cat: query in_progress(1/3) -> 夜猫窗口内，可补 1 次
[task_runner] aaaaaaaa black_cat: report 1/2 200 code=0 id=obj-1
[task_runner] aaaaaaaa black_cat: claim 200 ok(credit=+5 energy=+2)
== bbbbbbbb (乙) ==
ERR: [task_runner] bbbbbbbb query list_tasks 失败: list_tasks http=-1
task_runner done: accounts=3 total=3 ok=1 already=0 skipped=1 pending=0 fail=1 credit=+5 energy=+2`

func TestParseScriptOutputCatShape(t *testing.T) {
	items, warn := parseScriptOutput(catOutputSample)
	if warn != "" {
		t.Errorf("warn=%q 期望空（汇总 fail=1 已解析出、账号数也对得上）", warn)
	}
	if len(items) != 3 {
		t.Fatalf("items=%d 期望 3（含 1 条跳过）", len(items))
	}
	byUID := map[string]Outcome{}
	for _, it := range items {
		byUID[it.UID] = it
	}
	if got := byUID["aaaaaaaa"]; got.Status != StatusOK || !got.OK || got.Nickname != "甲" || got.Reward != 5 {
		t.Errorf("aaaaaaaa: %+v 期望 ok/昵称=甲/收益=5", got)
	}
	if got := byUID["bbbbbbbb"]; got.Status != StatusFail || got.OK || !strings.Contains(got.Message, "list_tasks 失败") {
		t.Errorf("bbbbbbbb: %+v 期望 fail 且说明含失败原因", got)
	}
	if got := byUID["cccccccc"]; got.Status != StatusSkipped || got.OK || !strings.Contains(got.Message, "不适用") {
		t.Errorf("cccccccc: %+v 期望 skipped 且说明含原因", got)
	}
	// 顺序 = 账号清单顺序（脚本声明的处理顺序），未执行账号留在它本该出现的位置
	if items[0].UID != "aaaaaaaa" || items[1].UID != "bbbbbbbb" || items[2].UID != "cccccccc" {
		t.Errorf("顺序错乱: %v %v %v", items[0].UID, items[1].UID, items[2].UID)
	}
}

func TestParseScriptOutputSchoolMultiTaskRollup(t *testing.T) {
	// 一个账号 7 个任务：3 条成功 + 1 条已领 + 1 条人工环节跳过 + 2 条中性进度行。
	// 面板要的是"这个账号这次到底执行了没有"，故汇总取权重最高的 ok，
	// 说明用最像结论的那条（点亮/claim/report），并把其它类别数量带出来。
	out := `mode=RUN REAL gap=1.5
[school2026] aaaaaaaa expert_use: viewed 激活 pending->in_progress
[school2026] aaaaaaaa expert_use: pending/0 -> completed/1（点亮）
[school2026] aaaaaaaa expert_use: claim 200 code=0
[school2026] aaaaaaaa task_share: completed 已完成/已领，跳过
[school2026] aaaaaaaa task_student_verify: pending -> 人工环节（微信学生认证），跳过
school2026 done: accounts=1 ok=3 already=1 skipped=2 pending=0 fail=0`
	items, warn := parseScriptOutput(out)
	if len(items) != 1 {
		t.Fatalf("items=%d 期望 1（一账号一条）", len(items))
	}
	it := items[0]
	if it.Status != StatusOK || !it.OK {
		t.Fatalf("%+v 期望 ok（有成功动作优先于「跳过」）", it)
	}
	if !strings.Contains(it.Message, "点亮") {
		t.Errorf("message=%q 期望取结论性行（含「点亮」）而不是中性进度行", it.Message)
	}
	if !strings.Contains(it.Message, "已领 1 项") || !strings.Contains(it.Message, "跳过 1 项") {
		t.Errorf("message=%q 期望带出其它类别数量", it.Message)
	}
	if warn != "" {
		t.Errorf("warn=%q 期望空", warn)
	}
}

func TestParseScriptOutputUnreachedAccount(t *testing.T) {
	// 声明了 3 个账号，只有 2 个有结果 → 第 3 个必须显式标为"未执行"，
	// 这正是"看不出哪个没执行"的直接答案。
	out := `mode=REAL accounts=['aaaaaaaa', 'bbbbbbbb', 'cccccccc'] only=['black_cat']
== aaaaaaaa (甲) ==
[task_runner] aaaaaaaa black_cat: claim 200 ok(credit=+1 energy=+0)
task_runner done: accounts=3 total=1 ok=1 already=0 skipped=0 pending=0 fail=0 credit=+1 energy=+0`
	items, warn := parseScriptOutput(out)
	if len(items) != 3 {
		t.Fatalf("items=%d 期望 3", len(items))
	}
	var unreached []string
	for _, it := range items {
		if it.Status == StatusUnreached {
			unreached = append(unreached, it.UID)
		}
	}
	// bbbbbbbb 只出现过分节头（无结果行）→ 也算未执行
	if len(unreached) != 2 {
		t.Fatalf("未执行账号=%v 期望 2 个（bbbbbbbb 无结果行、cccccccc 未出现）", unreached)
	}
	if !strings.Contains(warn, "未返回结果") {
		t.Errorf("warn=%q 期望提示有账号未返回结果", warn)
	}
}

func TestParseScriptOutputWarnsOnMissingFailure(t *testing.T) {
	// 脚本说 fail=1，但逐账号行里一条失败都没有（明细被输出上限截断的典型形态）
	out := `[school2026] aaaaaaaa 活动非进行期（in_period=false），跳过
school2026 done: accounts=2 ok=0 already=0 skipped=1 pending=0 fail=1`
	_, warn := parseScriptOutput(out)
	if !strings.Contains(warn, "fail=1") {
		t.Errorf("warn=%q 期望指出脚本汇总有失败但未解析出", warn)
	}
	if !strings.Contains(warn, "截断") {
		t.Errorf("warn=%q 期望提示可能被输出上限截断", warn)
	}
}

func TestParseScriptOutputDegradesGracefully(t *testing.T) {
	// 脚本改格式/输出为空 → 返回 nil，调用方退回"整批一条"的旧行为（退化而非变差）
	for _, out := range []string{"", "   ", "mode=RUN REAL gap=1.5\n脚本换了一种完全不同的输出"} {
		items, warn := parseScriptOutput(out)
		if items != nil || warn != "" {
			t.Errorf("out=%q → items=%v warn=%q 期望 (nil, \"\")", out, items, warn)
		}
	}
}

func TestClassifyScriptLineOrdering(t *testing.T) {
	// 顺序敏感用例：同时含"失败/已领/跳过"的措辞必须判定成更重的那个
	cases := []struct {
		line string
		want int
	}{
		{"task_student_verify: 回读任务失败，跳过", wFail},
		{"claim 失败: boom（可稍后手动补领）", wFail},
		{"task_share: completed 已完成/已领，跳过", wAlready},
		{"query claimed(3/3) -> 已领，跳过", wAlready},
		{"task_student_verify: pending -> 人工环节（微信学生认证），跳过", wSkipped},
		{"活动非进行期（in_period=false），跳过", wSkipped},
		{"query 非映射任务，skip", wSkipped},
		{"query in_progress(1/3) -> 非夜猫窗口(23-08 CST)，skip pending", wSkipped},
		{"expert_use: pending/0 -> completed/1（点亮）", wOK},
		{"viewed 激活 pending->in_progress", 0},
		{"lottery balance=0，无需抽奖", 0},
	}
	for _, c := range cases {
		if got := classifyScriptLine(c.line); got != c.want {
			t.Errorf("classify(%q)=%d 期望 %d", c.line, got, c.want)
		}
	}
}
