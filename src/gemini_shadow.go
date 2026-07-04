package main

import (
	"sync"
	"time"
)

// ============================================================================
// Gemini thoughtSignature 影子存储
// ============================================================================
//
// 背景：Anthropic 的 thinking 块带 `signature`，Gemini 的 functionCall 带
// `thoughtSignature`。两者都是上游用来校验 thinking 完整性的不透明令牌，
// 在多轮工具调用中必须原样回传给生成方，否则上游会以 400 拒绝。
//
// 问题：Anthropic tool_use 块没有承载 Gemini thoughtSignature 的字段。
// 若直接丢弃，下一轮把 Anthropic 历史转回 Gemini 时 functionCall 就缺
// thoughtSignature，Gemini 校验失败。
//
// 方案：仿照 cc-switch 的 shadow store，在 Gemini→Anthropic 响应转换时
// 抓取每个 functionCall 的 thoughtSignature，以 tool_call_id 为键存入
// 全局表；后续 Anthropic→Gemini 请求转换时按 tool_use.id 查回并附到
// functionCall 上。条目带 TTL 防止内存无限增长。
//
// 局限：网关无 session 概念，这里用 tool_call_id 全局唯一作键。Gemini
// 合成的 id 形如 "toolu_xxx" 或 uuid，碰撞概率极低。多副本部署时各副本
// 各自维护一份存储，不跨实例共享——同一会话需固定路由到同一网关实例。

const geminiShadowTTL = 1 * time.Hour

type geminiShadowEntry struct {
	signature string
	expireAt  time.Time
}

var (
	geminiShadowMu    sync.RWMutex
	geminiShadowStore = make(map[string]geminiShadowEntry)
)

// storeGeminiThoughtSignature 记录某个 tool_call_id 对应的 thoughtSignature。
// 空值不存储。覆盖旧值并刷新过期时间。
func storeGeminiThoughtSignature(toolCallID, signature string) {
	if toolCallID == "" || signature == "" {
		return
	}
	geminiShadowMu.Lock()
	defer geminiShadowMu.Unlock()
	geminiShadowStore[toolCallID] = geminiShadowEntry{
		signature: signature,
		expireAt:  time.Now().Add(geminiShadowTTL),
	}
}

// lookupGeminiThoughtSignature 查询 tool_call_id 对应的 thoughtSignature。
// 不存在或已过期返回空串。过期条目惰性清理。
func lookupGeminiThoughtSignature(toolCallID string) string {
	if toolCallID == "" {
		return ""
	}
	geminiShadowMu.RLock()
	entry, ok := geminiShadowStore[toolCallID]
	geminiShadowMu.RUnlock()
	if !ok {
		return ""
	}
	if time.Now().After(entry.expireAt) {
		geminiShadowMu.Lock()
		delete(geminiShadowStore, toolCallID)
		geminiShadowMu.Unlock()
		return ""
	}
	return entry.signature
}

// cleanupExpiredGeminiShadows 清理所有过期条目。由调度器周期调用。
func cleanupExpiredGeminiShadows() {
	now := time.Now()
	geminiShadowMu.Lock()
	defer geminiShadowMu.Unlock()
	for k, v := range geminiShadowStore {
		if now.After(v.expireAt) {
			delete(geminiShadowStore, k)
		}
	}
}

// extractGeminiThoughtSignature 从 Gemini functionCall part 提取 thoughtSignature。
// 兼容 camelCase（thoughtSignature）与 snake_case（thought_signature）。
func extractGeminiThoughtSignature(functionCall map[string]interface{}) string {
	if functionCall == nil {
		return ""
	}
	if sig, ok := asString(functionCall["thoughtSignature"]); ok && sig != "" {
		return sig
	}
	if sig, ok := asString(functionCall["thought_signature"]); ok && sig != "" {
		return sig
	}
	return ""
}
