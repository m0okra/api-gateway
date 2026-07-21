package main

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// ============================================================================
// 数据结构
// ============================================================================

// TokenMapConfig 是 EncryptedTokenMap 的顶层结构，包含两个映射：
//   - FakeTokens: fakeToken -> upstream队列（按优先级排序）
//   - Upstreams:    upstream -> 实际工作所需内容（realToken、targetBase、可用性配置等）
type TokenMapConfig struct {
	FakeTokens map[string][]string       `json:"fakeTokens"`
	Upstreams  map[string]UpstreamConfig `json:"upstreams"`
}

// UpstreamConfig 单个upstream的配置
type UpstreamConfig struct {
	RealToken    string              `json:"realToken"`
	TargetBase   string              `json:"targetBase"`
	Availability *AvailabilityConfig `json:"availability,omitempty"`
	// Extra 持久储存provider所需的额外内容（如opencode-go的cookie/workspace_id）
	// 对于从文件读取的内容（如gemini oauth_creds）则不储存
	Extra map[string]string `json:"extra,omitempty"`
	// FormatTransform 控制网关在转发前/后做 API 格式转换。
	// 取值：openai | openai_responses | anthropic | gemini | ""（空=透传）
	FormatTransform string `json:"formatTransform,omitempty"`
	// CacheInjection 控制 Anthropic Prompt Caching 自动断点注入。
	// 仅在目标格式为 anthropic 时生效。nil 或 Enabled=false 时不注入。
	CacheInjection *CacheInjectorConfig `json:"cacheInjection,omitempty"`
	// Aliases 模型别名映射（per-upstream）：key 为客户端请求中的模型名，
	// value 为转发到上游时替换后的真实模型名。nil 表示该 upstream 不启用别名。
	// 在模型列表响应中按 value→key 反向展开：同时保留真实模型名条目与 alias 条目，
	// 二者字段完全相同，客户端能从列表命中 alias 并请求。
	Aliases map[string]string `json:"aliases,omitempty"`
}

// CacheInjectorConfig 控制 Anthropic Prompt Caching 自动 cache_control 断点注入。
type CacheInjectorConfig struct {
	Enabled bool `json:"enabled"`
	// TTL 取值 "5m"（默认）或 "1h"。空串视为 "5m"。
	// "5m" 对应 Anthropic 的 ephemeral 默认 5 分钟；"1h" 对应 1 小时扩展缓存。
	TTL string `json:"ttl,omitempty"`
}

// AvailabilityConfig 可用性配置，对应5种基础类型
type AvailabilityConfig struct {
	Type string `json:"type"` // count|usage|balance|exhaust|none

	// count型参数
	Limit       int    `json:"limit,omitempty"`
	RefreshCron string `json:"refreshCron,omitempty"` // 次数刷新cron（运行中不变，预设定）

	// usage/balance型参数
	Provider string       `json:"provider,omitempty"` // provider标识，决定调用哪个检查实现
	Tiers    []TierConfig `json:"tiers,omitempty"`    // usage型的配额层级列表
}

// TierConfig usage型的配额层级配置
type TierConfig struct {
	Name string `json:"name"`
}

// AvailabilityState 可用性运行时状态，持久化在 AvailabilityState 中
// 大体格式与 EncryptedTokenMap 相同，只是多一个是否exhaust
type AvailabilityState struct {
	Exhausted bool `json:"exhausted"`

	// count型
	Count int `json:"count,omitempty"`

	// balance型
	Balance float64 `json:"balance,omitempty"`

	// usage型
	Tiers []TierState `json:"tiers,omitempty"`

	// 恢复调度依据（两选一）：
	//   - count 型：RecoveryCron 等于配置的 RefreshCron，调度器按 cron 周期匹配触发重置
	//   - usage/balance/exhaust 型：RecoveryAt 为下次 provider 复查时间点，
	//     由 provider 在 exhaust 时基于最长 resetInSec 设定（精确一次性定时）
	RecoveryCron string    `json:"recoveryCron,omitempty"`
	RecoveryAt   time.Time `json:"recoveryAt,omitempty"`
	LastRecovery time.Time `json:"lastRecovery,omitempty"`
	LastChecked  time.Time `json:"lastChecked,omitempty"`
}

// TierState usage型层级的运行时状态
type TierState struct {
	Name       string  `json:"name"`
	UsedPct    float64 `json:"usedPct"`
	ResetInSec int     `json:"resetInSec,omitempty"`
}

// AvailabilityResult 是provider检查的返回
type AvailabilityResult struct {
	Exhausted bool
	// 下面的字段用于更新state（provider填充）
	Count   int
	Balance float64
	Tiers   []TierState
	// RecoveryCron 仅count型使用，对应配置的 RefreshCron
	RecoveryCron string
	// RecoveryAt 仅usage/balance/exhaust型使用，下次 provider 复查时间点
	RecoveryAt time.Time
}

// DBDump 是 -e/-i 导入导出所用的 JSON 顶层结构。
// 单个文件包含配置（TokenMap）与运行时状态（State）两部分，便于备份/迁移/手工编辑。
type DBDump struct {
	TokenMap   *TokenMapConfig               `json:"tokenMap"`
	State      map[string]*AvailabilityState `json:"state"`
	Accounts   []AccountDump                 `json:"accounts,omitempty"`
	IPSessions []IPSessionDump               `json:"ipSessions,omitempty"`
}

type AccountDump struct {
	Username     string `json:"username"`
	PasswordHash string `json:"passwordHash"`
}

type IPSessionDump struct {
	IP       string `json:"ip"`
	Username string `json:"username"`
	LoginAt  string `json:"loginAt"`
}

// ============================================================================
// 配置校验（ISSUES #7）
//
// 每个配置结构提供 Validate()，在 loadFromDB / importFromJSON 时集中调用。
// 校验失败 → fail-fast：loadFromDB 让进程退出，importFromJSON 在触碰 DB 前拒绝。
// 错误收集策略：收集全部错误一次性返回，让用户一次看到所有问题。
// ============================================================================

// validFormatTransformValues 是 FormatTransform 字段的合法取值集合（不含空串，空串=透传合法）。
var validFormatTransformValues = map[string]bool{
	formatOpenAIChat:      true,
	formatOpenAIResponses: true,
	formatAnthropic:       true,
	formatGemini:          true,
}

// validAvailabilityTypes 是 AvailabilityConfig.Type 字段的合法取值集合。
var validAvailabilityTypes = map[string]bool{
	availCount:       true,
	availUsage:       true,
	availBalance:     true,
	availExhaust:     true,
	availPassthrough: true,
}

// validCacheTTLs 是 CacheInjectorConfig.TTL 字段的合法取值集合（空串=默认5m）。
var validCacheTTLs = map[string]bool{
	"":   true,
	"5m": true,
	"1h": true,
}

// Validate 校验整个 TokenMapConfig：遍历所有 upstream，收集全部错误。
// 空配置（无 upstream）合法——空库启动是正常场景。
func (t *TokenMapConfig) Validate() error {
	var errs []string
	for name, upstream := range t.Upstreams {
		if err := upstream.Validate(); err != nil {
			errs = append(errs, fmt.Sprintf("upstream %q: %s", name, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("config validation failed:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// Validate 校验单个 upstream 配置。
// RealToken 可为空（本地 Ollama 等无需鉴权的 upstream 合法）。
func (u *UpstreamConfig) Validate() error {
	var errs []string

	// TargetBase：非空 + 合法 URL + scheme 为 http/https + host 非空
	if u.TargetBase == "" {
		errs = append(errs, "targetBase is empty")
	} else {
		parsed, err := url.Parse(u.TargetBase)
		if err != nil {
			errs = append(errs, fmt.Sprintf("targetBase parse error: %v", err))
		} else if parsed.Scheme != "http" && parsed.Scheme != "https" {
			errs = append(errs, fmt.Sprintf("targetBase scheme must be http or https, got %q", parsed.Scheme))
		} else if parsed.Host == "" {
			errs = append(errs, "targetBase host is empty")
		}
	}

	// FormatTransform：空串合法（透传），非空必须在合法集合内
	if u.FormatTransform != "" && !validFormatTransformValues[u.FormatTransform] {
		errs = append(errs, fmt.Sprintf("formatTransform %q is invalid (valid: openai, openai_responses, anthropic, gemini)", u.FormatTransform))
	}

	// Availability：非 nil 时递归校验
	if u.Availability != nil {
		if err := u.Availability.Validate(); err != nil {
			errs = append(errs, err.Error())
		}
	}

	// CacheInjection：非 nil 时递归校验
	if u.CacheInjection != nil {
		if err := u.CacheInjection.Validate(); err != nil {
			errs = append(errs, err.Error())
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// Validate 校验可用性配置。
func (a *AvailabilityConfig) Validate() error {
	var errs []string

	// Type：必须在合法集合内
	if !validAvailabilityTypes[a.Type] {
		errs = append(errs, fmt.Sprintf("availability.type %q is invalid (valid: count, usage, balance, exhaust, none)", a.Type))
	}

	switch a.Type {
	case availCount:
		// count 型：Limit 必须 > 0，否则 Count>=0 立即耗尽
		if a.Limit <= 0 {
			errs = append(errs, "availability.limit must be > 0 for count type")
		}
	case availBalance, availUsage:
		// balance/usage 型：Provider 必须非空，否则静默走 fallbackResult
		if a.Provider == "" {
			errs = append(errs, fmt.Sprintf("availability.provider is required for %s type", a.Type))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// Validate 校验缓存注入配置。
// Enabled=false 时跳过 TTL 校验（未启用时不阻拦）。
func (c *CacheInjectorConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if !validCacheTTLs[c.TTL] {
		return fmt.Errorf("cacheInjection.ttl %q is invalid (valid: \"5m\", \"1h\", or empty for default)", c.TTL)
	}
	return nil
}
