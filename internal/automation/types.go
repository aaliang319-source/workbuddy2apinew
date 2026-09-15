// Package automation 自动化功能层：把定时任务（签到/猫猫旅行/活跃上报/token 保活/
// 开学季/夜猫子）从"只会打日志"提升为可观测、可手动触发、可热改排程的独立模块。
//
// 分层：本包只负责「何时跑 / 跑完记什么 / 对外怎么报告」；具体怎么跑由
// internal/scheduler（Runner）实现。定时循环、运行历史、状态查询、热更新排程都收敛在这里，
// 网关 HTTP 层（internal/server/admin_automation.go）只做协议转换。
package automation

import "time"

// Trigger 运行触发来源。
const (
	TriggerSchedule = "schedule" // 定时循环
	TriggerManual   = "manual"   // 面板/接口手动触发
)

// Item 单条执行结果（账号类任务一账号一条；脚本类任务一条，Output 带输出尾部）。
type Item struct {
	UID      string `json:"uid,omitempty"`
	Nickname string `json:"nickname,omitempty"`
	OK       bool   `json:"ok"`
	Status   string `json:"status"` // ok | already | fail | skipped
	Message  string `json:"message,omitempty"`
	Reward   int64  `json:"reward,omitempty"`
	Credits  *int64 `json:"credits,omitempty"`
}

// Run 一次任务执行的完整记录（落盘 + API 详情）。
type Run struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Trigger    string    `json:"trigger"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	OK         int       `json:"ok"`
	Failed     int       `json:"failed"`
	Skipped    int       `json:"skipped"`

	// Error 非空表示整批失败（撞车/执行器异常），账号级失败看 Items/Failed。
	Error string `json:"error,omitempty"`
	// Output / ExitCode 仅脚本类任务（school/cat）：子进程合并输出尾部与退出码。
	Output   string `json:"output,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`

	Items []Item `json:"items,omitempty"`
	// Running 运行中标记（历史里恒 false；status 与手动触发返回的即时记录用）。
	Running bool `json:"running"`
	// Truncated 条目超上限被裁剪（面板据此提示"仅显示部分账号"）。
	Truncated bool `json:"truncated,omitempty"`
}

// summary 列表视图：去掉逐账号明细与脚本输出，避免列表响应过大。
func (r Run) summary() Run {
	r.Items = nil
	r.Output = ""
	return r
}

// KindStatus 单个任务类型的实时状态。
type KindStatus struct {
	Kind         string     `json:"kind"`
	Enabled      bool       `json:"enabled"`
	Hours        []int      `json:"hours"`
	NextFire     *time.Time `json:"next_fire,omitempty"`
	Running      bool       `json:"running"`
	CurrentRunID string     `json:"current_run_id,omitempty"`
	LastRun      *Run       `json:"last_run,omitempty"`
}

// Status 自动化总览（GET /admin/automation/status）。
type Status struct {
	Enabled bool         `json:"enabled"`
	Now     time.Time    `json:"now"`
	Kinds   []KindStatus `json:"kinds"`
}
