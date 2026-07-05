package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// geminiModelRegex 从 Gemini 风格 URL path 中提取模型名，例如
// /v1beta/models/gemini-2.0-flash:generateContent -> gemini-2.0-flash
var geminiModelRegex = regexp.MustCompile(`/models/([^:/]+)`)

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
// 网关：upstream队列轮转（带锁读取）
// ============================================================================

// pickFirstAvailableUpstream 在一次写锁内原子完成：扫描队列找首个可用 upstream，
// 将其前方所有不可用 upstream 轮转到队尾，并返回该可用 upstream 名与其配置快照。
//
// 不可用定义：stateMap[upstream]==nil（含未初始化 upstream -> 保守视为耗尽）或 Exhausted==true，
// 或 tokenMap.Upstreams 中缺失该 upstream（配置缺失 -> 保守视为不可用）。
//
// 返回 (upstreamName, cfg, rotated)：
//   - upstreamName == "" 表示整队列不可用；rotated 为本次被跳过的 upstream 名列表（用于日志）。
//   - upstreamName != "" 时 cfg 为该 upstream 配置快照（值拷贝），调用方无需再次单独加锁读取配置。
//
// 用途：消除原 pickFirst -> isExhausted -> rotateToEnd 三次独立加锁间的
// TOCTOU 竞态——高并发下另一 goroutine 可能在两次加锁间改变队列或 state，
// 导致选中的 upstream 已被耗尽或配置已被移除。本函数把"选 + 把耗尽前置移到队尾"合为一次锁内操作。
func pickFirstAvailableUpstream(fakeToken string) (upstreamName string, cfg *UpstreamConfig, rotated []string) {
	mu.Lock()
	defer mu.Unlock()
	q := tokenMap.FakeTokens[fakeToken]
	if len(q) == 0 {
		return "", nil, nil
	}
	for i, a := range q {
		st := stateMap[a]
		if st == nil || st.Exhausted {
			rotated = append(rotated, a)
			continue
		}
		ac, ok := tokenMap.Upstreams[a]
		if !ok {
			// 配置缺失：保守视为不可用，轮到队尾等同耗尽
			rotated = append(rotated, a)
			continue
		}
		// 找到首个可用 upstream；其后的 upstream 保持原序，前方已耗尽的前缀整体移到队尾
		remaining := q[i+1:]
		newQ := make([]string, 0, len(q))
		newQ = append(newQ, a)
		newQ = append(newQ, remaining...)
		newQ = append(newQ, rotated...)
		tokenMap.FakeTokens[fakeToken] = newQ
		snap := ac // 值拷贝；UpstreamConfig 内 Extra/Availability 为引用，只读路径下安全
		return a, &snap, rotated
	}
	// 整队列不可用：直接返回 "" 让调用方回 503，
	// 避免原实现 maxAttempts 次循环逐个旋转的无谓空转（语义一致：仍返回 503）。
	// rotated 已含整个队列供日志使用。
	return "", nil, rotated
}

// rotateUpstreamToEnd 将指定upstream移到其fakeToken队列末端
func rotateUpstreamToEnd(fakeToken, upstreamName string) {
	mu.Lock()
	defer mu.Unlock()
	q := tokenMap.FakeTokens[fakeToken]
	idx := -1
	for i, a := range q {
		if a == upstreamName {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	q = append(q[:idx], q[idx+1:]...)
	q = append(q, upstreamName)
	tokenMap.FakeTokens[fakeToken] = q
}

// getUpstreamQueueLen 返回 fakeToken 对应队列长度（加锁读取，避免与 rotate 竞争）
func getUpstreamQueueLen(fakeToken string) int {
	mu.RLock()
	defer mu.RUnlock()
	return len(tokenMap.FakeTokens[fakeToken])
}

// hasUpstreamQueue 队列是否存在且非空（加锁读取）
func hasUpstreamQueue(fakeToken string) bool {
	return getUpstreamQueueLen(fakeToken) > 0
}

// incrementCount count型upstream请求计数+1，返回新的count；同时若达到limit则标记exhaust
// Limit 为 0 时永不 exhaust（无限制计数）
// 返回 (newCount, nowExhausted)
func incrementCount(upstreamName string) (int, bool) {
	mu.Lock()
	defer mu.Unlock()
	st := stateMap[upstreamName]
	if st == nil {
		st = initStateFor(upstreamName)
		stateMap[upstreamName] = st
	}
	cfg := tokenMap.Upstreams[upstreamName].Availability
	if cfg == nil || cfg.Type != availCount {
		return 0, false
	}
	st.Count++
	markDirty()
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
func applyAvailabilityResult(upstreamName string, res AvailabilityResult) bool {
	mu.Lock()
	defer mu.Unlock()
	st := stateMap[upstreamName]
	if st == nil {
		st = initStateFor(upstreamName)
		stateMap[upstreamName] = st
	}
	st.Exhausted = res.Exhausted
	st.LastChecked = time.Now()

	cfg := tokenMap.Upstreams[upstreamName].Availability
	isCount := cfg != nil && cfg.Type == availCount
	if isCount {
		// count 型：恢复依据是 cron（= RefreshCron），不使用 RecoveryAt
		if res.RecoveryCron != "" {
			st.RecoveryCron = res.RecoveryCron
		}
		st.RecoveryAt = time.Time{}
	} else {
		// usage/balance/exhaust型：恢复依据是精确时间点 RecoveryAt
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
	markDirty()
	return res.Exhausted
}

// ============================================================================
// 网关：请求转发 + upstream队列轮转
// ============================================================================

// handler 主请求处理：fakeToken -> upstream队列轮转
func handler(w http.ResponseWriter, r *http.Request) {
	// 全局并发上限：acquire 一个令牌，defer 保证释放（含 panic 路径）。
	// 通道满时阻塞排队，而非无限开新 goroutine，防止高并发下资源爆炸。
	reqSem <- struct{}{}
	defer func() { <-reqSem }()

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
	if !hasUpstreamQueue(fakeToken) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Invalid token"})
		return
	}
	maxAttempts := getUpstreamQueueLen(fakeToken)
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
	// 解析请求体提取 stream 标志与 model 字段；body 非 JSON 则静默忽略。
	// 无条件解析（不再依赖 !isStream）：仅靠 Accept 头判为 SSE 的请求同样
	// 需要从 body 读取 stream/model，顺带补齐原逻辑缺口。
	var modelStr string
	if hasBody {
		var bodyMap map[string]interface{}
		if json.Unmarshal(bodyBytes, &bodyMap) == nil {
			if s, ok := bodyMap["stream"].(bool); ok && s {
				isStream = true
			}
			if m, ok := bodyMap["model"].(string); ok {
				modelStr = m
			}
		}
	}
	// body 无 model（如 Gemini 风格）→ 回退从 URL path 提取模型名
	if modelStr == "" {
		if m := geminiModelRegex.FindStringSubmatch(r.URL.Path); m != nil {
			modelStr = m[1]
		}
	}
	if modelStr != "" {
		log.Printf("  model: %s", modelStr)
	}

	// 模型别名替换已迁移至 attempt 循环内：aliases 现为 per-upstream 配置（UpstreamConfig.Aliases），
	// 不同 upstream 可能有不同别名映射，故不能在循环外对共享的 bodyBytes/r.URL.Path/modelStr 做一次性重写。
	// 此处保留原始 client 输入原值，由每次 attempt 内按选中 upstream 的 aliases 各自计算 sendBody/sendModel/targetPath。

	// 判断 token 注入方式（与原实现一致：query key / goog header / x-api-key / bearer）
	useGoogHeader := googHeader != ""
	useQueryKey := queryKey != ""
	useAPIKeyHeader := apiKeyHeader != ""

	// 选择共享client（流式无整体超时，由 streamIdleTimeout 控制）
	client := proxyClient
	if isStream {
		client = streamClient
	}

	// upstream 队列轮转：最多尝试 maxAttempts 次（每次 attempt 对应一次真实上游请求 + 可能的后续重试）。
	// 原子挑选由 pickFirstAvailableUpstream 一次加锁完成：跳过所有已 exhausted 的前置 upstream（移到队尾），
	// 返回首个可用 upstream 及其配置快照，消除原三次加锁间的 TOCTOU 竞态。
	for attempt := 0; attempt < maxAttempts; attempt++ {
		upstreamName, upstreamCfg, rotated := pickFirstAvailableUpstream(fakeToken)
		if len(rotated) > 0 {
			log.Printf("[ROTATE] fakeToken=%s skipped exhausted upstreams=%v (attempt=%d)",
				maskFakeToken(fakeToken), rotated, attempt+1)
		}
		if upstreamName == "" {
			// 整队列不可用（含配置缺失兜底）——提前返回 503，避免空转 maxAttempts 次
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "No available upstream"})
			return
		}
		// upstreamCfg 由 pickFirstAvailableUpstream 一次性快照返回，无需再次单独加锁读 getUpstreamConfig

		// 构造目标请求
		query := r.URL.Query()
		query.Del("key")

		outHeaders := r.Header.Clone()
		removeHopHeaders(outHeaders)

		// 格式转换上下文：outFormat 来自 upstream 配置，inFormat 按 URL path 检测。
		// doTransform=false 时（未配置 / 同格式 / openai 两变体互转 / unknown）走原透传逻辑。
		outFormat := mapFormatTransform(upstreamCfg.FormatTransform)
		if upstreamCfg.FormatTransform != "" && outFormat == "" {
			log.Printf("[TRANSFORM] upstream=%s invalid formatTransform=%q -> passthrough",
				upstreamName, upstreamCfg.FormatTransform)
		}
		inFormat := detectInputFormat(r.URL.Path)
		// 列表请求检测：/v1/models（按 auth 头区分 openai/anthropic）或 /v1beta/models（gemini）。
		// 列表请求 inFormat == formatUnknown（detectInputFormat 不识别列表端点），需用 detectListFormat 旁路。
		listFormat := detectListFormat(r.URL.Path, r.Header)
		isListRequest := listFormat != formatUnknown

		// per-upstream 别名替换：仅在 upstreamCfg.Aliases 配置启用且提取到模型名时执行。
		// 列表请求 modelStr=="" 天然跳过。
		// 同时处理 body 的 model 字段（OpenAI/Anthropic 风格）与 URL path 中的模型名（Gemini 风格），
		// 二者独立检查，仅当源值与提取到的 modelStr 一致时才重写，避免误改无关字段。
		// 仅作用于本次 attempt 的局部 sendBody/sendModel/basePath，
		// 不污染跨 attempt 复用的原始 bodyBytes/r.URL.Path/modelStr（不同 upstream 别名不同）。
		basePath := r.URL.Path
		sendBody := bodyBytes
		sendModel := modelStr
		if upstreamCfg.Aliases != nil && modelStr != "" {
			if real, ok := upstreamCfg.Aliases[modelStr]; ok && real != "" && real != modelStr {
				log.Printf("[ALIAS] upstream=%s %s -> %s", upstreamName, modelStr, real)
				sendModel = real
				if hasBody {
					var bm map[string]interface{}
					if json.Unmarshal(bodyBytes, &bm) == nil {
						if cur, ok := bm["model"].(string); ok && cur == modelStr {
							bm["model"] = real
							if nb, merr := json.Marshal(bm); merr == nil {
								sendBody = nb
							}
						}
					}
				}
				if m := geminiModelRegex.FindStringSubmatch(r.URL.Path); m != nil && m[1] == modelStr {
					basePath = geminiModelRegex.ReplaceAllString(r.URL.Path, "/models/"+real)
				}
			}
		}

		// 列表请求需以 listFormat 覆盖 inFormat（用于错误响应转换），并选择列表转换分支
		// （不走业务端点 TransformRequest/Response）。非列表请求走原有 doTransform 逻辑。
		doTransform := false
		doListTransform := false
		if isListRequest {
			inFormat = listFormat
			doListTransform = outFormat != "" && needsTransform(listFormat, outFormat)
			if doListTransform {
				log.Printf("[TRANSFORM-LIST] upstream=%s %s -> %s", upstreamName, listFormat, outFormat)
			}
		} else {
			doTransform = outFormat != "" && needsTransform(inFormat, outFormat)
			if doTransform {
				log.Printf("[TRANSFORM] upstream=%s %s -> %s", upstreamName, inFormat, outFormat)
			}
		}

		// Pre-transform passes：对 Anthropic 客户端请求体做供应商兼容处理。
		// 这些 pass 作用于客户端请求体（转换前，已应用 alias 后的 sendBody），需要本地副本避免污染 bodyBytes。
		transformInput := sendBody
		if hasBody && inFormat == formatAnthropic {
			preBody := sendBody
			// reasoningVendor：将 thinking 块重写为占位符（Kimi/DeepSeek/MiMo 等拒绝原始 thinking）
			if rv := upstreamCfg.Extra["reasoningVendor"]; rv != "" {
				shouldNormalize := false
				if rv == "auto" {
					shouldNormalize = isReasoningVendorIdentifier(upstreamName) || isReasoningVendorIdentifier(upstreamCfg.TargetBase)
				} else {
					shouldNormalize = true
				}
				if shouldNormalize {
					if b, err := normalizeThinkingHistoryForVendorInBytes(preBody); err == nil {
						preBody = b
					} else {
						log.Printf("[TRANSFORM] upstream=%s normalize thinking history failed: %v", upstreamName, err)
					}
				}
			}
			// stripEffortWhenThinkingDisabled：thinking 禁用时剥离 effort 参数（DeepSeek 兼容）
			if upstreamCfg.Extra["stripEffortWhenThinkingDisabled"] == "true" {
				if b, err := stripEffortIfThinkingDisabledInBytes(preBody); err == nil {
					preBody = b
				} else {
					log.Printf("[TRANSFORM] upstream=%s strip effort failed: %v", upstreamName, err)
				}
			}
			transformInput = preBody
			if !doTransform {
				sendBody = preBody
			}
		}

		if doTransform && hasBody {
			opts := TransformOptions{
				PreserveReasoningContent: upstreamCfg.Extra["preserveReasoningContent"] == "true",
			}
			// 用 sendModel（post-alias）作为转换输入，确保发往上游的 body/path 用真实模型名
			newBody, newModel, terr := TransformRequestWithOptions(inFormat, outFormat, transformInput, sendModel, isStream, opts)
			if terr != nil {
				// 请求转换失败不直接 400 中断：队列中后续 upstream 可能是透传
				// （formatTransform 为空，doTransform=false）或不同 outFormat，
				// 本可成功处理同一请求体。改为 continue 让后续 upstream 继续尝试，
				// 全部失败后由循环外兜底 503。此处尚未创建 reqCtx/HTTP 请求，无需 cancel。
				log.Printf("[TRANSFORM] request convert failed (will try next upstream): %v", terr)
				continue
			}
			sendBody = newBody
			if newModel != "" {
				sendModel = newModel
			}
		}

		// Codex OAuth 后端整形：对发往 ChatGPT Codex 后端的 Responses 请求做字段整形。
		// 触发条件：extra["codexBackend"] 配置为 "true"（普通）或 "fast"（priority 模式），
		// 且发送给上游的 body 为 Responses 格式（转换到 openai_responses，或透传 responses 客户端）。
		if hasBody {
			isResponsesOut := outFormat == formatOpenAIResponses || (outFormat == "" && inFormat == formatOpenAIResponses)
			if isResponsesOut {
				if codex := upstreamCfg.Extra["codexBackend"]; codex == "true" || codex == "fast" {
					if shaped, err := shapeForCodexBackendInBytes(sendBody, codex == "fast"); err == nil {
						sendBody = shaped
					} else {
						log.Printf("[TRANSFORM] upstream=%s codex shape failed: %v", upstreamName, err)
					}
				}
			}
		}

		// Anthropic Prompt Caching 自动注入 cache_control 断点。
		// 触发条件：upstream 显式配置 cacheInjection.Enabled 且发送给上游的 body 为 Anthropic 格式
		// （转换到 anthropic，或透传 anthropic 客户端请求到 anthropic 上游）。
		if hasBody && upstreamCfg.CacheInjection != nil && upstreamCfg.CacheInjection.Enabled {
			isAnthropicOut := outFormat == formatAnthropic || (outFormat == "" && inFormat == formatAnthropic)
			if isAnthropicOut {
				if injected, err := injectCacheControlIntoBytes(sendBody, upstreamCfg.CacheInjection); err == nil {
					sendBody = injected
				} else {
					log.Printf("[CACHE-INJECT] upstream=%s inject failed: %v", upstreamName, err)
				}
			}
		}

		// Token 注入：转换路径用 swapAuthForTarget 重置 auth 头；透传路径沿用原逻辑
		// （仅当对应输入存在时注入对应输出字段，两者都存在则同时注入，否则回退 X-Api-Key / Authorization）。
		// 列表转换（doListTransform）与业务端点转换共享同一 auth 头处理。
		if doTransform || doListTransform {
			swapAuthForTarget(outHeaders, query, upstreamCfg.RealToken, outFormat)
			if outFormat == formatGemini && isStream {
				query.Set("alt", "sse")
			}
			if outFormat != formatGemini {
				query.Del("alt")
			}
		} else if useGoogHeader || useQueryKey {
			if useGoogHeader {
				outHeaders.Set("X-Goog-Api-Key", upstreamCfg.RealToken)
			}
			if useQueryKey {
				query.Set("key", upstreamCfg.RealToken)
			}
		} else if useAPIKeyHeader {
			outHeaders.Set("X-Api-Key", upstreamCfg.RealToken)
		} else {
			outHeaders.Set("Authorization", "Bearer "+upstreamCfg.RealToken)
		}

		// 目标 path：列表转换走 targetListEndpointPath；业务端点转换走 targetEndpointPath；
		// 透传路径用 basePath（已应用 per-upstream alias 的 Gemini 风格 path 重写）。
		targetPath := basePath
		if doListTransform {
			if p := targetListEndpointPath(outFormat); p != "" {
				targetPath = p
			}
		} else if doTransform {
			if p := targetEndpointPath(outFormat, sendModel, isStream); p != "" {
				targetPath = p
			}
		}
		targetURL := upstreamCfg.TargetBase + targetPath
		if encoded := query.Encode(); encoded != "" {
			targetURL += "?" + encoded
		}

		var bodyReader io.Reader
		if hasBody {
			bodyReader = bytes.NewReader(sendBody)
		}

		// thinking signature 自动修复：仅允许同一 upstream 内重试 1 次
		rectified := false

		// Issue 1: 移除循环内 defer cancelReq()，改在每个退出分支显式调用
		reqCtx, cancelReq := context.WithCancel(r.Context())
		// 流式请求加 maxStreamLife 生命周期硬上限：即便空闲监控 goroutine 在边界情况未触发，
		// 到期也会取消上游连接，避免 goroutine 无限堆积。
		// lifeCtx 派生自 reqCtx，过期会触发 reqCtx(=lifeCtx).Done() → 中断 resp.Body.Read
		// 同时通知 idle 监控 goroutine 退出。
		if isStream {
			lifeCtx, lifeCancel := context.WithTimeout(reqCtx, maxStreamLife)
			origCancel := cancelReq
			reqCtx = lifeCtx
			cancelReq = func() { lifeCancel(); origCancel() }
		}
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

		log.Printf("[RES] %s %s -> %d (%dms) upstream=%s",
			r.Method, maskURL(r.URL.String()), resp.StatusCode, time.Since(start).Milliseconds(), upstreamName)

		// 可用性错误 → 触发可用性检查（singleflight 去重：同一 upstream 并发触发只执行一次真实 provider 调用）
		if isAvailabilityError(resp.StatusCode) {
			// 先把错误响应体读出来（错误响应通常很小）
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			log.Printf("[TRANSFORM] upstream=%s returned error %d: %s", upstreamName, resp.StatusCode, string(errBody))

			// 调用可用性检查（count型走自身判断；其它调用provider），用 singleflight 合并同 upstream 并发调用
			res := availSF.Do(upstreamName, func() AvailabilityResult {
				var stCopy *AvailabilityState
				mu.RLock()
				stCopy = stateMap[upstreamName]
				mu.RUnlock()
				return checkAvailability(upstreamName, upstreamCfg.Availability, stCopy)
			})
			exhausted := applyAvailabilityResult(upstreamName, res)

			if exhausted {
				log.Printf("[AVAIL] upstream=%s exhausted (status=%d) -> rotate", upstreamName, resp.StatusCode)
				rotateUpstreamToEnd(fakeToken, upstreamName)
				cancelReq() // 释放本次迭代context（continue前）
				// 继续尝试下一个upstream
				continue
			}
			// 非exhaust但返回可用性错误 → 把错误响应返回给客户端。
			// 转换路径下先经 TransformErrorResponse 转为客户端格式，保持错误体格式一致。
			// 列表请求（doListTransform）同样按客户端列表格式重建错误响应。
			respBody := errBody
			if doTransform || doListTransform {
				respBody = TransformErrorResponse(inFormat, outFormat, errBody, resp.StatusCode)
			}
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
			if _, werr := w.Write(respBody); werr != nil {
				log.Printf("[resp] write error response failed: %v", werr)
			}
			cancelReq()
			return
		}

		// thinking signature 自动修复：检测 Anthropic 上游 400 thinking signature 错误，
		// 剥离请求中的 thinking 块后重试同一 upstream 一次。仅当发送给上游的 body 为
		// Anthropic 格式时触发（转换到 anthropic 或透传 anthropic 客户端请求）。
		if resp.StatusCode == 400 && hasBody && !rectified {
			isAnthropicOut := outFormat == formatAnthropic || (outFormat == "" && inFormat == formatAnthropic)
			if isAnthropicOut {
				errBody, _ := io.ReadAll(resp.Body)
				resp.Body.Close()

				if shouldRectifyThinkingSignature(errBody) {
					log.Printf("[THINKING-RECTIFY] upstream=%s 400 thinking signature error, retrying with thinking blocks stripped", upstreamName)
					newBody, rerr := rectifyAnthropicRequestBytes(sendBody)
					if rerr == nil {
						sendBody = newBody
						rectified = true
						// 释放旧 context 并重建请求
						cancelReq()
						reqCtx, cancelReq = context.WithCancel(r.Context())
						if isStream {
							lifeCtx, lifeCancel := context.WithTimeout(reqCtx, maxStreamLife)
							origCancel := cancelReq
							reqCtx = lifeCtx
							cancelReq = func() { lifeCancel(); origCancel() }
						}
						bodyReader = bytes.NewReader(sendBody)
						retryReq, rerr2 := http.NewRequestWithContext(reqCtx, r.Method, targetURL, bodyReader)
						if rerr2 != nil {
							cancelReq()
							log.Printf("[ERR] %s %s -> %v (rectify retry)", r.Method, maskURL(targetURL), rerr2)
							writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Proxy error"})
							return
						}
						retryReq.Header = outHeaders
						resp, err = client.Do(retryReq)
						if err != nil {
							cancelReq()
							log.Printf("[ERR] %s %s -> %v (rectify retry)", r.Method, maskURL(targetURL), err)
							writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Proxy error"})
							return
						}
						log.Printf("[RES] %s %s -> %d (%dms) upstream=%s (rectify retry)",
							r.Method, maskURL(r.URL.String()), resp.StatusCode, time.Since(start).Milliseconds(), upstreamName)
					} else {
						// rectify 失败：还原 body 供后续错误处理
						resp.Body = io.NopCloser(bytes.NewReader(errBody))
					}
				} else {
					// 非 thinking signature 错误：还原 body 供后续错误处理
					resp.Body = io.NopCloser(bytes.NewReader(errBody))
				}
			}
		}

		// count型：仅成功（2xx）且携带 model 名（计费请求）才计数。
		// 可用性错误 4xx(401/402/403/429) 已在上分支 continue/return，传输错误已提前 return，
		// 故此处只需额外门禁 modelStr != "" 与状态码 < 300。
		// 无 model 名（如 /v1/models 列模型、无 body 的查询请求）视为非计费请求，跳过计数。
		if upstreamCfg.Availability != nil && upstreamCfg.Availability.Type == availCount &&
			modelStr != "" && resp.StatusCode < 300 {
			_, nowExhausted := incrementCount(upstreamName)
			if nowExhausted {
				log.Printf("[COUNT] upstream=%s reached limit -> exhaust+rotate", upstreamName)
				rotateUpstreamToEnd(fakeToken, upstreamName)
			} else {
				log.Printf("[COUNT] upstream=%s incremented count (status=%d)", upstreamName, resp.StatusCode)
			}
		}

		// 响应转换：整读 → TransformResponse/TransformErrorResponse → 写回。
		// 覆盖非流式响应，以及流式请求的错误响应（错误响应通常为 JSON 而非 SSE，
		// 不能用 SSE 流式转换器处理）。可用性错误（401/402/403/429）已在上方单独处理。

		// 列表请求分支：模型列表响应永远非流式，独立于业务端点转换分支处理。
		// 成功响应走 TransformModelsListResponse（含 alias 反向展开），错误响应走 TransformErrorResponse。
		// 可用性错误（401/402/403/429）已在上分支单独处理（exhaust 路径 continue，非 exhaust 已 return）。
		if doListTransform {
			rawBody, rerr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if rerr != nil {
				log.Printf("[resp] read upstream list response failed: %v", rerr)
				writeJSON(w, http.StatusBadGateway, map[string]string{"error": "Bad gateway"})
				cancelReq()
				return
			}
			var tBody []byte
			if resp.StatusCode < 300 {
				converted, terr := TransformModelsListResponse(listFormat, outFormat, rawBody, upstreamCfg.Aliases)
				if terr != nil {
					log.Printf("[TRANSFORM-LIST] response convert failed: %v (fallback raw)", terr)
					tBody = rawBody
				} else {
					tBody = converted
				}
			} else {
				log.Printf("[TRANSFORM-LIST] upstream=%s returned error %d: %s",
					upstreamName, resp.StatusCode, string(rawBody))
				tBody = TransformErrorResponse(listFormat, outFormat, rawBody, resp.StatusCode)
			}
			for k, vs := range resp.Header {
				k = http.CanonicalHeaderKey(k)
				if k == "Content-Length" || k == "Transfer-Encoding" || k == "Connection" {
					continue
				}
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			// 转换后 body 长度变化，需重设 Content-Length（无下游透传会沿用上游长度导致截断/超长）
			w.Header().Del("Content-Length")
			w.WriteHeader(resp.StatusCode)
			if _, werr := w.Write(tBody); werr != nil {
				log.Printf("[resp] write transformed list response failed: %v", werr)
			}
			cancelReq()
			return
		}

		if doTransform && (!isStream || resp.StatusCode >= 300) {
			rawBody, rerr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if rerr != nil {
				log.Printf("[resp] read upstream response failed: %v", rerr)
				writeJSON(w, http.StatusBadGateway, map[string]string{"error": "Bad gateway"})
				cancelReq()
				return
			}
			var tBody []byte
			if resp.StatusCode < 300 {
				converted, terr := TransformResponse(inFormat, outFormat, rawBody)
				if terr != nil {
					log.Printf("[TRANSFORM] response convert failed: %v (fallback raw)", terr)
					tBody = rawBody
				} else {
					tBody = converted
				}
			} else {
				log.Printf("[TRANSFORM] upstream=%s returned error %d: %s", upstreamName, resp.StatusCode, string(rawBody))
				// 解析请求体提取诊断摘要
				var reqParsed map[string]interface{}
				if err := json.Unmarshal(sendBody, &reqParsed); err == nil {
					msgs := getArray(reqParsed, "messages")
					msgSummary := fmt.Sprintf("total=%d", len(msgs))
					// 取最后 3 条的 role 摘要
					start := len(msgs) - 3
					if start < 0 {
						start = 0
					}
					var roles []string
					for i := start; i < len(msgs); i++ {
						if m, ok := asMap(msgs[i]); ok {
							r := getString(m, "role")
							if r == "assistant" && getArray(m, "tool_calls") != nil {
								r += "(tools)"
							}
							if r == "tool" {
								r += "(" + getString(m, "tool_call_id") + ")"
							}
							roles = append(roles, r)
						}
					}
					msgSummary += fmt.Sprintf(", last=%v", roles)
					log.Printf("[TRANSFORM] upstream=%s request model=%s messages=%s",
						upstreamName, getString(reqParsed, "model"), msgSummary)
				} else {
					log.Printf("[TRANSFORM] upstream=%s sent request body (truncated): %s",
						upstreamName, string(sendBody))
				}
				tBody = TransformErrorResponse(inFormat, outFormat, rawBody, resp.StatusCode)
			}
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
			if _, werr := w.Write(tBody); werr != nil {
				log.Printf("[resp] write transformed response failed: %v", werr)
			}
			cancelReq()
			return
		}

		// 成功或其它状态 → 转发响应（不重试）
		if isStream {
			if doTransform {
				resp.Body = newTransformStreamReader(inFormat, outFormat, resp.Body)
			}
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
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "All upstreams exhausted"})
}

// maskFakeToken 简单脱敏fakeToken用于日志
func maskFakeToken(t string) string {
	if len(t) <= 4 {
		return mask
	}
	return t[:2] + "..." + t[len(t)-2:]
}

// ============================================================================
// /status 端点：暴露运行时状态（脱敏），供运维排查
// ============================================================================

// statusUpstream 单个 upstream 的运行时视图（给 /status 用）。
// realToken 脱敏为首尾各 4 字符，targetBase 完整暴露便于排查路由。
type statusUpstream struct {
	Name            string      `json:"name"`
	TargetBase      string      `json:"targetBase"`
	RealToken       string      `json:"realToken"`
	AvailType       string      `json:"availType,omitempty"`
	Exhausted       bool        `json:"exhausted"`
	Count           int         `json:"count,omitempty"`
	Balance         float64     `json:"balance,omitempty"`
	Tiers           []TierState `json:"tiers,omitempty"`
	RecoveryCron    string      `json:"recoveryCron,omitempty"`
	RecoveryAt      time.Time   `json:"recoveryAt,omitempty"`
	LastRecovery    time.Time   `json:"lastRecovery,omitempty"`
	LastChecked     time.Time   `json:"lastChecked,omitempty"`
	QueueFor        []string    `json:"queueFor,omitempty"` // 出现在哪些 fakeToken 队列中（脱敏）
	FormatTransform string      `json:"formatTransform,omitempty"`
}

// statusResponse /status 的响应体。
type statusResponse struct {
	Upstreams  []statusUpstream    `json:"upstreams"`
	FakeTokens map[string][]string `json:"fakeTokens"` // fakeToken(脱敏) -> upstream 名队列
}

// handlerStatus 构建内存状态的脱敏快照并通过 /status 返回。
func statusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
		return
	}

	mu.RLock()

	// 收集每个 upstream 出现在哪些 fakeToken 队列中
	queueFor := make(map[string][]string)
	for ft, q := range tokenMap.FakeTokens {
		ftMasked := maskFakeToken(ft)
		for _, a := range q {
			queueFor[a] = append(queueFor[a], ftMasked)
		}
	}

	resp := statusResponse{
		Upstreams:  make([]statusUpstream, 0, len(tokenMap.Upstreams)),
		FakeTokens: make(map[string][]string, len(tokenMap.FakeTokens)),
	}

	for name, upstream := range tokenMap.Upstreams {
		st := stateMap[name]
		sa := statusUpstream{
			Name:            name,
			TargetBase:      upstream.TargetBase,
			RealToken:       maskToken(upstream.RealToken),
			FormatTransform: upstream.FormatTransform,
		}
		if upstream.Availability != nil {
			sa.AvailType = upstream.Availability.Type
		}
		if st != nil {
			sa.Exhausted = st.Exhausted
			sa.Count = st.Count
			sa.Balance = st.Balance
			if len(st.Tiers) > 0 {
				sa.Tiers = make([]TierState, len(st.Tiers))
				copy(sa.Tiers, st.Tiers)
			}
			sa.RecoveryCron = st.RecoveryCron
			sa.RecoveryAt = st.RecoveryAt
			sa.LastRecovery = st.LastRecovery
			sa.LastChecked = st.LastChecked
		}
		if qf, ok := queueFor[name]; ok {
			sa.QueueFor = qf
		}
		resp.Upstreams = append(resp.Upstreams, sa)
	}

	for ft, q := range tokenMap.FakeTokens {
		cq := make([]string, len(q))
		copy(cq, q)
		resp.FakeTokens[maskFakeToken(ft)] = cq
	}

	mu.RUnlock()

	writeJSON(w, http.StatusOK, resp)
}

// maskToken 脱敏 realToken：保留首尾各 4 字符，中间用 mask 替代。
func maskToken(t string) string {
	if len(t) <= 8 {
		return t[:2] + mask
	}
	return t[:4] + mask + t[len(t)-4:]
}
