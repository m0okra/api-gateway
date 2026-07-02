package main

import "time"

// ============================================================================
// 数据结构
// ============================================================================

// TokenMapConfig 是 EncryptedTokenMap 的顶层结构，包含两个映射：
//   - FakeTokens: fakeToken -> alias队列（按优先级排序）
//   - Aliases:    alias -> 实际工作所需内容（realToken、targetBase、可用性配置等）
type TokenMapConfig struct {
	FakeTokens map[string][]string    `json:"fakeTokens"`
	Aliases    map[string]AliasConfig `json:"aliases"`
}

// AliasConfig 单个alias的配置
type AliasConfig struct {
	RealToken    string              `json:"realToken"`
	TargetBase   string              `json:"targetBase"`
	Availability *AvailabilityConfig `json:"availability,omitempty"`
	// Extra 持久储存provider所需的额外内容（如opencode-go的cookie/workspace_id）
	// 对于从文件读取的内容（如gemini oauth_creds）则不储存
	Extra map[string]string `json:"extra,omitempty"`
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
	TokenMap *TokenMapConfig              `json:"tokenMap"`
	State    map[string]*AvailabilityState `json:"state"`
}
