// 自动化（/admin/automation/*）客户端：状态总览、运行历史、手动触发、排程热更新。
//
// 触发端点返回 202（异步执行），与 adminDecode 的"仅 200"约定不同，故本文件自带
// 解码；409（同类任务在跑）映射为 ErrAutomationBusy 供 api 层回 409。
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ErrAutomationBusy 同类任务已在运行（网关 409）。
var ErrAutomationBusy = errors.New("同类任务正在执行中，请稍候")

// AutomationItem 单条执行结果（镜像网关 automation.Item）。
type AutomationItem struct {
	UID      string `json:"uid,omitempty"`
	Nickname string `json:"nickname,omitempty"`
	OK       bool   `json:"ok"`
	Status   string `json:"status"`
	Message  string `json:"message,omitempty"`
	Reward   int64  `json:"reward,omitempty"`
	Credits  *int64 `json:"credits,omitempty"`
}

// AutomationRun 一次执行记录（镜像网关 automation.Run）。
type AutomationRun struct {
	ID         string           `json:"id"`
	Kind       string           `json:"kind"`
	Trigger    string           `json:"trigger"`
	StartedAt  time.Time        `json:"started_at"`
	FinishedAt time.Time        `json:"finished_at"`
	OK         int              `json:"ok"`
	Failed     int              `json:"failed"`
	Skipped    int              `json:"skipped"`
	Error      string           `json:"error,omitempty"`
	Output     string           `json:"output,omitempty"`
	ExitCode   *int             `json:"exit_code,omitempty"`
	Items      []AutomationItem `json:"items,omitempty"`
	Running    bool             `json:"running"`
	Truncated  bool             `json:"truncated,omitempty"`
}

// AutomationKindStatus 单任务类型状态。
type AutomationKindStatus struct {
	Kind         string         `json:"kind"`
	Enabled      bool           `json:"enabled"`
	Hours        []int          `json:"hours"`
	NextFire     *time.Time     `json:"next_fire,omitempty"`
	Running      bool           `json:"running"`
	CurrentRunID string         `json:"current_run_id,omitempty"`
	LastRun      *AutomationRun `json:"last_run,omitempty"`
}

// AutomationStatus 自动化总览。
type AutomationStatus struct {
	Enabled bool                   `json:"enabled"`
	Now     time.Time              `json:"now"`
	Kinds   []AutomationKindStatus `json:"kinds"`
}

// automationDecode 解码 /admin/automation/* 响应；want 为可接受的状态码（缺省 200）。
func (c *Client) automationDecode(ctx context.Context, method, path string, body []byte, out any, want ...int) error {
	resp, err := c.do(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	// 缺省接受 200：此前 want 为空时循环零次、ok 恒 false，把正常的 200 响应
	// 当成错误抛出（"网关返回 HTTP 200：{...}"）——自动化页一打开就报错。
	acceptable := want
	if len(acceptable) == 0 {
		acceptable = []int{http.StatusOK}
	}
	ok := false
	for _, w := range acceptable {
		if resp.StatusCode == w {
			ok = true
			break
		}
	}
	if !ok {
		switch resp.StatusCode {
		case http.StatusConflict:
			return ErrAutomationBusy
		case http.StatusNotFound:
			return fmt.Errorf("记录不存在")
		}
		return fmt.Errorf("%s", explainGatewayError(resp.StatusCode, raw))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// AutomationStatus 拉取自动化总览。
func (c *Client) AutomationStatus(ctx context.Context) (*AutomationStatus, error) {
	var st AutomationStatus
	if err := c.automationDecode(ctx, http.MethodGet, "/admin/automation/status", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// AutomationRuns 拉取运行历史（新 → 旧，不含明细）。
func (c *Client) AutomationRuns(ctx context.Context, limit int) ([]AutomationRun, error) {
	if limit <= 0 {
		limit = 20
	}
	var out struct {
		Runs []AutomationRun `json:"runs"`
	}
	if err := c.automationDecode(ctx, http.MethodGet, fmt.Sprintf("/admin/automation/runs?limit=%d", limit), nil, &out); err != nil {
		return nil, err
	}
	return out.Runs, nil
}

// AutomationRun 拉取单条完整记录（含逐账号明细与脚本输出）。
func (c *Client) AutomationRun(ctx context.Context, id string) (*AutomationRun, error) {
	var run AutomationRun
	if err := c.automationDecode(ctx, http.MethodGet, "/admin/automation/runs/"+id, nil, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// AutomationTrigger 手动触发（网关 202 异步执行，返回运行中记录）。
func (c *Client) AutomationTrigger(ctx context.Context, kind string) (*AutomationRun, error) {
	var out struct {
		Run AutomationRun `json:"run"`
	}
	if err := c.automationDecode(ctx, http.MethodPost, "/admin/automation/run/"+kind, []byte("{}"), &out,
		http.StatusAccepted, http.StatusOK); err != nil {
		return nil, err
	}
	return &out.Run, nil
}

// AutomationApply 排程热生效（仅内存；文件由面板 SaveSchedule 写入）。
func (c *Client) AutomationApply(ctx context.Context, schedule map[string]any) (*AutomationStatus, error) {
	body, err := json.Marshal(map[string]any{"schedule": schedule})
	if err != nil {
		return nil, err
	}
	var out struct {
		Status AutomationStatus `json:"status"`
	}
	if err := c.automationDecode(ctx, http.MethodPost, "/admin/automation/apply", body, &out); err != nil {
		return nil, err
	}
	return &out.Status, nil
}
