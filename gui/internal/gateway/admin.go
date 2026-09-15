// 网关管理端（/admin/*）客户端：多业务 Key CRUD、账号禁用/启用。
// 鉴权与普通请求一致（Bearer 管理密钥 = 网关 config api_key）。
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// AdminAssociation Key↔账号关联（镜像网关 keys.Association）。
type AdminAssociation struct {
	UID      string `json:"uid"`
	Priority int    `json:"priority"` // 越大越优先（分层调度）
	Enabled  bool   `json:"enabled"`
}

// AdminKey 网关业务 Key 条目（镜像网关 keys.Key；含完整 value 供深链/复制）。
type AdminKey struct {
	ID           string             `json:"id"`
	Name         string             `json:"name"`
	Value        string             `json:"value"`
	Enabled      bool               `json:"enabled"`
	Associations []AdminAssociation `json:"associations"`
	CreatedAt    time.Time          `json:"created_at"`
	// Wildcard 通配标记（网关侧只对 legacy 迁移的 default Key 置位）：
	// 未关联账号也可用全池——UI 需与"未关联=不可用"区分显示。
	Wildcard bool `json:"wildcard,omitempty"`
}

// AdminKeyPatch Key 更新载荷（nil 字段 = 不修改；Associations 为全量替换）。
type AdminKeyPatch struct {
	Name         *string             `json:"name,omitempty"`
	Enabled      *bool               `json:"enabled,omitempty"`
	Associations *[]AdminAssociation `json:"associations,omitempty"`
}

// adminDecode 统一处理 /admin/* 响应：非 200 翻译错误，200 解析 JSON 到 out。
func (c *Client) adminDecode(ctx context.Context, method, path string, body []byte, out any) error {
	resp, err := c.do(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s", explainGatewayError(resp.StatusCode, raw))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// AdminKeys 拉取全部业务 Key。
func (c *Client) AdminKeys(ctx context.Context) ([]AdminKey, error) {
	var out struct {
		Keys []AdminKey `json:"keys"`
	}
	if err := c.adminDecode(ctx, http.MethodGet, "/admin/keys", nil, &out); err != nil {
		return nil, err
	}
	return out.Keys, nil
}

// AdminCreateKey 新建 Key（网关自动生成 value）。
func (c *Client) AdminCreateKey(ctx context.Context, name string) (*AdminKey, error) {
	body, _ := json.Marshal(map[string]string{"name": name})
	var k AdminKey
	if err := c.adminDecode(ctx, http.MethodPost, "/admin/keys", body, &k); err != nil {
		return nil, err
	}
	return &k, nil
}

// AdminUpdateKey 更新 Key（部分字段/全量关联替换）。
func (c *Client) AdminUpdateKey(ctx context.Context, id string, patch AdminKeyPatch) (*AdminKey, error) {
	body, err := json.Marshal(patch)
	if err != nil {
		return nil, err
	}
	var k AdminKey
	if err := c.adminDecode(ctx, http.MethodPut, "/admin/keys/"+id, body, &k); err != nil {
		return nil, err
	}
	return &k, nil
}

// AdminDeleteKey 删除 Key。
func (c *Client) AdminDeleteKey(ctx context.Context, id string) error {
	return c.adminDecode(ctx, http.MethodDelete, "/admin/keys/"+id, nil, nil)
}

// AdminRegenerateKey 重置 Key value（旧值立即失效）。
func (c *Client) AdminRegenerateKey(ctx context.Context, id string) (*AdminKey, error) {
	var k AdminKey
	if err := c.adminDecode(ctx, http.MethodPost, "/admin/keys/"+id+"/regenerate", []byte("{}"), &k); err != nil {
		return nil, err
	}
	return &k, nil
}

// AdminAccounts 拉取账号池状态（含禁用/冷却，网关 /admin/accounts = /status 同源）。
func (c *Client) AdminAccounts(ctx context.Context) (*Status, error) {
	var out struct {
		Accounts []AccountStatus `json:"accounts"`
	}
	if err := c.adminDecode(ctx, http.MethodGet, "/admin/accounts", nil, &out); err != nil {
		return nil, err
	}
	return &Status{Accounts: out.Accounts, Total: len(out.Accounts)}, nil
}

// AdminAccountToggle 禁用/启用账号（网关侧持久化，重启保持）。
func (c *Client) AdminAccountToggle(ctx context.Context, uid string, disable bool, reason string) error {
	path := "/admin/accounts/" + uid + "/enable"
	if disable {
		path = "/admin/accounts/" + uid + "/disable"
	}
	body, _ := json.Marshal(map[string]string{"reason": reason})
	return c.adminDecode(ctx, http.MethodPost, path, body, nil)
}

// AdminNotifyTest 让网关发送一封通知测试邮件（验证 SMTP 配置是否可用）。
func (c *Client) AdminNotifyTest(ctx context.Context) error {
	return c.adminDecode(ctx, http.MethodPost, "/admin/notify/test", []byte("{}"), nil)
}
