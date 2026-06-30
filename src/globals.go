package main

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"
)

// ============================================================================
// 全局状态
// ============================================================================

var (
	tokenMap   *TokenMapConfig
	stateMap   map[string]*AvailabilityState
	mu         sync.Mutex // 保护 tokenMap.FakeTokens 队列与 stateMap
	stateDirty bool
	dbPath     = "gateway.db"
	db         *sql.DB
	cronCache  = newCronLRU(cronCacheCap) // cron 表达式解析缓存（LRU）
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

	// defaultClient 用于provider可用性检查（httpGetJSON/httpGetText）
	defaultClient = &http.Client{
		Timeout:   15 * time.Second,
		Transport: sharedTransport,
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
