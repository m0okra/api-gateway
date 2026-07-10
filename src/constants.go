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
	recoveryMinGap        = 60 * time.Second // 同一upstream两次恢复触发之间的最小间隔，防止死循环
	streamIdleTimeout     = 5 * time.Minute  // 流式响应空闲读超时，超过则取消上游连接

	// saveStateTimeout saveState 事务的超时上限。
	// SIGTERM 后优雅停机期间 final save 用 shutdownCtx（10s），周期 save 用此值独立超时。
	// 防止卡在 SQLite busy_timeout（5s）后仍拖慢停机。
	saveStateTimeout = 10 * time.Second

	// 以下为非 count 型（usage/balance/exhaust）恢复调度的时间兜底值
	defaultFallbackRecoverGap = 30 * time.Minute // exhaust 兜底下次复查间隔
	defaultBalanceRecoverGap  = 30 * time.Minute // balance provider 未提供 reset 时默认间隔
	minRecoverGap             = 60 * time.Second // provider resetInSec 异常时的地板保护

	// providerRetryTimeouts 可用性检查 HTTP 请求的三阶段重试超时（2s + 4s + 8s）。
	// 短超时快速捕获瞬态抖动，长超时给慢响应留余地；仅网络错误/5xx/429 触发重试，
	// 2xx 成功直接返回，其他 4xx 视为确定失败不重试。
	providerRetryStage1 = 2 * time.Second
	providerRetryStage2 = 4 * time.Second
	providerRetryStage3 = 8 * time.Second
)

// 可用性类型
const (
	availCount       = "count"
	availUsage       = "usage"
	availBalance     = "balance"
	availExhaust     = "exhaust"
	availPassthrough = "none"
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
