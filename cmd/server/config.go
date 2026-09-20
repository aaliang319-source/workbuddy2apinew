// config.go 加载 JSON 配置 + 环境变量覆盖。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/config"
	"workbuddy2api/internal/notify"
	"workbuddy2api/internal/prompt"
)

// Config 顶层配置。
type Config struct {
	Listen    string `json:"listen"`     // ":7863"
	APIKey    string `json:"api_key"`    // 空 = 不鉴权；兼作 /admin/* 管理密钥
	AuthDir   string `json:"auth_dir"`   // ./auths
	StateFile string `json:"state_file"` // ./data/state.json
	// KeysFile 多业务 Key 存储路径（keys.Store 热生效）。首次启动文件不存在时
	// 把 api_key 迁移为 default 业务 Key；此后该文件为业务 Key 唯一权威源。
	KeysFile string `json:"keys_file"` // ./data/keys.json

	// MetricsFile 请求统计落盘路径（server.metrics_enabled=true 时生效）。
	// 与 state_file 同目录风格，默认 ./data/metrics.json。
	MetricsFile string `json:"metrics_file"`

	Server struct {
		// MaxBodyMB 聊天请求体大小上限（单位 MB，默认 8）。
		// 请求体超过该值直接返回 413 request_body_too_large，不再静默截断后喂给上游
		// （issue #41：截断的 JSON 让上游 unmarshal 报 unexpected EOF，网关却罚号）。
		// 0/负数视为非法 → normalize 回落默认并记录。
		MaxBodyMB int `json:"max_body_mb"`
		// MetricsEnabled 请求统计开关（/v1/stats）。缺省 false：不记录观测，
		// /v1/stats 返回 {"enabled":false,...}（面板据此显示未启用提示）。
		// 控制面板提示的正是本键 server.metrics_enabled。
		MetricsEnabled bool `json:"metrics_enabled"`
		// AuthResyncSeconds auths 目录运行期自动发现的扫描间隔（秒）。缺省 30：
		// 面板/脚本落盘新账号后无需重启网关。显式 0 = 关闭；负数回落默认。
		AuthResyncSeconds int `json:"auth_resync_seconds"`
	} `json:"server"`

	Cooldown struct {
		// hard_credit / err_threshold / err_cooldown 三个历史键已退役：
		// 硬冷却固定为次日 04:00（CooldownUntilTomorrow4AM），连续错误语义并入熔断器。
		// 旧 config 中的这些键因 JSON 未知字段而自然忽略，不报错。
		SoftRate string `json:"soft_rate"` // "600s"，软限流冷却基数
		// SoftRateMax 软冷却指数退避的封顶，默认 "2h"。
		// 空值回落默认，非法值报错（处理风格同 soft_rate）。
		SoftRateMax string `json:"soft_rate_max"` // "2h"
	} `json:"cooldown"`

	Schedule config.Schedule `json:"schedule"`

	Global struct {
		// Enabled global realm 路由开关。缺省 true：Realm() 正常把 realm=global/
		// domain=workbuddy.ai 的账号判为 global 并路由 global base/路径。
		// 显式 "enabled": false 关闭（逃生门，纯 CN 锁定：即便 auth 写了 realm=global
		// 也不路由，auth.Realm() 双保险的第一道闸）。纯 CN 部署行为不变：CN 账号
		// 恒判 cn，global base 只在 realm=global 的账号上被使用。
		Enabled bool `json:"enabled"`
		// MixRealms 国际版/国内账号混合调度（缺省 false = 现状分池隔离）。
		// true：选号不再按 realm 过滤，CN 与 global 账号在同一个关联/优先级体系内
		// 竞争（配合 key 关联优先级即真正生效）；出站仍按**命中账号的 realm** 选
		// base（CN 账号走 CN base、global 账号走 global base），模型名统一发裸名。
		// 模型列表同步"统一化"：不再输出 global: 前缀，两域同名模型去重为一个名字。
		MixRealms bool `json:"mix_realms"`
		// ChatBase / BillingBase 国际版上游 base 覆盖；空 = 回落内置默认
		// https://www.workbuddy.ai（D5，internal/upstream.defaultGlobalBase）。
		ChatBase    string `json:"chat_base"`
		BillingBase string `json:"billing_base"`
	} `json:"global"`

	Upstream struct {
		// TimeoutSeconds 短 RPC（refresh/checkin/balance/FetchModels）总时长上限，默认 120。
		TimeoutSeconds int `json:"timeout_seconds"`
		// HeaderTimeoutSeconds 聊天 SSE 首字节前（响应头）上限；<=0 回落 TimeoutSeconds。
		HeaderTimeoutSeconds int `json:"header_timeout_seconds"`
		// IdleTimeoutSeconds 聊天 SSE 流中空闲上限（活跃吐数据续命不掐）；<=0 回落默认 300。
		IdleTimeoutSeconds int `json:"idle_timeout_seconds"`
		// UserAgent 出站 User-Agent 显式覆盖（非空时全路径生效，优先于默认三段式）。
		// 全部出站请求生效：chat/refresh/checkin/balance/report/travel/FetchModels。
		// issue #42 深挖：官网「使用端」列基于出站请求 UA 的服务端归因，官方 WorkBuddy
		// 桌面 UA 为 `WorkBuddy/<version>`。默认值已对齐官方（A 段变更），用户仍可配完全
		// 自定义值改写。
		UserAgent string `json:"user_agent"`
		// ClientVersion WorkBuddy 客户端版本段（出站 UA 的 `WorkBuddy/<ver>` 与白名单
		// 头组 X-IDE-Version 的取值）。空 = 内置默认（对齐官方 5.5.4 分发包）；
		// 显式配置（如升级后的桌面包版本）则随配置走。
		ClientVersion string `json:"client_version"`
		// CliVersion 出站 UA 中 `CLI/<ver>` 段的版本。空 = 内置默认（对齐官方内置 CLI
		// 2.137.1）；显式配置则随配置走。
		CliVersion string `json:"cli_version"`

		// DeviceToken 设备风控 Token（X-Device-Token 头）全局兜底。
		// 容器内无桌面端 Turing SDK，这是把外部生成的 token 注入的入口；空 = 不注入。
		// 每号覆盖优先级：auths 文件 device_token > 本全局值 > DeviceTokenFile（文件兜底）。
		DeviceToken string `json:"device_token"`
		// DeviceTokenFile 宿主落盘的 device token 文件路径（可选，空 = 不读文件）。
		// 读取频率限 5 分钟一次缓存，>1KB 或读失败则忽略（优雅降级不注入）。
		DeviceTokenFile string `json:"device_token_file"`
		// ClientName 用量归属头 X-Product/X-IDE-Name/X-IDE-Type 的取值。
		// 空（缺省）= "WorkBuddy"：伪造官方桌面端指纹（X-IDE-* 四头 + X-Agent-Purpose，
		// 上游用量归因不再出现 client/agentPurpose 为空的网关特征）。
		// 显式配 "SaaS" 还原旧行为（仅 X-Product="SaaS"，不设 X-IDE-*）。
		ClientName string `json:"client_name"`
		// PassthroughIP 是否透传客户端 IP（X-Forwarded-For/X-Real-IP 首段）给上游。
		// 缺省 false（反代安全边界：不把内网/代理 IP 暴露给上游）；true 才透传。
		PassthroughIP bool `json:"passthrough_ip"`
	} `json:"upstream"`

	Features struct {
		// SanitizeBlacklistFingerprints 出站请求体黑名单指纹脱敏（默认 true；false 完全还原）。
		SanitizeBlacklistFingerprints bool `json:"sanitize_blacklist_fingerprints"`
	} `json:"features"`

	Prompt struct {
		// Mode passthrough（默认）= 透传客户端原始 system（降级重试仍会切到 Degraded）；
		// custom = 网关用自有系统提示词替换客户端 system/developer（显式配置仍可覆盖回替换）。
		Mode string `json:"mode"` // "custom" / "passthrough"
		// File 提示词文件路径；空 = 内置默认 defaultprompt.md；
		// 路径非空但不可读 → 启动报错（fail fast，避免静默回落到内置默认）。
		File string `json:"file"`
	} `json:"prompt"`

	// PromptText 解析后的系统提示词文本（custom 模式使用）。
	PromptText string `json:"-"`

	Upstash struct {
		URL   string `json:"url"`   // 空 = 纯内存模式；支持完整 rediss:// URL 或 https://xxx.upstash.io host
		Token string `json:"token"` // url 非完整连接串时用于组装 rediss://default:<token>@<host>:6379
	} `json:"upstash"`

	Pool struct {
		MaxInFlight        int     `json:"max_in_flight"`        // 单账号最大在途请求数，0 = 不限
		BreakerThreshold   int     `json:"breaker_threshold"`    // 连续失败次数触发熔断，默认 3
		BreakerCooldown    string  `json:"breaker_cooldown"`     // 基础熔断时长，默认 "30m"
		BreakerCooldownMax string  `json:"breaker_cooldown_max"` // 指数退避封顶，默认 "6h"
		IdleWeightPerHour  float64 `json:"idle_weight_per_hour"` // 闲置补偿：每小时未用 +0.5 权重
		IdleWeightMax      float64 `json:"idle_weight_max"`      // 闲置补偿封顶，默认 5.0
		// ExpiringSoon 快过期积分窗口（如 "168h"=7天）：签到查余额时，到期时间在此窗口内
		// 的积分被标记为"快过期"，选号优先消耗（issue:积分过期）。空/0 = 禁用分桶。
		ExpiringSoon string `json:"expiring_soon"`
	} `json:"pool"`

	Automation struct {
		// Enabled 是否由 automation 模块驱动定时循环。false = 回退 scheduler 原循环
		// （kill switch：行为与引入前逐字一致）。手动触发/历史/状态 API 不受影响。
		Enabled bool `json:"enabled"`
		// HistoryFile 运行历史落盘路径（默认 ./data/automation.json）。
		HistoryFile string `json:"history_file"`
		// ScriptTimeout 脚本类任务（school/cat）子进程超时，默认 "10m"。
		ScriptTimeout string `json:"script_timeout"`
		// HistoryRuns 历史保留条数（ring），默认 100。
		HistoryRuns int `json:"history_runs"`
	} `json:"automation"`

	SessionSticky struct {
		Enabled    bool   `json:"enabled"`     // 默认 true
		TTL        string `json:"ttl"`         // 会话绑定 TTL，默认 "30m"
		GCInterval string `json:"gc_interval"` // 会话 GC 周期，默认 "5m"
	} `json:"session_sticky"`

	Anthropic struct {
		// DefaultModel /v1/messages 上未知模型名（claude-sonnet-4-5 等）的缺省路由。
		// 缺省 "cn:auto"（国内版自动档）；带 cn:/global: 前缀的请求名永远直通不经过本值。
		DefaultModel string `json:"default_model"`
		// ModelMap 精确映射：客户端模型名 → 网关模型名（含 cn:/global: 前缀）。
		// 例：{"claude-sonnet-4-5": "cn:glm-5.3"}。未命中的名字走 DefaultModel。
		ModelMap map[string]string `json:"model_map"`
	} `json:"anthropic"`

	// Notify 账号事件邮件提醒（额度即将耗尽 / 额度已耗尽 / 账号路由切换）。
	// 配置为启动期加载：修改 SMTP 后需重启网关。
	Notify struct {
		Enabled bool `json:"enabled"`
		// SMTP 连接
		SMTPHost     string   `json:"smtp_host"`
		SMTPPort     int      `json:"smtp_port"` // 缺省 587
		SMTPUsername string   `json:"smtp_username"`
		SMTPPassword string   `json:"smtp_password"`
		SMTPFrom     string   `json:"smtp_from"`
		SMTPTo       []string `json:"smtp_to"`
		// SMTPTLS "starttls"（587，默认）/"tls"（465 隐式 TLS）/"none"（明文，仅内网中继）
		SMTPTLS string `json:"smtp_tls"`
		// CreditsThreshold 可用额度低于该值即告警（事件 A）
		CreditsThreshold int64 `json:"credits_threshold"`
		// ExpiringDays 存在快过期额度即告警（窗口语义与 pool.expiring_soon 对应）
		ExpiringDays int `json:"expiring_days"`
		// ThrottleHours 同一 (事件, 账号) 最小通知间隔（小时）
		ThrottleHours int `json:"throttle_hours"`
		// QueueSize 待发队列容量（满则丢弃）
		QueueSize int `json:"queue_size"`
		// ScanHours 额度扫描时刻（本地时区整点；空 = 不启用扫描任务，仅实时事件）
		ScanHours []int `json:"scan_hours"`
		// Events 事件开关（均缺省开启）
		Events struct {
			CreditsLow    *bool `json:"credits_low"`
			Exhausted     *bool `json:"exhausted"`
			AccountSwitch *bool `json:"account_switch"`
		} `json:"events"`
	} `json:"notify"`

	// ModelFallback 模型回退白名单（切模型故障转移）：请求的模型被域内所有账号
	// 拒绝（11102 无此模型 / 403）时，按序改用白名单里的便宜模型（低倍率档）。
	// 置空数组 = 关闭回退。默认 deepseek-v4.1-flash → glm-5.3-flash。
	ModelFallback []string `json:"model_fallback"`

	// 解析后
	SoftRateDur         time.Duration `json:"-"`
	SoftRateMaxDur      time.Duration `json:"-"`
	BreakerCooldownDur  time.Duration `json:"-"`
	BreakerCooldownMaxD time.Duration `json:"-"`
	SessionTTL          time.Duration `json:"-"`
	SessionGCInterval   time.Duration `json:"-"`
	ExpiringSoonDur     time.Duration `json:"-"`
	NotifyThrottleDur   time.Duration `json:"-"`
	NotifyExpiringWin   time.Duration `json:"-"`
	ScriptTimeoutDur    time.Duration `json:"-"`
}

// Default 默认配置。
func Default() *Config {
	c := &Config{
		Listen:      ":7863",
		APIKey:      "",
		AuthDir:     "./auths",
		StateFile:   "./data/state.json",
		KeysFile:    "./data/keys.json",
		MetricsFile: "./data/metrics.json",
	}
	c.Cooldown.SoftRate = "600s"
	c.Cooldown.SoftRateMax = "2h"
	c.Server.MaxBodyMB = 8 // 请求体上限默认 8MB
	// auths 运行期自动发现缺省 30s（面板/脚本落盘新账号无需重启网关）。
	c.Server.AuthResyncSeconds = 30
	// 排程段默认值由 internal/config 集中维护（cmd/server 与 cmd/activity 共用，
	// 消除 issue #49 的默认值漂移）。
	c.Schedule = config.DefaultSchedule()
	c.Upstream.TimeoutSeconds = 120
	// HeaderTimeoutSeconds/IdleTimeoutSeconds 默认 0（未设置态），回落见 normalize()。
	c.Upstream.HeaderTimeoutSeconds = 0
	c.Upstream.IdleTimeoutSeconds = 0
	// Global.Enabled 缺省 true（纯 CN 行为不变：CN 账号恒判 cn，global base 不被使用）；
	// ChatBase/BillingBase 缺省空（回落内置默认）。
	c.Global.Enabled = true
	// 出站指纹默认伪造官方 WorkBuddy 桌面端：UA 三段式 + X-IDE-* 头组
	// （upstream.Client 的 attributionClientName 空值也回落 WorkBuddy，双保险）；
	// 显式 client_name="SaaS" 还原旧行为。
	c.Upstream.ClientName = "WorkBuddy"
	c.Features.SanitizeBlacklistFingerprints = true
	c.Prompt.Mode = "passthrough" // 缺省 passthrough：默认透传客户端原始 system；显式配置 custom 仍可覆盖回替换
	c.Pool.MaxInFlight = 3
	c.Pool.BreakerThreshold = 3
	c.Pool.BreakerCooldown = "30m"
	c.Pool.BreakerCooldownMax = "6h"
	c.Pool.IdleWeightPerHour = 0.5
	c.Pool.IdleWeightMax = 5.0
	c.Pool.ExpiringSoon = "168h" // 快过期窗口默认 7 天：官方活动奖励积分多在两周内过期
	c.SessionSticky.Enabled = true
	c.SessionSticky.TTL = "30m"
	c.SessionSticky.GCInterval = "5m"
	// 自动化默认启用（定时循环由 automation 模块驱动；enabled=false 回退旧循环）。
	c.Automation.Enabled = true
	c.Automation.HistoryFile = "./data/automation.json"
	c.Automation.ScriptTimeout = "10m"
	c.Automation.HistoryRuns = 100
	// Anthropic /v1/messages：未知模型名缺省路由到国内版自动档。
	c.Anthropic.DefaultModel = "cn:auto"
	// 模型回退白名单（低倍率档便宜模型；显式置空数组关闭回退）。
	c.ModelFallback = []string{"deepseek-v4.1-flash", "glm-5.3-flash"}
	// 通知缺省关闭（需填 SMTP 后显式开启）；阈值/节流/扫描时刻给缺省值，
	// 便于面板把整段表单渲染出来。ScanHours 缺省跟随签到后一小时（9/21 → 10/22）。
	c.Notify.Enabled = false
	c.Notify.SMTPPort = 587
	c.Notify.SMTPTLS = "starttls"
	c.Notify.CreditsThreshold = 100
	c.Notify.ExpiringDays = 7
	c.Notify.ThrottleHours = 6
	c.Notify.QueueSize = 256
	c.Notify.ScanHours = []int{10, 22}
	return c
}

// Load 从文件读，再用 WB2A_* env 覆盖。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("WB2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("WB2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("WB2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("WB2A_AUTOMATION_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Automation.Enabled = b
		}
	}
	if v := os.Getenv("WB2A_AUTOMATION_HISTORY_FILE"); v != "" {
		c.Automation.HistoryFile = v
	}
	if v := os.Getenv("WB2A_AUTOMATION_SCRIPT_TIMEOUT"); v != "" {
		c.Automation.ScriptTimeout = v
	}
	if v := os.Getenv("WB2A_AUTOMATION_HISTORY_RUNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Automation.HistoryRuns = n
		}
	}
	if v := os.Getenv("WB2A_METRICS_FILE"); v != "" {
		c.MetricsFile = v
	}
	if v := os.Getenv("WB2A_METRICS_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Server.MetricsEnabled = b
		}
	}
	if v := os.Getenv("WB2A_MAX_BODY_MB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Server.MaxBodyMB = n
		}
	}
	if v := os.Getenv("WB2A_AUTH_RESYNC_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Server.AuthResyncSeconds = n
		}
	}
	if v := os.Getenv("WB2A_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("WB2A_SOFT_RATE_MAX"); v != "" {
		c.Cooldown.SoftRateMax = v
	}
	if v := os.Getenv("WB2A_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_HEADER_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.HeaderTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_IDLE_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.IdleTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_USER_AGENT"); v != "" {
		c.Upstream.UserAgent = v
	}
	if v := os.Getenv("WB2A_DEVICE_TOKEN"); v != "" {
		c.Upstream.DeviceToken = v
	}
	if v := os.Getenv("WB2A_DEVICE_TOKEN_FILE"); v != "" {
		c.Upstream.DeviceTokenFile = v
	}
	if v := os.Getenv("WB2A_CLIENT_NAME"); v != "" {
		c.Upstream.ClientName = v
	}
	if v := os.Getenv("WB2A_CLIENT_VERSION"); v != "" {
		c.Upstream.ClientVersion = v
	}
	if v := os.Getenv("WB2A_CLI_VERSION"); v != "" {
		c.Upstream.CliVersion = v
	}
	if v := os.Getenv("WB2A_PASSTHROUGH_IP"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Upstream.PassthroughIP = b
		}
	}
	if v := os.Getenv("WB2A_SANITIZE_FINGERPRINTS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Features.SanitizeBlacklistFingerprints = b
		}
	}
	// 通知（邮箱提醒）env 覆盖
	if v := os.Getenv("WB2A_NOTIFY_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Notify.Enabled = b
		}
	}
	if v := os.Getenv("WB2A_SMTP_HOST"); v != "" {
		c.Notify.SMTPHost = v
	}
	if v := os.Getenv("WB2A_SMTP_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Notify.SMTPPort = n
		}
	}
	if v := os.Getenv("WB2A_SMTP_USERNAME"); v != "" {
		c.Notify.SMTPUsername = v
	}
	if v := os.Getenv("WB2A_SMTP_PASSWORD"); v != "" {
		c.Notify.SMTPPassword = v
	}
	if v := os.Getenv("WB2A_SMTP_FROM"); v != "" {
		c.Notify.SMTPFrom = v
	}
	if v := os.Getenv("WB2A_SMTP_TO"); v != "" {
		var to []string
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				to = append(to, p)
			}
		}
		if len(to) > 0 {
			c.Notify.SMTPTo = to
		}
	}
	if v := os.Getenv("WB2A_SMTP_TLS"); v != "" {
		c.Notify.SMTPTLS = v
	}
	if v := os.Getenv("WB2A_NOTIFY_CREDITS_THRESHOLD"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			c.Notify.CreditsThreshold = n
		}
	}
	if v := os.Getenv("WB2A_NOTIFY_EXPIRING_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Notify.ExpiringDays = n
		}
	}
	if v := os.Getenv("WB2A_NOTIFY_THROTTLE_HOURS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Notify.ThrottleHours = n
		}
	}
	if v := os.Getenv("WB2A_MODEL_FALLBACK"); v != "" {
		var fb []string
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				fb = append(fb, p)
			}
		}
		if len(fb) > 0 {
			c.ModelFallback = fb
		}
	}
	if v := os.Getenv("WB2A_NOTIFY_SCAN_HOURS"); v != "" {
		var hours []int
		for _, p := range strings.Split(v, ",") {
			if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil {
				hours = append(hours, n)
			}
		}
		if len(hours) > 0 {
			c.Notify.ScanHours = hours
		}
	}
	if v := os.Getenv("WB2A_PROMPT_MODE"); v != "" {
		c.Prompt.Mode = v
	}
	if v := os.Getenv("WB2A_PROMPT_FILE"); v != "" {
		c.Prompt.File = v
	}
	if v := os.Getenv("WB2A_EXPIRING_SOON"); v != "" {
		c.Pool.ExpiringSoon = v
	}
	if v := os.Getenv("WB2A_ANTHROPIC_DEFAULT_MODEL"); v != "" {
		c.Anthropic.DefaultModel = v
	}
	if v := os.Getenv("WB2A_ANTHROPIC_MODEL_MAP"); v != "" {
		// 形如 "claude-sonnet-4-5=cn:glm-5.3,claude-haiku-4-5=cn:auto"（逗号分隔 k=v，
		// 覆盖 merge 进文件配置的 model_map）。非法片段跳过不报错（环境变量宜宽容）。
		if c.Anthropic.ModelMap == nil {
			c.Anthropic.ModelMap = map[string]string{}
		}
		for _, pair := range strings.Split(v, ",") {
			k, val, found := strings.Cut(strings.TrimSpace(pair), "=")
			if !found || strings.TrimSpace(k) == "" {
				continue
			}
			c.Anthropic.ModelMap[strings.TrimSpace(k)] = strings.TrimSpace(val)
		}
	}
}

func (c *Config) normalize() error {
	var err error
	// max_body_mb 非法（0/负数）直接报错：0 若被静默当成默认 8MB，用户以为"不限"，
	// 大请求又被静默 413——不如 fail fast 提示显式配大上限。
	if c.Server.MaxBodyMB <= 0 {
		return fmt.Errorf("server.max_body_mb: %d 非法（需为正整数，单位 MB）", c.Server.MaxBodyMB)
	}
	// metrics_file / keys_file 空（显式 "" 或存量 config.json 缺字段）回落默认；
	// keys_file 为空会让 keys.Load 在空路径上落盘崩溃，必须兜底。
	if c.MetricsFile == "" {
		c.MetricsFile = "./data/metrics.json"
	}
	if c.KeysFile == "" {
		c.KeysFile = "./data/keys.json"
	}
	// automation 段：空值回落默认（与 state_file/metrics_file/keys_file 同口径）。
	if c.Automation.HistoryFile == "" {
		c.Automation.HistoryFile = "./data/automation.json"
	}
	if c.Automation.ScriptTimeout == "" {
		c.Automation.ScriptTimeout = "10m"
	}
	if c.ScriptTimeoutDur, err = time.ParseDuration(c.Automation.ScriptTimeout); err != nil {
		return fmt.Errorf("automation.script_timeout: %w", err)
	}
	if c.Automation.HistoryRuns <= 0 {
		c.Automation.HistoryRuns = 100
	}
	// anthropic.default_model 空回落 "cn:auto"（Default 已置；兜底显式 "" 场景）。
	if c.Anthropic.DefaultModel == "" {
		c.Anthropic.DefaultModel = "cn:auto"
	}
	// auths 自动发现间隔：负数回落默认 30s（显式 0 = 关闭，尊重用户选择）。
	if c.Server.AuthResyncSeconds < 0 {
		c.Server.AuthResyncSeconds = 30
	}
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	// 空值回落默认 2h（Default() 已置值；此兜底覆盖显式 "" 与 Default() 被绕过的场景）。
	if c.Cooldown.SoftRateMax == "" {
		c.Cooldown.SoftRateMax = "2h"
	}
	if c.SoftRateMaxDur, err = time.ParseDuration(c.Cooldown.SoftRateMax); err != nil {
		return fmt.Errorf("cooldown.soft_rate_max: %w", err)
	}
	if c.BreakerCooldownDur, err = time.ParseDuration(c.Pool.BreakerCooldown); err != nil {
		return fmt.Errorf("pool.breaker_cooldown: %w", err)
	}
	if c.BreakerCooldownMaxD, err = time.ParseDuration(c.Pool.BreakerCooldownMax); err != nil {
		return fmt.Errorf("pool.breaker_cooldown_max: %w", err)
	}
	if c.SessionTTL, err = time.ParseDuration(c.SessionSticky.TTL); err != nil {
		return fmt.Errorf("session_sticky.ttl: %w", err)
	}
	if c.SessionGCInterval, err = time.ParseDuration(c.SessionSticky.GCInterval); err != nil {
		return fmt.Errorf("session_sticky.gc_interval: %w", err)
	}
	if c.Pool.BreakerThreshold <= 0 {
		c.Pool.BreakerThreshold = 3
	}
	if c.Pool.IdleWeightPerHour <= 0 {
		c.Pool.IdleWeightPerHour = 0.5
	}
	if c.Pool.IdleWeightMax <= 0 {
		c.Pool.IdleWeightMax = 5.0
	}
	// 快过期窗口：空值回落默认 168h（Default 已置；此兜底覆盖显式 ""）；显式 "0"/负值 = 禁用分桶。
	if c.Pool.ExpiringSoon == "" {
		c.Pool.ExpiringSoon = "168h"
	}
	if c.ExpiringSoonDur, err = time.ParseDuration(c.Pool.ExpiringSoon); err != nil {
		return fmt.Errorf("pool.expiring_soon: %w", err)
	}
	if c.ExpiringSoonDur < 0 {
		c.ExpiringSoonDur = 0 // 负值视为禁用，避免 upstream 判定窗口反转
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	// header 缺省回落 timeout（保"首字节前换号"既有语义）；idle 缺省走内置大值。
	// 任务书约定：0 一律视为"未设置"走默认，真正的"禁用"留待后续（避免歧义）。
	if c.Upstream.HeaderTimeoutSeconds <= 0 {
		c.Upstream.HeaderTimeoutSeconds = c.Upstream.TimeoutSeconds
	}
	if c.Upstream.IdleTimeoutSeconds <= 0 {
		c.Upstream.IdleTimeoutSeconds = 300
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	// 排程段归一（空数组回落默认、ActivityReportCount 归一、小时范围校验）
	// 由 internal/config 统一实现，cmd/server 与 cmd/activity 共用同一份语义。
	if err := c.Schedule.Normalize(); err != nil {
		return err
	}
	// notify 段：映射为 notify.Config 并归一化校验（Enabled 但缺 SMTP 必需项 → 启动失败，
	// 避免"配了却不通知"的静默失效）；同时把解析后的时长回填与校验扫描时刻。
	nc, err := c.BuildNotifyConfig()
	if err != nil {
		return err
	}
	// 把归一化结果写回（面板/管理端读到的即生效值：非法端口、非法 tls 等已被纠正）。
	c.Notify.SMTPPort = nc.Port
	c.Notify.SMTPTLS = nc.TLSMode
	c.Notify.CreditsThreshold = nc.CreditsThreshold
	c.Notify.ExpiringDays = nc.ExpiringDays
	c.Notify.ThrottleHours = int(nc.Throttle / time.Hour)
	c.Notify.QueueSize = nc.QueueSize
	c.NotifyThrottleDur = nc.Throttle
	c.NotifyExpiringWin = time.Duration(nc.ExpiringDays) * 24 * time.Hour
	c.Notify.ScanHours = normalizeScanHours(c.Notify.ScanHours)
	return c.normalizePrompt()
}

// BuildNotifyConfig 把 config.json 的 notify 段映射为 notify.Config 并归一化。
func (c *Config) BuildNotifyConfig() (notify.Config, error) {
	nc := notify.Config{
		Enabled:          c.Notify.Enabled,
		Host:             c.Notify.SMTPHost,
		Port:             c.Notify.SMTPPort,
		Username:         c.Notify.SMTPUsername,
		Password:         c.Notify.SMTPPassword,
		From:             c.Notify.SMTPFrom,
		To:               c.Notify.SMTPTo,
		TLSMode:          c.Notify.SMTPTLS,
		CreditsThreshold: c.Notify.CreditsThreshold,
		ExpiringDays:     c.Notify.ExpiringDays,
		Throttle:         time.Duration(c.Notify.ThrottleHours) * time.Hour,
		QueueSize:        c.Notify.QueueSize,
		Events: notify.Events{
			// 三字段用 *bool：nil = 未配置（缺省开），显式 false = 关。
			CreditsLow:    boolOrTrue(c.Notify.Events.CreditsLow),
			Exhausted:     boolOrTrue(c.Notify.Events.Exhausted),
			AccountSwitch: boolOrTrue(c.Notify.Events.AccountSwitch),
		},
	}
	if err := nc.Normalize(); err != nil {
		return nc, err
	}
	return nc, nil
}

// boolOrTrue nil → true，否则取显式值。
func boolOrTrue(p *bool) bool {
	if p == nil {
		return true
	}
	return *p
}

// normalizeScanHours 去重、排序并剔除越界时刻（0-23）；空 = 不启用扫描任务。
func normalizeScanHours(in []int) []int {
	seen := map[int]bool{}
	out := make([]int, 0, len(in))
	for _, h := range in {
		if h < 0 || h > 23 || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	sort.Ints(out)
	return out
}

// normalizePrompt 校验 prompt.mode 并按 file 加载提示词文本（custom 模式）。
//
// mode 非法（非 custom/passthrough）启动报错，避免静默回落到某一分支；
// custom 模式下 file 非空但不可读 → 报错（fail fast），file 空 → 用内置默认。
// passthrough 模式不加载文本（透传客户端原始 system，文本在降级时用 prompt.Degraded）。
func (c *Config) normalizePrompt() error {
	switch m := strings.ToLower(strings.TrimSpace(c.Prompt.Mode)); m {
	case "", "passthrough":
		c.Prompt.Mode = "passthrough" // 缺省 passthrough：默认透传客户端原始 system
	case "custom":
		c.Prompt.Mode = "custom"
	default:
		return fmt.Errorf("prompt.mode: %q 不是合法值（custom / passthrough）", c.Prompt.Mode)
	}
	if c.Prompt.Mode == "custom" {
		text, err := prompt.Load(c.Prompt.Mode, c.Prompt.File)
		if err != nil {
			return err
		}
		c.PromptText = text
	}
	return nil
}
