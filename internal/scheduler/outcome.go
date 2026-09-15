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

// Outcome 一次任务执行中单个账号（或脚本类任务的整体）的结果。
// 账号类任务 UID 非空；脚本类任务 UID 为空、Message 携带输出尾部与退出码语义。
type Outcome struct {
	UID      string `json:"uid,omitempty"`
	Nickname string `json:"nickname,omitempty"`
	OK       bool   `json:"ok"`
	Status   string `json:"status"` // ok | already | fail | skipped
	Message  string `json:"message,omitempty"`
	Reward   int64  `json:"reward,omitempty"`  // 领奖等一次性收益
	Credits  *int64 `json:"credits,omitempty"` // 签到后余额（查询成功才有值）

	// Output / ExitCode 仅脚本类任务（school/cat）填充：合并的 stdout+stderr 尾部
	// 与子进程退出码，供面板展示失败原因（此前输出被丢弃、失败只剩 exit status）。
	Output   string `json:"output,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
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
	}
}

// Spec 返回当前排程规格（Runner 接口用；Scheduler 的 cfg 在生命周期内不变）。
func (s *Scheduler) Spec() ScheduleSpec { return s.cfg.Spec() }

// NextWake 纯函数：返回 now 之后最近的唤醒时刻，以及该时刻需要执行的全部任务类型。
//
// 多类任务配到同一小时（如签到与旅行都含 9）时该时刻一并执行；显式禁用的任务
// 不进候选（nextFire 对其零值返回零时间，这里再跳过零时点）。
// 六类全部禁用 → 返回零时间与 nil（调用方据此不空转）。
//
// 这是排程语义的唯一来源：内部 nextWake 只是把 Kind 映射回 taskKind。
func NextWake(now time.Time, spec ScheduleSpec) (time.Time, []Kind) {
	type slot struct {
		at   time.Time
		kind Kind
	}
	var slots []slot
	if !spec.CheckinDisabled {
		slots = append(slots, slot{nextFire(now, spec.CheckinHours), KindCheckin})
	}
	if !spec.TravelDisabled {
		slots = append(slots, slot{nextFire(now, spec.TravelHours), KindTravel})
	}
	if !spec.ActivityDisabled {
		slots = append(slots, slot{nextFire(now, spec.ActivityHours), KindActivity})
	}
	if !spec.KeepaliveDisabled {
		slots = append(slots, slot{nextFire(now, spec.KeepaliveHours), KindKeepalive})
	}
	if !spec.SchoolDisabled {
		slots = append(slots, slot{nextFire(now, spec.SchoolHours), KindSchool})
	}
	if !spec.CatDisabled {
		slots = append(slots, slot{nextFire(now, spec.CatHours), KindCat})
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
