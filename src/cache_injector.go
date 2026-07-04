package main

import "encoding/json"

// ============================================================================
// Anthropic Prompt Caching 自动 cache_control 断点注入
// ============================================================================
//
// Anthropic 的 Prompt Caching 需要在请求体的 tools / system / messages 上
// 显式标记 cache_control 断点，否则每个请求全量计费。客户端不设置时，
// 由网关依据 upstream 配置自动注入最多 4 个断点（Anthropic 上限）：
//
//   1. tools 数组最后一个 tool
//   2. system 末尾（字符串 system 转数组后挂载）
//   3. 最后一条 assistant 消息的最后一个非 thinking 块
//   4. 最后一条 user 消息末尾
//
// 已存在的 cache_control 不覆盖，仅注入缺失的。多副本/透传场景下若客户端
// 已显式设置断点则保持客户端意图。

// injectCacheControlIntoBytes 解析 Anthropic 请求体 JSON，注入 cache_control
// 断点后重新序列化。失败时返回原 body 与错误，调用方可选择回退原 body。
func injectCacheControlIntoBytes(body []byte, cfg *CacheInjectorConfig) ([]byte, error) {
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body, err
	}
	injectCacheControl(parsed, cfg)
	return json.Marshal(parsed)
}

// injectCacheControl 在 Anthropic 请求体的关键位置注入 cache_control 断点。
// 最多注入 4 个断点（Anthropic 上限）。cfg.Enabled=false 时直接返回。
func injectCacheControl(body map[string]interface{}, cfg *CacheInjectorConfig) {
	if cfg == nil || !cfg.Enabled {
		return
	}

	var cacheControl interface{}
	if cfg.TTL == "" || cfg.TTL == "5m" {
		cacheControl = map[string]interface{}{"type": "ephemeral"}
	} else {
		cacheControl = map[string]interface{}{"type": "ephemeral", "ttl": cfg.TTL}
	}

	remaining := 4

	// 1. tools 末尾
	if remaining > 0 {
		if tools, ok := body["tools"].([]interface{}); ok && len(tools) > 0 {
			if lastTool, ok := tools[len(tools)-1].(map[string]interface{}); ok {
				if _, exists := lastTool["cache_control"]; !exists {
					lastTool["cache_control"] = cacheControl
					remaining--
				}
			}
		}
	}

	// 2. system 末尾
	if remaining > 0 {
		injectCacheControlToSystem(body, cacheControl, &remaining)
	}

	// 3 & 4. 消息中的最后 assistant / user
	if remaining > 0 {
		injectCacheControlToMessages(body, cacheControl, &remaining)
	}
}

// injectCacheControlToSystem 在 system 字段末尾注入 cache_control。
// 字符串 system 转为单元素数组以承载 cache_control。
func injectCacheControlToSystem(body map[string]interface{}, cc interface{}, remaining *int) {
	sys, ok := body["system"]
	if !ok || sys == nil {
		return
	}
	switch s := sys.(type) {
	case string:
		if s == "" {
			return
		}
		body["system"] = []interface{}{
			map[string]interface{}{"type": "text", "text": s, "cache_control": cc},
		}
		*remaining--
	case []interface{}:
		if len(s) > 0 {
			if last, ok := s[len(s)-1].(map[string]interface{}); ok {
				if _, exists := last["cache_control"]; !exists {
					last["cache_control"] = cc
					*remaining--
				}
			}
		}
	}
}

// injectCacheControlToMessages 从后往前扫描 messages，分别给最后一条
// assistant 和最后一条 user 消息的最后一个非 thinking 块注入 cache_control。
func injectCacheControlToMessages(body map[string]interface{}, cc interface{}, remaining *int) {
	messages, ok := body["messages"].([]interface{})
	if !ok || len(messages) == 0 {
		return
	}

	injectedAssistant := false
	injectedUser := false

	for i := len(messages) - 1; i >= 0 && *remaining > 0; i-- {
		msg, ok := messages[i].(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)

		// 跳过非目标 role 或已注入的 role
		if role == "assistant" && injectedAssistant {
			continue
		}
		if role == "user" && injectedUser {
			continue
		}
		if role != "assistant" && role != "user" {
			continue
		}

		content, ok := msg["content"].([]interface{})
		if !ok || len(content) == 0 {
			continue
		}

		// 找最后一个非 thinking/redacted_thinking 块
		for j := len(content) - 1; j >= 0; j-- {
			block, ok := content[j].(map[string]interface{})
			if !ok {
				continue
			}
			t, _ := block["type"].(string)
			if t == "thinking" || t == "redacted_thinking" {
				continue
			}
			if _, exists := block["cache_control"]; !exists {
				block["cache_control"] = cc
				*remaining--
			}
			if role == "assistant" {
				injectedAssistant = true
			} else {
				injectedUser = true
			}
			break
		}
	}
}
