package main

import "encoding/json"

// reasoningVendorThinkingPlaceholder 是 reasoning vendor（DeepSeek/Kimi/MiMo）
// 要求 assistant tool-call 消息上 thinking 块非空时使用的占位文本。
const reasoningVendorThinkingPlaceholder = "tool call"

// reasoningVendorHints 是 reasoning vendor 标识关键词列表（port 自 cc-switch claude.rs）。
// 这些 vendor 的 Anthropic 兼容端点拒绝原始 thinking 块，要求用占位符替代。
var reasoningVendorHints = []string{"moonshot", "kimi", "deepseek", "mimo", "xiaomimimo"}

// isReasoningVendorIdentifier 判断字符串是否包含 reasoning vendor 标识。
func isReasoningVendorIdentifier(value string) bool {
	if value == "" {
		return false
	}
	lower := toLowerASCII(value)
	for _, hint := range reasoningVendorHints {
		if containsASCII(lower, hint) {
			return true
		}
	}
	return false
}

// normalizeThinkingHistoryForVendor 将 assistant tool-call 消息中的 thinking 块
// 重写为占位符，使 reasoning vendor（DeepSeek/Kimi/MiMo）的 Anthropic 兼容端点接受请求。
//
// 这些 vendor 拒绝原始 thinking 块（返回 "thinking ... must be passed back" 400），
// 但要求 assistant tool-call 消息上存在非空 thinking 块。处理逻辑：
//   - 剥离 thinking 块的 signature 字段
//   - 空 thinking 文本替换为占位符 "tool call"
//   - redacted_thinking 块改写为 thinking 块，文本为 "[redacted thinking]"
//   - 若 tool-call 消息无任何 thinking 块，在 content 开头插入一个
func normalizeThinkingHistoryForVendor(body map[string]interface{}) {
	messages, ok := body["messages"].([]interface{})
	if !ok {
		return
	}
	for _, msgRaw := range messages {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "assistant" {
			continue
		}
		content, ok := msg["content"].([]interface{})
		if !ok || len(content) == 0 {
			continue
		}
		// 只处理含 tool_use 块的 assistant 消息
		hasToolUse := false
		for _, blockRaw := range content {
			block, ok := blockRaw.(map[string]interface{})
			if !ok {
				continue
			}
			if getString(block, "type") == "tool_use" {
				hasToolUse = true
				break
			}
		}
		if !hasToolUse {
			continue
		}

		hasThinking := false
		for i, blockRaw := range content {
			block, ok := blockRaw.(map[string]interface{})
			if !ok {
				continue
			}
			blockType := getString(block, "type")
			switch blockType {
			case "thinking":
				hasThinking = true
				delete(block, "signature")
				if t, ok := block["thinking"].(string); !ok || t == "" {
					block["thinking"] = reasoningVendorThinkingPlaceholder
				}
			case "redacted_thinking":
				hasThinking = true
				block["type"] = "thinking"
				block["thinking"] = redactedThinkingPlaceholder
				delete(block, "data")
			}
			content[i] = block
		}
		// 若 tool-call 消息无任何 thinking 块，在 content 开头插入占位 thinking 块
		if !hasThinking {
			placeholder := map[string]interface{}{
				"type":     "thinking",
				"thinking": reasoningVendorThinkingPlaceholder,
			}
			msg["content"] = append([]interface{}{placeholder}, content...)
		}
	}
}

// normalizeThinkingHistoryForVendorInBytes 是 byte 级别的封装，供 gateway.go 调用。
func normalizeThinkingHistoryForVendorInBytes(body []byte) ([]byte, error) {
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body, err
	}
	normalizeThinkingHistoryForVendor(parsed)
	return json.Marshal(parsed)
}

// stripEffortIfThinkingDisabled 在 thinking.type != "enabled" 时移除 effort 参数。
// DeepSeek 官方 Anthropic 兼容端点拒绝 thinking:disabled 与 reasoning_effort 共存
// （返回 400 "thinking options type cannot be disabled when reasoning_effort is set"）。
// 移除 output_config.effort（Anthropic 风格）和 reasoning_effort（OpenAI 风格）。
// 返回 true 表示做了修改。
func stripEffortIfThinkingDisabled(body map[string]interface{}) bool {
	thinking := getMap(body, "thinking")
	if thinking == nil {
		return false
	}
	t, ok := thinking["type"].(string)
	if !ok || t == "enabled" {
		return false
	}

	changed := false
	if oc := getMap(body, "output_config"); oc != nil {
		if _, exists := oc["effort"]; exists {
			delete(oc, "effort")
			changed = true
		}
		if len(oc) == 0 {
			delete(body, "output_config")
		}
	}
	if _, exists := body["reasoning_effort"]; exists {
		delete(body, "reasoning_effort")
		changed = true
	}
	return changed
}

// stripEffortIfThinkingDisabledInBytes 是 byte 级别的封装，供 gateway.go 调用。
func stripEffortIfThinkingDisabledInBytes(body []byte) ([]byte, error) {
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body, err
	}
	stripEffortIfThinkingDisabled(parsed)
	return json.Marshal(parsed)
}

// toLowerASCII 将字符串转为小写（ASCII 范围）。
func toLowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// containsASCII 检查 s 是否包含 substr（ASCII 大小写敏感，调用方应先转为小写）。
func containsASCII(s, substr string) bool {
	if len(substr) == 0 {
		return true
	}
	if len(s) < len(substr) {
		return false
	}
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
