// outcome.go 调度结果与排程规格的对外契约：automation 模块与 admin API 只依赖
// 本文件导出的类型，不感知内部 taskKind。
//
// 设计要点（保住既有战斗测试）：既有 `Run*Now()` 由 `()` 改为 `() []Outcome`，
// Go 允许表达式语句丢弃返回值，因此既有测试调用点（`s.RunCheckinNow()` 等）
// 零改动即可编译，行为与日志逐字不变。
package scheduler

import (
	"context"
	"time"
)

// 逐账号结果状态取值（Outcome.Status 与 automation.Item.Status 共用同一口径）。
const (
	StatusOK      = "ok"      // 执行成功
	StatusAlready = "already" // 上游判定已完成/已领（幂等重复，视为正常）
	StatusFail    = "fail"    // 失败
	StatusSkipped = "skipped" // 跳过（门控/无有效凭证/非进行期等，Message 带原因）
	// StatusUnreached 脚本未返回任何结果的账号：声明要处理却没出现在输出里
	// （输出被上限截断、或执行中途中断）。只在脚本类任务的解析结果里出现。
	StatusUnreached = "unreached"
)

// Outcome 一次任务执行中单个账号（或脚本类任务的整体）的结果。
// 账号类任务 UID 非空；脚本类任务 UID 为空、Message 携带输出尾部与退出码语义。
type Outcome struct {
	UID      string `json:"uid,omitempty"`
	Nickname string `json:"nickname,omitempty"`
	OK       bool   `json:"ok"`
	Status   string `json:"status"` // ok | already | fail | skipped | unreached
	Message  string `json:"message,omitempty"`
	Reward   int64  `json:"reward,omitempty"`  // 领奖等一次性收益
	Credits  *int64 `json:"credits,omitempty"` // 签到后余额（查询成功才有值）

	// Output / ExitCode 仅脚本类任务（school/cat）填充：合并的 stdout+stderr 尾部
	// 与子进程退出码，供面板展示失败原因（此前输出被丢弃、失败只剩 exit status）。
	Output   string `json:"output,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`

	// Items 仅脚本类任务填充：从子进程输出解析出的逐账号结果（见 script_parse.go）。
	// 非空表示这条 Outcome 是"命令级元信息"（UID 为空、携带 Output/ExitCode），
	// 真实结论在 Items 里；为空表示没有可解析的逐账号线索，调用方按整批一条处理。
	Items []Outcome `json:"items,omitempty"`
	// ParseWarning 逐账号明细与脚本自己的汇总行对不上时的提示（可能被输出上限截断）。
	ParseWarning string `json:"parse_warning,omitempty"`
}


// Kind 任务类型的稳定字符串标识（配置键、admin API 路径、历史记录共用）。
type Kind string

const (
	KindCheckin   Kind = "checkin"
	KindTravel    Kind = "travel"
	KindActivity  Kind = "activity"
	KindKeepalive Kind = "keepalive"
	KindSchool    Kind = "school"
	KindCat       Kind = "cat"
)

// AllKinds 全部任务类型（顺序即面板展示顺序）。
var AllKinds = []Kind{KindCheckin, KindTravel, KindActivity, KindKeepalive, KindSchool, KindCat}

// ValidKind 判定字符串是否为合法任务类型。
func ValidKind(s string) bool {
	for _, k := range AllKinds {
		if string(k) == s {
			return true
		}
	}
	return false
}

// ScheduleSpec 排程决策所需的全部输入（纯数据，可由 automation 独立持有并热替换）。
// 语义与 Config 的六个 *_Hours / *_Disabled 一一对应。
type ScheduleSpec struct {
	CheckinHours   []int
	TravelHours    []int
	ActivityHours  []int
	KeepaliveHours []int
	SchoolHours    []int
	CatHours       []int

	CheckinDisabled   bool
	TravelDisabled    bool
	ActivityDisabled  bool
	KeepaliveDisabled bool
	SchoolDisabled    bool
	CatDisabled       bool

	// JitterSpanMinutes 每个小时窗口内的抖动跨度（分钟）；<=0 走默认。
	// JitterDisabled 明确要求准点执行（跨度归 0），用于需要精确时刻的排障场景。
	JitterSpanMinutes int
	JitterDisabled    bool
}

// Spec 从 Config 提取排程规格（Hours 已由 New 回落默认）。
func (c Config) Spec() ScheduleSpec {
	return ScheduleSpec{
		CheckinHours:      c.CheckinHours,
		TravelHours:       c.TravelHours,
		ActivityHours:     c.ActivityHours,
		KeepaliveHours:    c.KeepaliveHours,
		SchoolHours:       c.SchoolHours,
		CatHours:          c.CatHours,
		CheckinDisabled:   c.CheckinDisabled,
		TravelDisabled:    c.TravelDisabled,
		ActivityDisabled:  c.ActivityDisabled,
		KeepaliveDisabled: c.KeepaliveDisabled,
		SchoolDisabled:    c.SchoolDisabled,
		CatDisabled:       c.CatDisabled,
		JitterSpanMinutes: c.JitterSpanMinutes,
		JitterDisabled:    c.JitterDisabled,
	}
}

// JitterSpan 返回本规格的抖动跨度（分钟）：JitterDisabled 时为 0（准点），
// 否则归一为非正 → 默认、超限 → 上限（与 NewRandomizer/SetSpanMinutes 同口径）。
func (s ScheduleSpec) JitterSpan() int {
	if s.JitterDisabled {
		return 0
	}
	if s.JitterSpanMinutes <= 0 {
		return jitterSpanDefault
	}
	if s.JitterSpanMinutes > jitterSpanMax {
		return jitterSpanMax
	}
	return s.JitterSpanMinutes
}

// Spec 返回当前排程规格（Runner 接口用；Scheduler 的 cfg 在生命周期内不变）。
func (s *Scheduler) Spec() ScheduleSpec { return s.cfg.Spec() }

// NextWake 返回 now 之后最近的唤醒时刻与该时刻需执行的全部任务类型。
//
// **每次调用都重新掷随机数**（抖动是每次排定现滚的，不是按日期哈希出来的固定偏移）——
// 因此它不再是纯函数，且**只能由定时循环在排定时调用**。面板要把「下次执行」显示给
// 用户，必须读定时循环记下的落点（Randomizer.Scheduled），否则会掷出另一个随机值，
// 显示的时刻与真实执行不符。
//
// 各任务族各自掷各小时的落点，因此"配在同一小时"的不同任务族通常不再同刻触发——
// 错开正是目的（同刻并发是最易识别的脚本特征）；偶发相撞时仍一并执行。
// 显式禁用的任务不进候选；六类全禁用 → 返回零时间与 nil（调用方据此不空转）。
//
// 滚动由调用方传入的 Randomizer 承担；r 为 nil 时按"准点"处理（无抖动），
// 便于只关心"哪个小时"的调用方与老测试沿用。
func NextWake(now time.Time, spec ScheduleSpec, r *Randomizer) (time.Time, []Kind) {
	if r == nil {
		// 无抖动源：按"准点、只关心哪个小时"处理（零值 Randomizer 跨度即 0）。
		r = &Randomizer{}
	} else {
		r.setSpanFromSpec(spec)
	}

	type slot struct {
		at   time.Time
		kind Kind
	}
	var slots []slot
	if !spec.CheckinDisabled {
		slots = append(slots, slot{r.nextFire(now, spec.CheckinHours), KindCheckin})
	}
	if !spec.TravelDisabled {
		slots = append(slots, slot{r.nextFire(now, spec.TravelHours), KindTravel})
	}
	if !spec.ActivityDisabled {
		slots = append(slots, slot{r.nextFire(now, spec.ActivityHours), KindActivity})
	}
	if !spec.KeepaliveDisabled {
		slots = append(slots, slot{r.nextFire(now, spec.KeepaliveHours), KindKeepalive})
	}
	if !spec.SchoolDisabled {
		slots = append(slots, slot{r.nextFire(now, spec.SchoolHours), KindSchool})
	}
	if !spec.CatDisabled {
		slots = append(slots, slot{r.nextFire(now, spec.CatHours), KindCat})
	}
	var earliest time.Time
	for _, sl := range slots {
		if sl.at.IsZero() {
			continue
		}
		if earliest.IsZero() || sl.at.Before(earliest) {
			earliest = sl.at
		}
	}
	if earliest.IsZero() {
		return time.Time{}, nil
	}
	// 每个任务族各自的落点都要记下（不只最早的那个）：面板按任务类型展示
	// 「下次执行」，读的就是这里——自己重算会掷出另一个随机值。
	for _, sl := range slots {
		if !sl.at.IsZero() {
			r.remember(string(sl.kind), sl.at)
		}
	}
	var kinds []Kind
	for _, sl := range slots {
		if !sl.at.IsZero() && sl.at.Equal(earliest) {
			kinds = append(kinds, sl.kind)
		}
	}
	return earliest, kinds
}

// kindToTask / taskToKind 内部 taskKind 与导出 Kind 的双向映射。
func kindToTask(k Kind) (taskKind, bool) {
	switch k {
	case KindCheckin:
		return taskCheckin, true
	case KindTravel:
		return taskTravel, true
	case KindActivity:
		return taskActivity, true
	case KindKeepalive:
		return taskKeepalive, true
	case KindSchool:
		return taskSchool, true
	case KindCat:
		return taskCat, true
	}
	return 0, false
}

func taskToKind(k taskKind) Kind {
	switch k {
	case taskCheckin:
		return KindCheckin
	case taskTravel:
		return KindTravel
	case taskActivity:
		return KindActivity
	case taskKeepalive:
		return KindKeepalive
	case taskSchool:
		return KindSchool
	case taskCat:
		return KindCat
	}
	return ""
}

// outcomeFromCheckin 把签到结果映射为统一 Outcome（status/reward 语义对齐）。
func outcomeFromCheckin(oc CheckinOutcome) Outcome {
	ok := oc.Status == CheckinOK || oc.Status == CheckinAlready
	return Outcome{
		UID:      oc.UID,
		Nickname: oc.Nickname,
		OK:       ok,
		Status:   string(oc.Status),
		Message:  oc.Detail,
		Credits:  oc.Credits,
	}
}

// runKindCtx 按导出 Kind 分派到带 ctx 的执行函数（automation 手动触发与
// 定时循环共用；与内部 dispatch 的差别仅是入口类型）。
func (s *Scheduler) runKindCtx(ctx context.Context, k Kind) []Outcome {
	switch k {
	case KindCheckin:
		return s.RunCheckinCtx(ctx)
	case KindTravel:
		return s.runTravel(ctx)
	case KindActivity:
		return s.runActivity(ctx)
	case KindKeepalive:
		return s.RunKeepaliveCtx(ctx)
	case KindSchool:
		return s.RunSchoolCtx(ctx)
	case KindCat:
		return s.RunCatCtx(ctx)
	}
	return nil
}

// RunKind 按类型立即执行一次任务（automation 手动触发入口）。
// 未知类型返回 nil（调用方已做 ValidKind 校验）。
func (s *Scheduler) RunKind(ctx context.Context, k Kind) []Outcome {
	return s.runKindCtx(ctx, k)
}
