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

// HTTP Server 并发与超时保护
const (
	// maxConcurrentReqs 全局并发请求上限（channel semaphore）。超过则请求阻塞排队，
	// 防止高并发下无限 goroutine 爆炸。256 在中等并发场景下兼顾吞吐与资源。
	maxConcurrentReqs = 256
	// serverReadTimeout 读取请求头/体的超时，防御慢速连接（Slowloris）攻击。
	// 注意：代理请求体可能较大（上限 32MB），10s 足以读取完毕。
	serverReadTimeout = 10 * time.Second
	// serverIdleTimeout keep-alive 空闲连接超时，回收闲置连接资源。
	serverIdleTimeout = 120 * time.Second
	// serverMaxHeaderBytes 请求头上限，防止超大头部滥用。
	serverMaxHeaderBytes = 1 << 20 // 1MB

	// maxStreamLife 流式（SSE）请求最大生命周期。
	// streamClient 本身无整体超时（Timeout=0），仅有 streamIdleTimeout 空闲读保护。
	// 这里给流式上下文加一个硬性上限作为兜底：即便空闲监控 goroutine 在边界情况
	// 未及时触发（如 channel 调度阻塞），maxStreamLife 后也会强制取消上游连接，
	// 防止流式 goroutine 无限堆积。30min 兼顾合法长流与资源回收。
	maxStreamLife = 30 * time.Minute
)
