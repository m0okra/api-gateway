package main

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================================
// 全局状态
// ============================================================================

var (
	tokenMap   *TokenMapConfig
	stateMap   map[string]*AvailabilityState
	mu         sync.RWMutex // 保护 tokenMap.FakeTokens 队列与 stateMap；读多写少用 RWMutex
	stateDirty bool
	// stateGen 代际计数器：每次标记脏时自增。saveState 快照后据此判断提交期间是否有新变更，
	// 避免误清 stateDirty 导致快照后产生的写入丢失。需在持 mu 写锁时操作。
	stateGen  atomic.Uint64
	dbPath    = "gateway.db"
	db        *sql.DB
	cronCache = newCronLRU(cronCacheCap) // cron 表达式解析缓存（LRU）
	// reqSem 并发请求信号量（channel semaphore）。在 main 初始化为容量 maxConcurrentReqs。
	// handler 入口 acquire（写入），defer release（读出），实现全局并发上限保护。
	reqSem chan struct{}

	// availSF 可用性检查 singleflight：同一 upstream 的并发 provider 检查合并为一次实际调用，
	// 避免上游批量返回 401/429 时对同一 upstream 发起重复外部 HTTP 检查风暴。
	availSF = newAvailSingleFlight()
)

// 流式（SSE）超时参数：编译期默认值见 constants.go，可在启动时通过
// -stream-idle-timeout / -stream-max-life 覆盖（flag.Parse 前绑定到此二变量）。
// 仅在 main 启动阶段写入，启动完成后只读，故无需加锁。
var (
	// streamIdleTimeout 流式响应空闲读超时：连续无数据超过该时长则取消上游连接。
	streamIdleTimeout = defaultStreamIdleTimeout
	// maxStreamLife 流式请求最大生命周期硬上限：到期强制取消上游连接。
	maxStreamLife = defaultMaxStreamLife
)

// 请求体上限与服务端读超时：编译期默认值见 constants.go，
// 可分别在启动时通过 -max-body-size / -read-timeout 覆盖。
// 两者共同决定「大 body 能否在上限内传完」：所需客户端上传速率 ≥ maxBodySize/serverReadTimeout，
// 低于此值时请求会先撞读超时（500），而不是拿到 413。
// 仅在 main 启动阶段写入，启动完成后只读，故无需加锁。
var (
	// maxBodySize 单次请求体上限。语义：请求体超过该字节数即返回 413，
	// 且不再转发上游（连一次 attempt 都不会发起）。
	maxBodySize = defaultMaxBodySize
	// serverReadTimeout HTTP 服务端读超时（http.Server.ReadTimeout），
	// 覆盖「读完请求头 + 请求体」的总时长，兼防慢速连接（Slowloris）攻击。
	serverReadTimeout = defaultServerReadTimeout
)

// ============================================================================
// 共享 HTTP 客户端（复用连接，避免每次请求创建Transport）
// ============================================================================

var (
	// sharedTransport 所有共享client使用的底层Transport
	sharedTransport = &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
	}

	// proxyClient 用于普通（非流式）反向代理请求
	proxyClient = &http.Client{
		Timeout:   120 * time.Second,
		Transport: sharedTransport,
	}

	// streamClient 用于流式代理请求（无整体超时，由 streamIdleTimeout 控制）
	streamClient = &http.Client{
		Timeout:   0,
		Transport: sharedTransport,
	}
)

// ============================================================================
// 通用响应工具
// ============================================================================

// writeJSON 统一写回JSON响应并捕获编码错误，避免多次错误被静默忽略。
// 调用方应在调用本函数之前不再手动 WriteHeader。
func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[resp] writeJSON encode failed: %v", err)
	}
}
