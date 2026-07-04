package main

import (
	"encoding/json"
	"regexp"
)

// ============================================================================
// thinking signature 自动修复
// ============================================================================
//
// Anthropic 的 thinking 块带 signature 字段用于校验完整性。客户端修改请求
// （裁剪 thinking 块、插入新消息、跨会话拼接等）会导致签名校验失败，上游
// 返回 400。本模块检测此类 400 错误，自动剥离 thinking 块后重试，让请求
// 能正常完成（牺牲 thinking 上下文换取请求成功率）。

// thinkingSignatureErrorPatterns 匹配 Anthropic 返回的 thinking signature 错误。
// 覆盖 cc-switch 观察到的 7 类错误消息。
var thinkingSignatureErrorPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)signature.*not.*valid`),
	regexp.MustCompile(`(?i)thought signature is not valid`),
	regexp.MustCompile(`(?i)must start with a thinking block`),
	regexp.MustCompile(`(?i)expected thinking or redacted_thinking but found`),
	regexp.MustCompile(`(?i)signature.*field required`),
	regexp.MustCompile(`(?i)extra inputs are not permitted`),
	regexp.MustCompile(`(?i)thinking.*cannot be modified`),
}

// shouldRectifyThinkingSignature 检测上游 400 错误响应体是否为 thinking
// signature 问题。匹配任一模式返回 true。
func shouldRectifyThinkingSignature(errorBody []byte) bool {
	if len(errorBody) == 0 {
		return false
	}
	bodyStr := string(errorBody)
	for _, p := range thinkingSignatureErrorPatterns {
		if p.MatchString(bodyStr) {
			return true
		}
	}
	return false
}

// rectifyAnthropicRequest 清理 Anthropic 请求体中的 thinking 块以通过签名校验：
//   - 移除所有 thinking / redacted_thinking 块
//   - 剥离非 thinking 块上残留的 signature 字段
//   - 若最后一条 assistant 消息移除 thinking 后不以 thinking 开头，
//     且顶层 thinking 配置为 enabled，则移除 thinking 配置
//     （Anthropic 要求启用 extended thinking 时最后一条 assistant 消息必须以 thinking 开头）
//
// 修改原地 body。无 messages 字段时直接返回。
func rectifyAnthropicRequest(body map[string]interface{}) {
	messages, ok := body["messages"].([]interface{})
	if !ok {
		return
	}

	lastAssistantIdx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if msg, ok := messages[i].(map[string]interface{}); ok {
			if role, _ := msg["role"].(string); role == "assistant" {
				lastAssistantIdx = i
				break
			}
		}
	}

	for _, msgRaw := range messages {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			continue
		}
		content, ok := msg["content"].([]interface{})
		if !ok {
			continue
		}

		// 移除所有 thinking / redacted_thinking 块，剥离非 thinking 块的 signature
		var newContent []interface{}
		removed := false
		for _, blockRaw := range content {
			block, ok := blockRaw.(map[string]interface{})
			if !ok {
				newContent = append(newContent, blockRaw)
				continue
			}
			t, _ := block["type"].(string)
			if t == "thinking" || t == "redacted_thinking" {
				removed = true
				continue
			}
			delete(block, "signature")
			newContent = append(newContent, block)
		}
		if removed {
			msg["content"] = newContent
		}
	}

	// 若有 assistant 消息被处理，移除顶层 thinking 配置以避免 Anthropic
	// "必须以 thinking 开头" 的二次报错
	if lastAssistantIdx >= 0 {
		if thinking, ok := body["thinking"].(map[string]interface{}); ok {
			if t, _ := thinking["type"].(string); t == "enabled" {
				delete(body, "thinking")
			}
		}
	}
}

// rectifyAnthropicRequestBytes 解析 JSON body，调用 rectifyAnthropicRequest，
// 重新序列化返回。失败时返回原 body 与错误。
func rectifyAnthropicRequestBytes(body []byte) ([]byte, error) {
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body, err
	}
	rectifyAnthropicRequest(parsed)
	return json.Marshal(parsed)
}
