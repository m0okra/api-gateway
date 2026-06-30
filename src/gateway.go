package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// ============================================================================
// 网关：HTTP 头/URL 处理（沿用原实现）
// ============================================================================

var keyQueryRegex = regexp.MustCompile(`([?&])key=([^&#]*)`)

func maskURL(rawURL string) string {
	return keyQueryRegex.ReplaceAllString(rawURL, "${1}key="+mask)
}

func maskHeadersStr(headers http.Header) string {
	c := headers.Clone()
	if v := c.Get("Authorization"); v != "" {
		if strings.HasPrefix(strings.ToLower(v), "bearer ") {
			c.Set("Authorization", "Bearer "+mask)
		} else {
			c.Set("Authorization", mask)
		}
	}
	if c.Get("X-Goog-Api-Key") != "" {
		c.Set("X-Goog-Api-Key", mask)
	}
	if c.Get("X-Api-Key") != "" {
		c.Set("X-Api-Key", mask)
	}
	b, _ := json.Marshal(c)
	return string(b)
}

var streamHeaderRegex = regexp.MustCompile(`^(content-type|cache-control|x-request-id)$`)

func forwardStreamHeaders(src, dst http.Header) {
	for k, vs := range src {
		if streamHeaderRegex.MatchString(strings.ToLower(k)) {
			dst.Del(k)
			for _, v := range vs {
				dst.Add(k, v)
			}
		}
	}
	if dst.Get("Content-Type") == "" {
		dst.Set("Content-Type", "text/event-stream")
		dst.Set("Cache-Control", "no-cache")
	}
}

func removeHopHeaders(h http.Header) {
	for _, k := range []string{
		"Host", "Connection", "Content-Length",
		"Accept-Encoding", "Transfer-Encoding",
		"Authorization", "X-Goog-Api-Key", "X-Api-Key",
	} {
		h.Del(k)
	}
}

// isAvailabilityError 判断状态码是否为可用性错误（401/402/403/429）
func isAvailabilityError(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusPaymentRequired,
		http.StatusForbidden, http.StatusTooManyRequests:
		return true
	}
	return false
}

// ============================================================================
// 网关：alias队列轮转（带锁读取）
// ============================================================================

// pickFirstAlias 取队列首部alias名（不删除）。若队列为空返回空串。
func pickFirstAlias(fakeToken string) string {
	mu.Lock()
	defer mu.Unlock()
	q := tokenMap.FakeTokens[fakeToken]
	if len(q) == 0 {
		return ""
	}
	return q[0]
}

// rotateAliasToEnd 将指定alias移到其fakeToken队列末端
func rotateAliasToEnd(fakeToken, alias string) {
	mu.Lock()
	defer mu.Unlock()
	q := tokenMap.FakeTokens[fakeToken]
	idx := -1
	for i, a := range q {
		if a == alias {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	q = append(q[:idx], q[idx+1:]...)
	q = append(q, alias)
	tokenMap.FakeTokens[fakeToken] = q
}

// getAliasQueueLen 返回 fakeToken 对应队列长度（加锁读取，避免与 rotate 竞争）
func getAliasQueueLen(fakeToken string) int {
	mu.Lock()
	defer mu.Unlock()
	return len(tokenMap.FakeTokens[fakeToken])
}

// hasAliasQueue 队列是否存在且非空（加锁读取）
func hasAliasQueue(fakeToken string) bool {
	return getAliasQueueLen(fakeToken) > 0
}

// isAliasExhausted 读取alias的exhausted状态。
// stateMap 中不存在的 alias 视为"已耗尽"（保守策略），避免转发到未初始化的 alias。
func isAliasExhausted(alias string) bool {
	mu.Lock()
	defer mu.Unlock()
	st := stateMap[alias]
	if st == nil {
		log.Printf("[state] alias=%s has no state -> treat as exhausted (conservative)", alias)
		return true
	}
	return st.Exhausted
}

// incrementCount count型alias请求计数+1，返回新的count；同时若达到limit则标记exhaust
// Limit 为 0 时永不 exhaust（无限制计数）
// 返回 (newCount, nowExhausted)
func incrementCount(alias string) (int, bool) {
	mu.Lock()
	defer mu.Unlock()
	st := stateMap[alias]
	if st == nil {
		st = initStateFor(alias)
		stateMap[alias] = st
	}
	cfg := tokenMap.Aliases[alias].Availability
	if cfg == nil || cfg.Type != availCount {
		return 0, false
	}
	st.Count++
	stateDirty = true
	if cfg.Limit > 0 && st.Count >= cfg.Limit {
		st.Exhausted = true
		if st.RecoveryCron == "" && cfg.RefreshCron != "" {
			st.RecoveryCron = cfg.RefreshCron
		}
		return st.Count, true
	}
	return st.Count, false
}

// applyAvailabilityResult 将provider检查结果应用到state，返回是否exhausted
func applyAvailabilityResult(alias string, res AvailabilityResult) bool {
	mu.Lock()
	defer mu.Unlock()
	st := stateMap[alias]
	if st == nil {
		st = initStateFor(alias)
		stateMap[alias] = st
	}
	st.Exhausted = res.Exhausted
	st.LastChecked = time.Now()

	cfg := tokenMap.Aliases[alias].Availability
	isCount := cfg != nil && cfg.Type == availCount
	if isCount {
		// count 型：恢复依据是 cron（= RefreshCron），不使用 RecoveryAt
		if res.RecoveryCron != "" {
			st.RecoveryCron = res.RecoveryCron
		}
		st.RecoveryAt = time.Time{}
	} else {
		// usage/balance/fallback型：恢复依据是精确时间点 RecoveryAt
		if !res.RecoveryAt.IsZero() {
			st.RecoveryAt = res.RecoveryAt
		} else if res.Exhausted {
			// provider 未填 RecoveryAt 但已 exhaust，给予兜底时间点
			st.RecoveryAt = time.Now().Add(defaultFallbackRecoverGap)
		}
		st.RecoveryCron = "" // 清理旧文件中遗留的 cron，避免调度器误判
	}

	// 更新各类型状态字段
	if cfg != nil {
		switch cfg.Type {
		case availBalance:
			st.Balance = res.Balance
		case availUsage:
			if res.Tiers != nil {
				st.Tiers = res.Tiers
			}
		}
	}
	stateDirty = true
	return res.Exhausted
}

// ============================================================================
// 网关：请求转发 + alias队列轮转
// ============================================================================

// handler 主请求处理：fakeToken -> alias队列轮转
func handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-goog-api-key")

	if r.Method == http.MethodOptions {
		log.Printf("[OPTIONS] %s -> 204", maskURL(r.URL.String()))
		w.WriteHeader(http.StatusNoContent)
		return
	}

	start := time.Now()
	log.Printf("\n[REQ] %s %s", r.Method, maskURL(r.URL.String()))
	log.Printf("  headers: %s", maskHeadersStr(r.Header))

	var bearer string
	if auth := r.Header.Get("Authorization"); auth != "" {
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			bearer = auth[7:]
		}
	}
	googHeader := r.Header.Get("X-Goog-Api-Key")
	queryKey := r.URL.Query().Get("key")
	apiKeyHeader := r.Header.Get("X-Api-Key")

	fakeToken := bearer
	if fakeToken == "" {
		fakeToken = googHeader
	}
	if fakeToken == "" {
		fakeToken = queryKey
	}
	if fakeToken == "" {
		fakeToken = apiKeyHeader
	}

	// Issue 2: 读取 FakeTokens 队列须加锁
	if !hasAliasQueue(fakeToken) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Invalid token"})
		return
	}
	maxAttempts := getAliasQueueLen(fakeToken)
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	// Issue 3 & 4: 读取请求体 限制最大 32MB，并处理读取错误
	// 注意：http.MaxBytesReader 在超限时已内部调用 w.WriteHeader(413)，
	// 故这里使用 errors.As 判断 *http.MaxBytesError 后直接 return，
	// 避免再次 writeJSON 触发 "superfluous WriteHeader" 警告。
	var bodyBytes []byte
	hasBody := false
	if r.Body != nil {
		limited := http.MaxBytesReader(w, r.Body, maxBodySize)
		read, rerr := io.ReadAll(limited)
		r.Body.Close()
		var maxBytesErr *http.MaxBytesError
		if errors.As(rerr, &maxBytesErr) {
			// 超限：413 状态码已由 MaxBytesReader 写入，直接终止
			return
		}
		if rerr != nil {
			writeJSON(w, http.StatusInternalServerError,
				map[string]string{"error": "Read request body failed: " + rerr.Error()})
			return
		}
		if len(read) > 0 {
			bodyBytes = read
			hasBody = true
		}
	}

	isStream := strings.Contains(r.Header.Get("Accept"), "text/event-stream") ||
		r.URL.Query().Get("alt") == "sse"
	if hasBody && !isStream {
		var bodyMap map[string]interface{}
		if json.Unmarshal(bodyBytes, &bodyMap) == nil {
			if s, ok := bodyMap["stream"].(bool); ok && s {
				isStream = true
			}
		}
	}

	// 判断 token 注入方式（与原实现一致：query key / goog header / x-api-key / bearer）
	useGoogHeader := googHeader != ""
	useQueryKey := queryKey != ""
	useAPIKeyHeader := apiKeyHeader != ""

	// 选择共享client（流式无整体超时，由 streamIdleTimeout 控制）
	client := proxyClient
	if isStream {
		client = streamClient
	}

	// alias 队列轮转：最多尝试 maxAttempts 次（避免全部exhaust时无限循环）
	for attempt := 0; attempt < maxAttempts; attempt++ {
		alias := pickFirstAlias(fakeToken)
		if alias == "" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "No available alias"})
			return
		}
		// 队列首部alias已exhaust → 轮转到队尾，尝试下一个alias
		if isAliasExhausted(alias) {
			log.Printf("[ROTATE] fakeToken=%s alias=%s exhausted -> rotate (attempt=%d)", maskFakeToken(fakeToken), alias, attempt+1)
			rotateAliasToEnd(fakeToken, alias)
			continue
		}

		// 取alias配置
		aliasCfg := getAliasConfig(alias)
		if aliasCfg == nil {
			// 配置缺失，直接当作exhaust处理并轮转
			log.Printf("[ROTATE] alias=%s config missing -> rotate", alias)
			applyAvailabilityResult(alias, fallbackResult(nil))
			rotateAliasToEnd(fakeToken, alias)
			continue
		}

		// 构造目标请求
		query := r.URL.Query()
		query.Del("key")

		outHeaders := r.Header.Clone()
		removeHopHeaders(outHeaders)

		// Issue 11: Token 注入简化（语义与原实现等价）
		// 仅当对应输入存在时注入对应的输出字段；两者都存在则同时注入；
		// 若两者都不在则回退到 X-Api-Key 或 Authorization。
		if useGoogHeader || useQueryKey {
			if useGoogHeader {
				outHeaders.Set("X-Goog-Api-Key", aliasCfg.RealToken)
			}
			if useQueryKey {
				query.Set("key", aliasCfg.RealToken)
			}
		} else if useAPIKeyHeader {
			outHeaders.Set("X-Api-Key", aliasCfg.RealToken)
		} else {
			outHeaders.Set("Authorization", "Bearer "+aliasCfg.RealToken)
		}

		targetURL := aliasCfg.TargetBase + r.URL.Path
		if encoded := query.Encode(); encoded != "" {
			targetURL += "?" + encoded
		}

		var bodyReader io.Reader
		if hasBody {
			bodyReader = bytes.NewReader(bodyBytes)
		}

		// Issue 1: 移除循环内 defer cancelReq()，改在每个退出分支显式调用
		reqCtx, cancelReq := context.WithCancel(r.Context())
		req, err := http.NewRequestWithContext(reqCtx, r.Method, targetURL, bodyReader)
		if err != nil {
			cancelReq()
			log.Printf("[ERR] %s %s -> %v", r.Method, maskURL(targetURL), err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Proxy error"})
			return
		}
		req.Header = outHeaders

		resp, err := client.Do(req)
		if err != nil {
			cancelReq()
			log.Printf("[ERR] %s %s -> %v", r.Method, maskURL(targetURL), err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Proxy error"})
			return
		}

		// count型：请求发出即计数（不论结果），达limit则exhaust
		if aliasCfg.Availability != nil && aliasCfg.Availability.Type == availCount {
			_, nowExhausted := incrementCount(alias)
			if nowExhausted {
				log.Printf("[COUNT] alias=%s reached limit -> exhaust+rotate", alias)
				rotateAliasToEnd(fakeToken, alias)
				// 若响应是可用性错误，继续轮转；否则照常转发响应
			}
		}

		log.Printf("[RES] %s %s -> %d (%dms) alias=%s",
			r.Method, maskURL(r.URL.String()), resp.StatusCode, time.Since(start).Milliseconds(), alias)

		// 可用性错误 → 触发可用性检查
		if isAvailabilityError(resp.StatusCode) {
			// 先把错误响应体读出来（错误响应通常很小）
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			// 调用可用性检查（count型走自身判断；其它调用provider）
			var stCopy *AvailabilityState
			mu.Lock()
			stCopy = stateMap[alias]
			mu.Unlock()
			res := checkAvailability(alias, aliasCfg.Availability, stCopy)
			exhausted := applyAvailabilityResult(alias, res)

			if exhausted {
				log.Printf("[AVAIL] alias=%s exhausted (status=%d) -> rotate", alias, resp.StatusCode)
				rotateAliasToEnd(fakeToken, alias)
				cancelReq() // 释放本次迭代context（continue前）
				// 继续尝试下一个alias
				continue
			}
			// 非exhaust但返回可用性错误 → 把错误响应返回给客户端
			for k, vs := range resp.Header {
				k = http.CanonicalHeaderKey(k)
				if k == "Content-Length" || k == "Transfer-Encoding" || k == "Connection" {
					continue
				}
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(resp.StatusCode)
			if _, werr := w.Write(errBody); werr != nil {
				log.Printf("[resp] write error response failed: %v", werr)
			}
			cancelReq()
			return
		}

		// 成功或其它状态 → 转发响应（不重试）
		if isStream {
			forwardStreamHeaders(resp.Header, w.Header())
			w.WriteHeader(resp.StatusCode)
			flusher, _ := w.(http.Flusher)
			buf := make([]byte, 4096)
			// 空闲读超时：持续无数据超过 streamIdleTimeout 则取消上游连接
			idle := time.NewTimer(streamIdleTimeout)
			go func() {
				select {
				case <-idle.C:
					cancelReq() // 中断 resp.Body.Read
				case <-reqCtx.Done():
				}
			}()
			for {
				n, rerr := resp.Body.Read(buf)
				if n > 0 {
					idle.Reset(streamIdleTimeout)
					if _, werr := w.Write(buf[:n]); werr != nil {
						log.Printf("[resp] stream write failed: %v", werr)
					}
					if flusher != nil {
						flusher.Flush()
					}
				}
				if rerr != nil {
					break
				}
			}
			idle.Stop()
			resp.Body.Close()
			cancelReq() // 通知空闲监听goroutine退出（并释放本次迭代context）
		} else {
			for k, vs := range resp.Header {
				k = http.CanonicalHeaderKey(k)
				if k == "Content-Length" || k == "Transfer-Encoding" || k == "Connection" {
					continue
				}
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(resp.StatusCode)
			if _, err := io.Copy(w, resp.Body); err != nil {
				log.Printf("[resp] copy response failed: %v", err)
			}
			resp.Body.Close()
			cancelReq() // 释放本次迭代context
		}
		return
	}

	// 所有尝试都用尽（全部exhaust）
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "All aliases exhausted"})
}

// maskFakeToken 简单脱敏fakeToken用于日志
func maskFakeToken(t string) string {
	if len(t) <= 4 {
		return mask
	}
	return t[:2] + "..." + t[len(t)-2:]
}
