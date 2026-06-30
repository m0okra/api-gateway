package main

import "time"

// ============================================================================
// 常量
// ============================================================================

const mask = "********"

const (
	opencodeGoServiceID   = "c7389bd0e731f80f49593e5ee53835475f4e28594dd6bd83eb229bab753498cd"
	stateSaveInterval     = 5 * time.Minute
	schedulerTickInterval = 1 * time.Second
	recoveryMinGap        = 60 * time.Second // 同一alias两次恢复触发之间的最小间隔，防止死循环
	streamIdleTimeout     = 5 * time.Minute  // 流式响应空闲读超时，超过则取消上游连接

	// 以下为非 count 型（usage/balance/fallback）恢复调度的时间兜底值
	defaultFallbackRecoverGap = 30 * time.Minute // fallback 兜底下次复查间隔
	defaultBalanceRecoverGap  = 30 * time.Minute // balance provider 未提供 reset 时默认间隔
	minRecoverGap             = 60 * time.Second // provider resetInSec 异常时的地板保护
)

// 可用性类型
const (
	availCount    = "count"
	availUsage    = "usage"
	availBalance  = "balance"
	availFallback = "fallback"
)

// maxBodySize 限制单次请求体大小，避免恶意超大body导致OOM
const maxBodySize int64 = 32 << 20 // 32MB

// cronCacheCap cron解析缓存LRU容量上限
const cronCacheCap = 256
