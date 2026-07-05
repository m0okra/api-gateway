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
	stateGen atomic.Uint64
	dbPath     = "gateway.db"
	db         *sql.DB
	cronCache  = newCronLRU(cronCacheCap) // cron 表达式解析缓存（LRU）
	// reqSem 并发请求信号量（channel semaphore）。在 main 初始化为容量 maxConcurrentReqs。
	// handler 入口 acquire（写入），defer release（读出），实现全局并发上限保护。
	reqSem chan struct{}

	// availSF 可用性检查 singleflight：同一 upstream 的并发 provider 检查合并为一次实际调用，
	// 避免上游批量返回 401/429 时对同一 upstream 发起重复外部 HTTP 检查风暴。
	availSF = newAvailSingleFlight()
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
