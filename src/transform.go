package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ============================================================================
// 格式常量与检测
// ============================================================================

const (
	formatUnknown         = ""
	formatOpenAIChat      = "openai"
	formatOpenAIResponses = "openai_responses"
	formatAnthropic       = "anthropic"
	formatGemini          = "gemini"
)

// redactedThinkingPlaceholder 是 Anthropic redacted_thinking 块转其他格式时的占位文本。
// 加密的 thinking 无法还原为明文，用占位符保留语义，避免客户端丢失 thinking 上下文。
const redactedThinkingPlaceholder = "[redacted thinking]"

var (
	openaiChatPathRegex      = regexp.MustCompile(`(^|.*/)(chat/completions)$`)
	openaiResponsesPathRegex = regexp.MustCompile(`(^|.*/)(responses|responses/compact)$`)
	anthropicPathRegex       = regexp.MustCompile(`(^|.*/)(messages)$`)
)

// detectInputFormat 按 URL path 后缀检测客户端请求的 API 格式。
func detectInputFormat(path string) string {
	if openaiChatPathRegex.MatchString(path) {
		return formatOpenAIChat
	}
	if openaiResponsesPathRegex.MatchString(path) {
		return formatOpenAIResponses
	}
	if anthropicPathRegex.MatchString(path) {
		return formatAnthropic
	}
	if geminiModelRegex.MatchString(path) {
		return formatGemini
	}
	return formatUnknown
}

// openaiModelsListPathRegex 匹配 OpenAI/Anthropic 风格的模型列表端点 /v1/models。
// OpenAI 与 Anthropic 列表共享同一 path，由 detectListFormat 按 auth header 区分。
var openaiModelsListPathRegex = regexp.MustCompile(`(^|.*/)(v1/models)$`)

// geminiModelsListPathRegex 匹配 Gemini 风格的模型列表端点 /v1beta/models。
var geminiModelsListPathRegex = regexp.MustCompile(`(^|.*/)(v1beta/models)$`)

// detectListFormat 按 URL path + auth header 判定客户端模型列表请求的格式。
// 返回 formatXxx 之一；非列表请求返回 formatUnknown。
//
// 客户端格式判定：
//   - /v1beta/models → gemini（path 自带上游风格信号）
//   - /v1/models + X-Api-Key 或 anthropic-version 头任一 → anthropic
//   - /v1/models + 其余（Bearer / 兜底）→ openai_chat（openai_responses 复用同一列表端点）
//
// Anthropic 头兜底同时识别 X-Api-Key（Anthropic SDK 默认 auth 头）与
// anthropic-version（部分客户端即使 token 走 Bearer 也会带 version 头）。
func detectListFormat(path string, headers http.Header) string {
	if geminiModelsListPathRegex.MatchString(path) {
		return formatGemini
	}
	if openaiModelsListPathRegex.MatchString(path) {
		if headers != nil {
			if headers.Get("X-Api-Key") != "" || headers.Get("anthropic-version") != "" {
				return formatAnthropic
			}
		}
		return formatOpenAIChat
	}
	return formatUnknown
}

// targetListEndpointPath 返回目标上游格式对应的模型列表端点 path。
// openai_chat / openai_responses / anthropic 共用 /v1/models，gemini 用 /v1beta/models。
func targetListEndpointPath(format string) string {
	switch format {
	case formatOpenAIChat, formatOpenAIResponses, formatAnthropic:
		return "/v1/models"
	case formatGemini:
		return "/v1beta/models"
	}
	return ""
}

// ============================================================================
// 模型列表响应转换（4 格式 6 向直转）
//
// 列表响应结构简单（id + 元数据），采用直接两两转换而非 anthropic pivot 链式，
// 避免 inputTokenLimit/outputTokenLimit 等字段经 anthropic 中转后丢失。
// openai_chat 与 openai_responses 列表响应结构完全相同，互转直接透传。
// ============================================================================

// modelsListEntry 中性列表条目，承载 4 种格式共有字段。
type modelsListEntry struct {
	ID               string
	Created          int64  // unix 秒（openai_chat 用）
	CreatedAt        string // anthropic ISO 字符串（保留原值）
	OwnedBy          string // openai_chat 用（转 anthropic/gemini 时不写入，丢弃）
	DisplayName      string // gemini / anthropic 用
	Version          string // gemini 用
	InputTokenLimit  int64  // gemini 用
	OutputTokenLimit int64  // gemini 用
}

// modelsList 中性模型列表。
type modelsList struct {
	Entries []modelsListEntry
}

func parseOpenAIModelsList(body map[string]interface{}) (modelsList, error) {
	ml := modelsList{}
	data := getArray(body, "data")
	if data == nil {
		return ml, fmt.Errorf("openai models list: missing data[]")
	}
	for _, e := range data {
		em, ok := asMap(e)
		if !ok {
			continue
		}
		entry := modelsListEntry{
			ID: getString(em, "id"),
		}
		if c, ok := asFloat64(em["created"]); ok {
			entry.Created = int64(c)
		}
		entry.OwnedBy = getString(em, "owned_by")
		// OpenAI 列表响应无 display_name 字段，转 anthropic/gemini 时用 id 兜底，
		// 避免下游客户端看到空 displayName 字段。
		if entry.DisplayName == "" {
			entry.DisplayName = entry.ID
		}
		ml.Entries = append(ml.Entries, entry)
	}
	return ml, nil
}

func parseAnthropicModelsList(body map[string]interface{}) (modelsList, error) {
	ml := modelsList{}
	data := getArray(body, "data")
	if data == nil {
		return ml, fmt.Errorf("anthropic models list: missing data[]")
	}
	for _, e := range data {
		em, ok := asMap(e)
		if !ok {
			continue
		}
		entry := modelsListEntry{
			ID:          getString(em, "id"),
			CreatedAt:   getString(em, "created_at"),
			DisplayName: getString(em, "display_name"),
		}
		// Anthropic 列表只有 ISO 字符串 created_at，无 Unix 时间戳。
		// 解析为 Unix 秒填入 Created，使转 OpenAI 列表时 created 字段非 0，
		// 避免客户端按 created>0 过滤时丢模型。解析失败保持 0（兜底）。
		if entry.CreatedAt != "" {
			if t, err := time.Parse(time.RFC3339, entry.CreatedAt); err == nil {
				entry.Created = t.Unix()
			}
		}
		if entry.DisplayName == "" {
			entry.DisplayName = entry.ID
		}
		ml.Entries = append(ml.Entries, entry)
	}
	return ml, nil
}

func parseGeminiModelsList(body map[string]interface{}) (modelsList, error) {
	ml := modelsList{}
	models := getArray(body, "models")
	if models == nil {
		return ml, fmt.Errorf("gemini models list: missing models[]")
	}
	for _, e := range models {
		em, ok := asMap(e)
		if !ok {
			continue
		}
		// name 形如 "models/gemini-2.0-flash"，取尾段作为 ID
		name := getString(em, "name")
		id := name
		if idx := strings.LastIndex(name, "/"); idx >= 0 && idx < len(name)-1 {
			id = name[idx+1:]
		}
		entry := modelsListEntry{
			ID:          id,
			DisplayName: getString(em, "displayName"),
			Version:     getString(em, "version"),
		}
		if entry.DisplayName == "" {
			entry.DisplayName = id
		}
		if v, ok := asFloat64(em["inputTokenLimit"]); ok {
			entry.InputTokenLimit = int64(v)
		}
		if v, ok := asFloat64(em["outputTokenLimit"]); ok {
			entry.OutputTokenLimit = int64(v)
		}
		ml.Entries = append(ml.Entries, entry)
	}
	return ml, nil
}

// parseModelsListByFormat 按上游格式解析列表响应。
func parseModelsListByFormat(format string, body map[string]interface{}) (modelsList, error) {
	switch format {
	case formatOpenAIChat, formatOpenAIResponses:
		return parseOpenAIModelsList(body)
	case formatAnthropic:
		return parseAnthropicModelsList(body)
	case formatGemini:
		return parseGeminiModelsList(body)
	}
	return modelsList{}, fmt.Errorf("unknown models list format %q", format)
}

func buildOpenAIModelsList(ml modelsList) map[string]interface{} {
	data := make([]interface{}, 0, len(ml.Entries))
	for _, e := range ml.Entries {
		data = append(data, map[string]interface{}{
			"id":       e.ID,
			"object":   "model",
			"created":  e.Created,
			"owned_by": e.OwnedBy,
		})
	}
	return map[string]interface{}{
		"object": "list",
		"data":   data,
	}
}

func buildAnthropicModelsList(ml modelsList) map[string]interface{} {
	data := make([]interface{}, 0, len(ml.Entries))
	for _, e := range ml.Entries {
		entry := map[string]interface{}{
			"type":         "model",
			"id":           e.ID,
			"display_name": e.DisplayName,
			"created_at":   e.CreatedAt,
		}
		data = append(data, entry)
	}
	return map[string]interface{}{
		"object": "list",
		"data":   data,
	}
}

func buildGeminiModelsList(ml modelsList) map[string]interface{} {
	methods := []string{"generateContent", "streamGenerateContent"}
	models := make([]interface{}, 0, len(ml.Entries))
	for _, e := range ml.Entries {
		entry := map[string]interface{}{
			"name":                       "models/" + e.ID,
			"displayName":                e.DisplayName,
			"version":                    e.Version,
			"inputTokenLimit":            e.InputTokenLimit,
			"outputTokenLimit":           e.OutputTokenLimit,
			"supportedGenerationMethods": methods,
		}
		models = append(models, entry)
	}
	return map[string]interface{}{
		"models": models,
	}
}

// buildModelsListByFormat 按客户端格式构造列表响应。
func buildModelsListByFormat(format string, ml modelsList) map[string]interface{} {
	switch format {
	case formatOpenAIChat, formatOpenAIResponses:
		return buildOpenAIModelsList(ml)
	case formatAnthropic:
		return buildAnthropicModelsList(ml)
	case formatGemini:
		return buildGeminiModelsList(ml)
	}
	return map[string]interface{}{"error": "unknown models list format"}
}

// reverseAliasesMap 由 aliases（key→value，client→upstream）构造反向映射
// value→[]key（同一上游真实名可能有多个 alias 指向它）。
func reverseAliasesMap(aliases map[string]string) map[string][]string {
	rev := make(map[string][]string)
	for k, v := range aliases {
		if v == "" || k == "" {
			continue
		}
		rev[v] = append(rev[v], k)
	}
	return rev
}

// applyAliasesReverseToList 对中性 modelsList 应用反向别名：
// 对每条 entry，若其 ID 命中 aliases 的 value（即上游真实模型名），
// 则为每个指向它的 alias key 追加一条新 entry，字段与原 entry 完全相同（仅 ID 改为 alias key）。
// 原 ID 条目保留不变，使客户端既能看到真实名也能看到 alias。
// aliases=nil 时不做任何处理。
func applyAliasesReverseToList(ml *modelsList, aliases map[string]string) {
	if aliases == nil || len(aliases) == 0 {
		return
	}
	rev := reverseAliasesMap(aliases)
	if len(rev) == 0 {
		return
	}
	n := len(ml.Entries)
	for i := 0; i < n; i++ {
		revIDs, ok := rev[ml.Entries[i].ID]
		if !ok {
			continue
		}
		for _, aliasKey := range revIDs {
			if aliasKey == ml.Entries[i].ID {
				continue // alias 与真实名相同，无需重复
			}
			dup := ml.Entries[i] // 值拷贝：所有字段保持一致
			dup.ID = aliasKey
			ml.Entries = append(ml.Entries, dup)
		}
	}
}

// TransformModelsListResponse 转换模型列表响应。outFormat=上游响应格式，inFormat=客户端格式。
// 等价于 TransformResponse 但作用于列表端点，采用 6 向直接两两转换。
// aliases 为该 upstream 的别名配置（nil=未启用），用于反向展开 alias 条目。
func TransformModelsListResponse(inFormat, outFormat string, body []byte, aliases map[string]string) ([]byte, error) {
	if !needsTransform(inFormat, outFormat) {
		return body, nil
	}
	// openai_chat 与 openai_responses 列表结构完全一致，fast path 透传
	if isOpenAIVariant(inFormat) && isOpenAIVariant(outFormat) {
		return body, nil
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse models list: %w", err)
	}
	ml, err := parseModelsListByFormat(outFormat, parsed)
	if err != nil {
		return nil, err
	}
	applyAliasesReverseToList(&ml, aliases)
	out := buildModelsListByFormat(inFormat, ml)
	data, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal models list: %w", err)
	}
	return data, nil
}

// targetEndpointPath 返回目标格式对应的 URL path。
func targetEndpointPath(format, model string, isStream bool) string {
	switch format {
	case formatAnthropic:
		return "/v1/messages"
	case formatOpenAIChat:
		return "/v1/chat/completions"
	case formatOpenAIResponses:
		return "/v1/responses"
	case formatGemini:
		if isStream {
			return "/v1beta/models/" + model + ":streamGenerateContent"
		}
		return "/v1beta/models/" + model + ":generateContent"
	}
	return ""
}

// needsTransform 判断是否需要格式转换。
// openai_chat 与 openai_responses 结构不同（messages/choices vs input/output），
// 需经 anthropic pivot 链式转换。
func needsTransform(inFormat, outFormat string) bool {
	if inFormat == formatUnknown || outFormat == formatUnknown {
		return false
	}
	if inFormat == outFormat {
		return false
	}
	return true
}

func isOpenAIVariant(f string) bool {
	return f == formatOpenAIChat || f == formatOpenAIResponses
}

// mapFormatTransform 将配置中的 formatTransform 字符串映射为内部格式常量。
// 空串或非法值返回 ""（表示透传）。
func mapFormatTransform(s string) string {
	switch s {
	case "openai":
		return formatOpenAIChat
	case "openai_responses":
		return formatOpenAIResponses
	case "anthropic":
		return formatAnthropic
	case "gemini":
		return formatGemini
	}
	return ""
}

// ============================================================================
// 类型安全提取 helpers
// ============================================================================

func asMap(v interface{}) (map[string]interface{}, bool) {
	if m, ok := v.(map[string]interface{}); ok {
		return m, true
	}
	return nil, false
}

func asArray(v interface{}) ([]interface{}, bool) {
	if a, ok := v.([]interface{}); ok {
		return a, true
	}
	return nil, false
}

func asString(v interface{}) (string, bool) {
	if s, ok := v.(string); ok {
		return s, true
	}
	return "", false
}

func asBool(v interface{}) (bool, bool) {
	if b, ok := v.(bool); ok {
		return b, true
	}
	return false, false
}

func asFloat64(v interface{}) (float64, bool) {
	if f, ok := v.(float64); ok {
		return f, true
	}
	return 0, false
}

func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := asString(v); ok {
			return s
		}
	}
	return ""
}

func getMap(m map[string]interface{}, key string) map[string]interface{} {
	if v, ok := m[key]; ok {
		if mm, ok := asMap(v); ok {
			return mm
		}
	}
	return nil
}

func getArray(m map[string]interface{}, key string) []interface{} {
	if v, ok := m[key]; ok {
		if a, ok := asArray(v); ok {
			return a
		}
	}
	return nil
}

// ============================================================================
// 共享辅助函数（port 自 cc-switch）
// ============================================================================

// canonicalJSONString 排序 key 的 JSON 序列化，用于工具参数。
// Go 的 json.Marshal 已按 key 排序 map[string]interface{}。
func canonicalJSONString(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// ensureArgsMap 确保 v 为 map[string]interface{}，否则返回空 map。
// 用于工具调用的 args/input 字段：Gemini 和 Anthropic 均要求该字段为 JSON 对象。
func ensureArgsMap(v interface{}) map[string]interface{} {
	if m, ok := asMap(v); ok {
		return m
	}
	return map[string]interface{}{}
}

// canonicalizeToolArguments 规范化工具调用 arguments 字段。
// 空字符串 → "{}"；字符串尝试解析后规范化；对象直接序列化。
func canonicalizeToolArguments(v interface{}) string {
	if v == nil {
		return "{}"
	}
	if s, ok := asString(v); ok {
		trimmed := strings.TrimSpace(s)
		if trimmed == "" {
			return "{}"
		}
		var parsed interface{}
		if err := json.Unmarshal([]byte(trimmed), &parsed); err == nil {
			return canonicalJSONString(parsed)
		}
		return s
	}
	return canonicalJSONString(v)
}

// cleanSchema 递归移除 JSON Schema 中的 format:"uri" 字段（OpenAI 不接受）。
func cleanSchema(schema map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{}, len(schema))
	for k, v := range schema {
		if k == "format" {
			if s, ok := asString(v); ok && s == "uri" {
				continue
			}
		}
		if k == "properties" {
			if props, ok := asMap(v); ok {
				cleaned := make(map[string]interface{}, len(props))
				for name, prop := range props {
					if pm, ok := asMap(prop); ok {
						cleaned[name] = cleanSchema(pm)
					} else {
						cleaned[name] = prop
					}
				}
				result[k] = cleaned
				continue
			}
		}
		if k == "items" {
			if im, ok := asMap(v); ok {
				result[k] = cleanSchema(im)
				continue
			}
		}
		if k == "anyOf" || k == "oneOf" || k == "allOf" {
			if arr, ok := asArray(v); ok {
				cleaned := make([]interface{}, len(arr))
				for i, item := range arr {
					if im, ok := asMap(item); ok {
						cleaned[i] = cleanSchema(im)
					} else {
						cleaned[i] = item
					}
				}
				result[k] = cleaned
				continue
			}
		}
		// 递归处理其他可能包含嵌套 schema 的关键字
		if k == "not" || k == "if" || k == "then" || k == "else" {
			if im, ok := asMap(v); ok {
				result[k] = cleanSchema(im)
				continue
			}
		}
		if k == "additionalProperties" {
			if im, ok := asMap(v); ok {
				result[k] = cleanSchema(im)
				continue
			}
		}
		if k == "prefixItems" {
			if arr, ok := asArray(v); ok {
				cleaned := make([]interface{}, len(arr))
				for i, item := range arr {
					if im, ok := asMap(item); ok {
						cleaned[i] = cleanSchema(im)
					} else {
						cleaned[i] = item
					}
				}
				result[k] = cleaned
				continue
			}
		}
		result[k] = v
	}
	return result
}

// isOSSeries 判断模型是否为 OpenAI o-series（o1/o3/o4/gpt-5 等）。
var oSeriesRegex = regexp.MustCompile(`^o[1-9]|^gpt-5`)

func isOSSeries(model string) bool {
	return oSeriesRegex.MatchString(model)
}

// supportsReasoningEffort 判断模型是否支持 reasoning.effort 参数。
func supportsReasoningEffort(model string) bool {
	return isOSSeries(model)
}

// openaiExtraPassthroughFields 是 Anthropic↔OpenAI 转换时额外透传的标准参数。
// 这些参数在两种格式中字段名一致，可直接透传，避免客户端设置静默失效。
// stop 已单独处理（Anthropic 用 stop_sequences），stream_options 已单独注入，不在此列。
var openaiExtraPassthroughFields = []string{
	"frequency_penalty",
	"logit_bias",
	"logprobs",
	"metadata",
	"n",
	"parallel_tool_calls",
	"presence_penalty",
	"response_format",
	"seed",
	"service_tier",
	"top_logprobs",
	"user",
}

// resolveReasoningEffort 从 Anthropic thinking 配置提取 reasoning effort。
// 优先级：output_config.effort > thinking.budget_tokens。
// budget_tokens 阈值与 cc-switch 对齐：<4000→low, 4000-15999→medium, >=16000→high。
// adaptive 模式（type=="adaptive" 或 budget_tokens=="adaptive"）映射为 xhigh。
// enabled 无 budget → high（与 Rust 参考一致）。
func resolveReasoningEffort(body map[string]interface{}) string {
	// 优先级 1：output_config.effort（Anthropic 风格）
	if oc := getMap(body, "output_config"); oc != nil {
		if e, ok := asString(oc["effort"]); ok && e != "" {
			switch e {
			case "low", "medium", "high":
				return e
			case "max":
				return "xhigh"
			}
			return "" // 未知值不注入
		}
	}

	// 优先级 2：thinking.type + budget_tokens
	thinking := getMap(body, "thinking")
	if thinking == nil {
		return ""
	}
	t, ok := asString(thinking["type"])
	if !ok {
		return ""
	}
	// adaptive 模式 → xhigh：兼容 type=="adaptive" 和 budget_tokens=="adaptive" 两种 schema
	if t == "adaptive" {
		return "xhigh"
	}
	if t != "enabled" {
		return ""
	}
	if a, ok := asString(thinking["budget_tokens"]); ok && a == "adaptive" {
		return "xhigh"
	}
	if budget, ok := asFloat64(thinking["budget_tokens"]); ok {
		if budget >= 16000 {
			return "high"
		}
		if budget >= 4000 {
			return "medium"
		}
		return "low"
	}
	return "high"
}

// resolveAnthropicThinkingFromReasoning 将 OpenAI reasoning_effort / reasoning.effort
// 反向映射为 Anthropic thinking 配置。budget_tokens 取值保证与 resolveReasoningEffort
// 的阈值往返一致（low<4000<=medium<16000<=high）。maxTokens<=budget 时返回 nil，
// 避免违反 Anthropic "max_tokens > budget_tokens" 约束导致上游 400。
func resolveAnthropicThinkingFromReasoning(body map[string]interface{}, maxTokens int) map[string]interface{} {
	effort := ""
	if e, ok := asString(body["reasoning_effort"]); ok && e != "" {
		effort = e
	} else if r := getMap(body, "reasoning"); r != nil {
		if e, ok := asString(r["effort"]); ok && e != "" {
			effort = e
		}
	}
	if effort == "" {
		return nil
	}
	var budget int
	switch effort {
	case "low":
		// < 4000 → low（resolveReasoningEffort 阈值）
		budget = 2048
	case "medium":
		// 4000-15999 → medium
		budget = 8000
	case "high":
		// >= 16000 → high
		budget = 16000
	case "xhigh":
		// xhigh 无法通过 budget_tokens 完整往返（resolveReasoningEffort 仅到 high），
		// 取 32000 使其落在 high 区间，避免无限增大；xhigh 仅由 output_config.effort=max
		// 或 type="adaptive"/budget_tokens="adaptive" 表达
		budget = 32000
	default:
		return nil
	}
	if maxTokens > 0 && maxTokens <= budget {
		return nil
	}
	return map[string]interface{}{
		"type":          "enabled",
		"budget_tokens": budget,
	}
}

// stripLeadingAnthropicBillingHeader 剥离 Claude Code 的 billing header 前缀。
func stripLeadingAnthropicBillingHeader(text string) string {
	const prefix = "x-anthropic-billing-header:"
	if strings.HasPrefix(text, prefix) {
		rest := text[len(prefix):]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			return strings.TrimSpace(rest[nl+1:])
		}
		return ""
	}
	return text
}

// normalizeOpenAISystemMessages 合并多个 system 消息到队首单个消息。
func normalizeOpenAISystemMessages(messages []interface{}) []interface{} {
	var systemTexts []string
	var rest []interface{}
	for _, msg := range messages {
		m, ok := asMap(msg)
		if !ok {
			rest = append(rest, msg)
			continue
		}
		if getString(m, "role") == "system" {
			switch c := m["content"].(type) {
			case string:
				if c != "" {
					systemTexts = append(systemTexts, c)
				}
			case []interface{}:
				// 数组形式：提取每个 text part
				for _, part := range c {
					if pm, ok := asMap(part); ok {
						pt := getString(pm, "type")
						if pt == "text" || pt == "" {
							if t, ok := asString(pm["text"]); ok && t != "" {
								systemTexts = append(systemTexts, t)
							}
						}
					}
				}
			}
		} else {
			rest = append(rest, msg)
		}
	}
	if len(systemTexts) == 0 {
		return messages
	}
	systemMsg := map[string]interface{}{
		"role":    "system",
		"content": strings.Join(systemTexts, "\n"),
	}
	return append([]interface{}{systemMsg}, rest...)
}

// ============================================================================
// Tool choice 映射
// ============================================================================

// mapToolChoiceToChat 将 Anthropic tool_choice 映射为 OpenAI Chat 格式。
func mapToolChoiceToChat(toolChoice interface{}) interface{} {
	m, ok := asMap(toolChoice)
	if !ok {
		return nil
	}
	t := getString(m, "type")
	switch t {
	case "auto":
		return "auto"
	case "none":
		return "none"
	case "any":
		return "required"
	case "tool":
		name := getString(m, "name")
		if name == "" {
			return "required"
		}
		return map[string]interface{}{
			"type":     "function",
			"function": map[string]interface{}{"name": name},
		}
	}
	return nil
}

// mapToolChoiceToResponses 将 Anthropic tool_choice 映射为 OpenAI Responses 格式。
func mapToolChoiceToResponses(toolChoice interface{}) interface{} {
	m, ok := asMap(toolChoice)
	if !ok {
		return nil
	}
	t := getString(m, "type")
	switch t {
	case "auto":
		return "auto"
	case "none":
		return "none"
	case "any":
		return "required"
	case "tool":
		name := getString(m, "name")
		if name == "" {
			return "required"
		}
		return map[string]interface{}{
			"type": "function",
			"name": name,
		}
	}
	return nil
}

// mapToolChoiceToGemini 将 Anthropic tool_choice 映射为 Gemini toolConfig。
func mapToolChoiceToGemini(toolChoice interface{}) map[string]interface{} {
	m, ok := asMap(toolChoice)
	if !ok {
		return nil
	}
	t := getString(m, "type")
	switch t {
	case "auto":
		return map[string]interface{}{
			"functionCallingConfig": map[string]interface{}{"mode": "AUTO"},
		}
	case "none":
		return map[string]interface{}{
			"functionCallingConfig": map[string]interface{}{"mode": "NONE"},
		}
	case "any":
		return map[string]interface{}{
			"functionCallingConfig": map[string]interface{}{"mode": "ANY"},
		}
	case "tool":
		name := getString(m, "name")
		if name == "" {
			return map[string]interface{}{
				"functionCallingConfig": map[string]interface{}{"mode": "ANY"},
			}
		}
		return map[string]interface{}{
			"functionCallingConfig": map[string]interface{}{
				"mode":                 "ANY",
				"allowedFunctionNames": []interface{}{name},
			},
		}
	}
	return nil
}

// ============================================================================
// Stop/finish reason 映射
// ============================================================================

func mapStopReasonToAnthropic(finishReason string) string {
	switch finishReason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "end_turn"
	}
	return "end_turn"
}

func mapAnthropicStopReasonToOpenAI(stopReason string) string {
	switch stopReason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	}
	return "stop"
}

func mapResponsesStopReason(stopReason, incompleteReason string, hasToolUse bool) string {
	switch stopReason {
	case "completed":
		if hasToolUse {
			return "tool_use"
		}
		return "end_turn"
	case "incomplete":
		switch incompleteReason {
		case "max_output_tokens", "max_tokens", "":
			return "max_tokens"
		default:
			return "end_turn"
		}
	}
	return "end_turn"
}

func mapGeminiFinishReason(reason string, hasToolUse bool) string {
	switch reason {
	case "MAX_TOKENS":
		return "max_tokens"
	case "SAFETY", "RECITATION", "SPII", "BLOCKLIST", "PROHIBITED_CONTENT":
		return "refusal"
	case "STOP":
		if hasToolUse {
			return "tool_use"
		}
		return "end_turn"
	}
	if hasToolUse {
		return "tool_use"
	}
	return "end_turn"
}

// ============================================================================
// Usage token 映射（含 cache token 三桶互斥恒等式）
// ============================================================================

// buildAnthropicUsageFromOpenAI 将 OpenAI usage 转为 Anthropic usage。
// 三桶互斥：input + cache_read + cache_creation == prompt_tokens
func buildAnthropicUsageFromOpenAI(usage map[string]interface{}) map[string]interface{} {
	if usage == nil {
		return map[string]interface{}{"input_tokens": 0, "output_tokens": 0}
	}
	promptTokens := intFromInterface(usage["prompt_tokens"])
	completionTokens := intFromInterface(usage["completion_tokens"])

	cached := 0
	if details, ok := asMap(usage["prompt_tokens_details"]); ok {
		cached = intFromInterface(details["cached_tokens"])
	}
	cacheCreation := intFromInterface(usage["cache_creation_input_tokens"])

	inputTokens := promptTokens - cached - cacheCreation
	if inputTokens < 0 {
		inputTokens = 0
	}

	result := map[string]interface{}{
		"input_tokens":  inputTokens,
		"output_tokens": completionTokens,
	}
	if cached > 0 {
		result["cache_read_input_tokens"] = cached
	}
	if cacheCreation > 0 {
		result["cache_creation_input_tokens"] = cacheCreation
	}
	return result
}

// buildAnthropicUsageFromResponses 将 Responses API usage 转为 Anthropic usage。
func buildAnthropicUsageFromResponses(usage map[string]interface{}) map[string]interface{} {
	if usage == nil {
		return map[string]interface{}{"input_tokens": 0, "output_tokens": 0}
	}
	inputTokens := intFromInterface(usage["input_tokens"])
	outputTokens := intFromInterface(usage["output_tokens"])

	var cached int
	if details, ok := asMap(usage["input_tokens_details"]); ok {
		cached = intFromInterface(details["cached_tokens"])
	}
	if cached == 0 {
		if details, ok := asMap(usage["prompt_tokens_details"]); ok {
			cached = intFromInterface(details["cached_tokens"])
		}
	}
	cacheCreation := intFromInterface(usage["cache_creation_input_tokens"])

	inputTokens -= cached + cacheCreation
	if inputTokens < 0 {
		inputTokens = 0
	}

	result := map[string]interface{}{
		"input_tokens":  inputTokens,
		"output_tokens": outputTokens,
	}
	if cached > 0 {
		result["cache_read_input_tokens"] = cached
	}
	if cacheCreation > 0 {
		result["cache_creation_input_tokens"] = cacheCreation
	}
	return result
}

// buildAnthropicUsageFromGemini 将 Gemini usageMetadata 转为 Anthropic usage。
func buildAnthropicUsageFromGemini(usage map[string]interface{}) map[string]interface{} {
	if usage == nil {
		return map[string]interface{}{"input_tokens": 0, "output_tokens": 0}
	}
	promptTokenCount := intFromInterface(usage["promptTokenCount"])
	candidatesTokenCount := intFromInterface(usage["candidatesTokenCount"])
	cachedContentTokenCount := intFromInterface(usage["cachedContentTokenCount"])

	inputTokens := promptTokenCount - cachedContentTokenCount
	if inputTokens < 0 {
		inputTokens = 0
	}

	result := map[string]interface{}{
		"input_tokens":  inputTokens,
		"output_tokens": candidatesTokenCount,
	}
	if cachedContentTokenCount > 0 {
		result["cache_read_input_tokens"] = cachedContentTokenCount
	}
	return result
}

// buildOpenAIUsageFromAnthropic 将 Anthropic usage 转为 OpenAI usage。
func buildOpenAIUsageFromAnthropic(usage map[string]interface{}) map[string]interface{} {
	if usage == nil {
		return map[string]interface{}{"prompt_tokens": 0, "completion_tokens": 0}
	}
	inputTokens := intFromInterface(usage["input_tokens"])
	outputTokens := intFromInterface(usage["output_tokens"])
	cacheRead := intFromInterface(usage["cache_read_input_tokens"])
	cacheCreation := intFromInterface(usage["cache_creation_input_tokens"])

	result := map[string]interface{}{
		"prompt_tokens":     inputTokens + cacheRead + cacheCreation,
		"completion_tokens": outputTokens,
	}
	if cacheRead > 0 {
		result["prompt_tokens_details"] = map[string]interface{}{
			"cached_tokens": cacheRead,
		}
	}
	if cacheCreation > 0 {
		result["cache_creation_input_tokens"] = cacheCreation
	}
	return result
}

// buildResponsesUsageFromAnthropic 将 Anthropic usage 转为 Responses API usage。
func buildResponsesUsageFromAnthropic(usage map[string]interface{}) map[string]interface{} {
	if usage == nil {
		return map[string]interface{}{"input_tokens": 0, "output_tokens": 0}
	}
	inputTokens := intFromInterface(usage["input_tokens"])
	outputTokens := intFromInterface(usage["output_tokens"])
	cacheRead := intFromInterface(usage["cache_read_input_tokens"])
	cacheCreation := intFromInterface(usage["cache_creation_input_tokens"])

	result := map[string]interface{}{
		"input_tokens":  inputTokens + cacheRead + cacheCreation,
		"output_tokens": outputTokens,
	}
	if cacheRead > 0 {
		result["input_tokens_details"] = map[string]interface{}{
			"cached_tokens": cacheRead,
		}
	}
	return result
}

// buildGeminiUsageFromAnthropic 将 Anthropic usage 转为 Gemini usageMetadata。
func buildGeminiUsageFromAnthropic(usage map[string]interface{}) map[string]interface{} {
	if usage == nil {
		return map[string]interface{}{"promptTokenCount": 0, "candidatesTokenCount": 0}
	}
	inputTokens := intFromInterface(usage["input_tokens"])
	outputTokens := intFromInterface(usage["output_tokens"])
	cacheRead := intFromInterface(usage["cache_read_input_tokens"])

	result := map[string]interface{}{
		"promptTokenCount":     inputTokens + cacheRead,
		"candidatesTokenCount": outputTokens,
	}
	if cacheRead > 0 {
		result["cachedContentTokenCount"] = cacheRead
	}
	return result
}

func intFromInterface(v interface{}) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

// mergeAnthropicUsage 合并 Anthropic 流式用量：input_tokens 及 cache 字段取自
// message_start.usage（start），output_tokens 取自 message_delta.usage（delta）。
// delta 中若也带 input_tokens 则被 start 覆盖，符合 Anthropic 规范。
func mergeAnthropicUsage(start, delta map[string]interface{}) map[string]interface{} {
	merged := map[string]interface{}{}
	for k, v := range start {
		merged[k] = v
	}
	if delta != nil {
		// output_tokens 以 delta 为准（start 中的为初始预占值）
		if v, ok := delta["output_tokens"]; ok {
			merged["output_tokens"] = v
		}
	}
	return merged
}

// ============================================================================
// Gemini schema helpers（port 自 gemini_schema.rs）
// ============================================================================

// geminiSchemaAllowedKeys 是 Gemini parameters schema 接受的字段。
var geminiSchemaAllowedKeys = map[string]bool{
	"type": true, "format": true, "title": true, "description": true,
	"nullable": true, "enum": true, "maxItems": true, "minItems": true,
	"required": true, "minProperties": true, "maxProperties": true,
	"minLength": true, "maxLength": true, "pattern": true, "example": true,
	"propertyOrdering": true, "default": true, "minimum": true, "maximum": true,
}

// buildGeminiFunctionDeclaration 将 Anthropic tool 转为 Gemini FunctionDeclaration。
func buildGeminiFunctionDeclaration(name, description string, inputSchema map[string]interface{}) map[string]interface{} {
	schema := normalizeJSONSchema(inputSchema)
	schema = ensureObjectSchema(schema)

	decl := map[string]interface{}{
		"name":        name,
		"description": description,
	}
	if requiresParametersJSONSchema(schema) {
		decl["parametersJsonSchema"] = schema
	} else {
		decl["parameters"] = toGeminiSchema(schema)
	}
	return decl
}

func ensureObjectSchema(schema map[string]interface{}) map[string]interface{} {
	if _, ok := schema["type"]; !ok {
		schema["type"] = "object"
	}
	if t, ok := asString(schema["type"]); ok && t == "object" {
		if _, ok := schema["properties"]; !ok {
			schema["properties"] = map[string]interface{}{}
		}
	}
	return schema
}

func normalizeJSONSchema(schema map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{}, len(schema))
	for k, v := range schema {
		if k == "$schema" || k == "$id" {
			continue
		}
		if k == "properties" {
			if props, ok := asMap(v); ok {
				cleaned := make(map[string]interface{}, len(props))
				for name, prop := range props {
					if pm, ok := asMap(prop); ok {
						cleaned[name] = normalizeJSONSchema(pm)
					} else {
						cleaned[name] = prop
					}
				}
				result[k] = cleaned
				continue
			}
		}
		if k == "items" {
			if im, ok := asMap(v); ok {
				result[k] = normalizeJSONSchema(im)
				continue
			}
		}
		for _, arrKey := range []string{"anyOf", "oneOf", "allOf", "prefixItems"} {
			if k == arrKey {
				if arr, ok := asArray(v); ok {
					cleaned := make([]interface{}, len(arr))
					for i, item := range arr {
						if im, ok := asMap(item); ok {
							cleaned[i] = normalizeJSONSchema(im)
						} else {
							cleaned[i] = item
						}
					}
					result[k] = cleaned
				}
				continue
			}
		}
		for _, objKey := range []string{"not", "if", "then", "else", "additionalProperties"} {
			if k == objKey {
				if om, ok := asMap(v); ok {
					result[k] = normalizeJSONSchema(om)
				}
				continue
			}
		}
		result[k] = v
	}
	return result
}

func requiresParametersJSONSchema(schema map[string]interface{}) bool {
	for k, v := range schema {
		switch k {
		case "type":
			if _, ok := asArray(v); ok {
				return true
			}
		case "format", "title", "description", "nullable", "enum", "maxItems", "minItems",
			"required", "minProperties", "maxProperties", "minLength", "maxLength",
			"pattern", "example", "propertyOrdering", "default", "minimum", "maximum":
			// allowed in Gemini parameters
		case "properties":
			if props, ok := asMap(v); ok {
				for _, prop := range props {
					if pm, ok := asMap(prop); ok {
						if requiresParametersJSONSchema(pm) {
							return true
						}
					} else {
						return true
					}
				}
			} else {
				return true
			}
		case "items":
			if im, ok := asMap(v); !ok || requiresParametersJSONSchema(im) {
				return true
			}
		case "anyOf":
			if arr, ok := asArray(v); ok {
				for _, item := range arr {
					if im, ok := asMap(item); ok {
						if requiresParametersJSONSchema(im) {
							return true
						}
					} else {
						return true
					}
				}
			} else {
				return true
			}
		case "$ref", "$defs", "definitions", "additionalProperties", "unevaluatedProperties",
			"patternProperties", "oneOf", "allOf", "const", "not", "if", "then", "else",
			"dependentRequired", "dependentSchemas", "contains", "minContains", "maxContains",
			"prefixItems", "exclusiveMinimum", "exclusiveMaximum", "multipleOf", "examples":
			return true
		default:
			return true
		}
	}
	return false
}

func toGeminiSchema(schema map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})
	for k, v := range schema {
		if geminiSchemaAllowedKeys[k] {
			result[k] = v
		}
		if k == "properties" {
			if props, ok := asMap(v); ok {
				cleaned := make(map[string]interface{}, len(props))
				for name, prop := range props {
					if pm, ok := asMap(prop); ok {
						cleaned[name] = toGeminiSchema(pm)
					} else {
						cleaned[name] = prop
					}
				}
				result["properties"] = cleaned
			}
		}
		if k == "items" {
			if im, ok := asMap(v); ok {
				result["items"] = toGeminiSchema(im)
			}
		}
		if k == "anyOf" {
			if arr, ok := asArray(v); ok {
				cleaned := make([]interface{}, len(arr))
				for i, item := range arr {
					if im, ok := asMap(item); ok {
						cleaned[i] = toGeminiSchema(im)
					} else {
						cleaned[i] = item
					}
				}
				result["anyOf"] = cleaned
			}
		}
	}
	return result
}

// ============================================================================
// 请求转换器：Anthropic → X（port 自 cc-switch）
// ============================================================================

// anthropicToOpenAIChatRequest 将 Anthropic Messages 请求转为 OpenAI Chat Completions 请求。
// preserveReasoningContent=true 时，assistant tool-call 消息的 thinking 块文本
// 被提取为 reasoning_content 字段（Kimi/DeepSeek/MiMo 等 reasoning vendor 需要）。
func anthropicToOpenAIChatRequest(body map[string]interface{}, preserveReasoningContent bool) (map[string]interface{}, error) {
	result := make(map[string]interface{})
	model := getString(body, "model")
	if model != "" {
		result["model"] = model
	}

	var messages []interface{}

	// system → messages[0] (role:system)
	if system, ok := body["system"]; ok {
		systemText := extractAnthropicSystemText(system)
		if systemText != "" {
			systemText = stripLeadingAnthropicBillingHeader(systemText)
			messages = append(messages, map[string]interface{}{
				"role":    "system",
				"content": systemText,
			})
		}
	}

	// messages → openai messages
	if msgs := getArray(body, "messages"); msgs != nil {
		for _, msg := range msgs {
			m, ok := asMap(msg)
			if !ok {
				continue
			}
			role := getString(m, "role")
			if role == "" {
				role = "user"
			}
			converted := convertAnthropicMessageToOpenAI(role, m["content"], preserveReasoningContent)
			messages = append(messages, converted...)
		}
	}

	result["messages"] = messages

	// max_tokens
	if mt, ok := body["max_tokens"]; ok {
		if isOSSeries(model) {
			result["max_completion_tokens"] = mt
		} else {
			result["max_tokens"] = mt
		}
	}

	// 透传参数
	isStream := false
	for _, key := range []string{"temperature", "top_p", "stream"} {
		if v, ok := body[key]; ok {
			result[key] = v
			if key == "stream" {
				if b, ok := asBool(v); ok {
					isStream = b
				}
			}
		}
	}
	for _, key := range openaiExtraPassthroughFields {
		if v, ok := body[key]; ok {
			result[key] = v
		}
	}
	if ss := getArray(body, "stop_sequences"); ss != nil {
		result["stop"] = ss
	}

	// 流式请求注入 stream_options.include_usage，确保上游在末尾 chunk 返回 usage。
	// 不注入则转换后的 Anthropic 流 input/output/cache tokens 全为 0。
	// 先复制客户端的 stream_options（若有），再补 include_usage。
	if so, ok := body["stream_options"].(map[string]interface{}); ok {
		result["stream_options"] = so
	}
	if isStream {
		if existing, ok := result["stream_options"].(map[string]interface{}); ok {
			if _, exists := existing["include_usage"]; !exists {
				existing["include_usage"] = true
			}
		} else {
			result["stream_options"] = map[string]interface{}{"include_usage": true}
		}
	}

	// reasoning effort
	if supportsReasoningEffort(model) {
		if effort := resolveReasoningEffort(body); effort != "" {
			if effort == "xhigh" {
				effort = "high"
			}
			result["reasoning_effort"] = effort
		}
	}

	// tools
	if tools := getArray(body, "tools"); tools != nil {
		var openaiTools []interface{}
		for _, tool := range tools {
			tm, ok := asMap(tool)
			if !ok {
				continue
			}
			if getString(tm, "type") == "BatchTool" {
				continue
			}
			name := getString(tm, "name")
			desc := getString(tm, "description")
			inputSchema := getMap(tm, "input_schema")
			if inputSchema == nil {
				inputSchema = map[string]interface{}{}
			}
			openaiTools = append(openaiTools, map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        name,
					"description": desc,
					"parameters":  cleanSchema(inputSchema),
				},
			})
		}
		if len(openaiTools) > 0 {
			result["tools"] = openaiTools
		}
	}

	// tool_choice
	if tc, ok := body["tool_choice"]; ok {
		if mapped := mapToolChoiceToChat(tc); mapped != nil {
			result["tool_choice"] = mapped
		}
	}

	return result, nil
}

func extractAnthropicSystemText(system interface{}) string {
	if s, ok := asString(system); ok {
		return s
	}
	if arr, ok := asArray(system); ok {
		var texts []string
		for _, block := range arr {
			if bm, ok := asMap(block); ok {
				if t, ok := asString(bm["text"]); ok && t != "" {
					texts = append(texts, t)
				}
			}
		}
		return strings.Join(texts, "\n\n")
	}
	return ""
}

// convertAnthropicMessageToOpenAI 将单条 Anthropic 消息转为 OpenAI 消息（可能产生多条）。
// preserveReasoningContent=true 时，assistant tool-call 消息的 thinking 块文本
// 被提取为 reasoning_content 字段（Kimi/DeepSeek/MiMo 等 reasoning vendor 需要）。
func convertAnthropicMessageToOpenAI(role string, content interface{}, preserveReasoningContent bool) []interface{} {
	// content 为字符串 → 单条消息
	if s, ok := asString(content); ok {
		return []interface{}{map[string]interface{}{
			"role":    role,
			"content": s,
		}}
	}

	blocks, ok := asArray(content)
	if !ok {
		return []interface{}{map[string]interface{}{
			"role":    role,
			"content": "",
		}}
	}

	// 收集各类型 block
	var textParts []string
	var imageParts []interface{}
	var toolCalls []interface{}
	var toolResults []interface{}
	var reasoningParts []string

	for _, block := range blocks {
		bm, ok := asMap(block)
		if !ok {
			continue
		}
		blockType := getString(bm, "type")
		switch blockType {
		case "text":
			if t, ok := asString(bm["text"]); ok && t != "" {
				textParts = append(textParts, t)
			}
		case "image":
			if img := convertAnthropicImageToOpenAI(bm); img != nil {
				imageParts = append(imageParts, img)
			}
		case "tool_use":
			id := getString(bm, "id")
			name := getString(bm, "name")
			args := canonicalizeToolArguments(bm["input"])
			toolCalls = append(toolCalls, map[string]interface{}{
				"id":   id,
				"type": "function",
				"function": map[string]interface{}{
					"name":      name,
					"arguments": args,
				},
			})
		case "tool_result":
			toolUseID := getString(bm, "tool_use_id")
			resultContent := convertToolResultContent(bm["content"])
			toolResults = append(toolResults, map[string]interface{}{
				"role":         "tool",
				"tool_call_id": toolUseID,
				"content":      resultContent,
			})
		case "thinking":
			if t, ok := asString(bm["thinking"]); ok && t != "" {
				reasoningParts = append(reasoningParts, t)
			}
		case "redacted_thinking":
			if preserveReasoningContent {
				reasoningParts = append(reasoningParts, redactedThinkingPlaceholder)
			} else {
				textParts = append(textParts, redactedThinkingPlaceholder)
			}
		}
	}

	var result []interface{}

	// 主消息：assistant 带 tool_calls，或 user 带 text+image
	if role == "assistant" {
		msg := map[string]interface{}{"role": "assistant"}
		if len(textParts) > 0 {
			msg["content"] = strings.Join(textParts, "")
		} else if len(toolCalls) > 0 {
			msg["content"] = ""
		} else {
			msg["content"] = ""
		}
		if len(toolCalls) > 0 {
			msg["tool_calls"] = toolCalls
			if preserveReasoningContent {
				rc := reasoningVendorThinkingPlaceholder
				if len(reasoningParts) > 0 {
					rc = strings.Join(reasoningParts, "\n")
				}
				msg["reasoning_content"] = rc
			}
		}
		result = append(result, msg)
	} else if len(toolResults) > 0 {
		// user 消息有 tool_result block 时：
		// 1) tool 角色消息必须先输出（紧跟前面 assistant 的 tool_calls），
		//    否则 OpenAI 兼容后端报 400。
		// 2) 纯 tool_result 的 user 消息不再产生 user 消息。
		// 3) 既有 text 又有 tool_result 时，tool 在前、user 在后。
		result = append(result, toolResults...)
		toolResults = nil
		if len(textParts) > 0 || len(imageParts) > 0 {
			var contentParts []interface{}
			for _, t := range textParts {
				contentParts = append(contentParts, map[string]interface{}{
					"type": "text",
					"text": t,
				})
			}
			contentParts = append(contentParts, imageParts...)
			if len(contentParts) == 1 {
				if tm, ok := asMap(contentParts[0]); ok && getString(tm, "type") == "text" {
					result = append(result, map[string]interface{}{
						"role":    role,
						"content": getString(tm, "text"),
					})
				} else {
					result = append(result, map[string]interface{}{
						"role":    role,
						"content": contentParts,
					})
				}
			} else {
				result = append(result, map[string]interface{}{
					"role":    role,
					"content": contentParts,
				})
			}
		}
	} else {
		// user: 组合 text + image
		var contentParts []interface{}
		for _, t := range textParts {
			contentParts = append(contentParts, map[string]interface{}{
				"type": "text",
				"text": t,
			})
		}
		contentParts = append(contentParts, imageParts...)
		if len(contentParts) == 0 {
			contentParts = []interface{}{map[string]interface{}{"type": "text", "text": ""}}
		}
		// 如果只有 text 且无 image，简化为字符串 content
		if len(contentParts) == 1 {
			if tm, ok := asMap(contentParts[0]); ok && getString(tm, "type") == "text" {
				result = append(result, map[string]interface{}{
					"role":    role,
					"content": getString(tm, "text"),
				})
			} else {
				result = append(result, map[string]interface{}{
					"role":    role,
					"content": contentParts,
				})
			}
		} else {
			result = append(result, map[string]interface{}{
				"role":    role,
				"content": contentParts,
			})
		}
	}

	// tool_result 作为独立的 tool 角色消息（仅限尚未被 emit 的部分）
	result = append(result, toolResults...)

	return result
}

func convertAnthropicImageToOpenAI(block map[string]interface{}) interface{} {
	source := getMap(block, "source")
	if source == nil {
		return nil
	}
	mediaType := getString(source, "media_type")
	data := getString(source, "data")
	sourceType := getString(source, "type")
	if sourceType != "base64" || data == "" {
		return nil
	}
	return map[string]interface{}{
		"type": "image_url",
		"image_url": map[string]interface{}{
			"url": "data:" + mediaType + ";base64," + data,
		},
	}
}

func convertToolResultContent(content interface{}) string {
	if s, ok := asString(content); ok {
		return s
	}
	if arr, ok := asArray(content); ok {
		var texts []string
		for _, block := range arr {
			if bm, ok := asMap(block); ok {
				if t, ok := asString(bm["text"]); ok {
					texts = append(texts, t)
				}
			}
		}
		return strings.Join(texts, "\n")
	}
	return ""
}

// anthropicToOpenAIResponsesRequest 将 Anthropic Messages 请求转为 OpenAI Responses API 请求。
func anthropicToOpenAIResponsesRequest(body map[string]interface{}) (map[string]interface{}, error) {
	result := make(map[string]interface{})
	model := getString(body, "model")
	if model != "" {
		result["model"] = model
	}

	// system → instructions
	if system, ok := body["system"]; ok {
		instructions := extractAnthropicSystemText(system)
		if instructions != "" {
			instructions = stripLeadingAnthropicBillingHeader(instructions)
			result["instructions"] = instructions
		}
	}

	// messages → input
	if msgs := getArray(body, "messages"); msgs != nil {
		input := convertAnthropicMessagesToResponsesInput(msgs)
		result["input"] = input
	}

	// max_tokens → max_output_tokens
	if mt, ok := body["max_tokens"]; ok {
		result["max_output_tokens"] = mt
	}

	for _, key := range []string{"temperature", "top_p", "stream"} {
		if v, ok := body[key]; ok {
			result[key] = v
		}
	}
	for _, key := range openaiExtraPassthroughFields {
		if v, ok := body[key]; ok {
			result[key] = v
		}
	}

	// reasoning effort
	if supportsReasoningEffort(model) {
		if effort := resolveReasoningEffort(body); effort != "" {
			if effort == "xhigh" {
				effort = "high"
			}
			result["reasoning"] = map[string]interface{}{"effort": effort}
		}
	}

	// tools
	if tools := getArray(body, "tools"); tools != nil {
		var respTools []interface{}
		for _, tool := range tools {
			tm, ok := asMap(tool)
			if !ok {
				continue
			}
			if getString(tm, "type") == "BatchTool" {
				continue
			}
			name := getString(tm, "name")
			desc := getString(tm, "description")
			inputSchema := getMap(tm, "input_schema")
			if inputSchema == nil {
				inputSchema = map[string]interface{}{}
			}
			respTools = append(respTools, map[string]interface{}{
				"type":        "function",
				"name":        name,
				"description": desc,
				"parameters":  cleanSchema(inputSchema),
			})
		}
		if len(respTools) > 0 {
			result["tools"] = respTools
		}
	}

	// tool_choice
	if tc, ok := body["tool_choice"]; ok {
		if mapped := mapToolChoiceToResponses(tc); mapped != nil {
			result["tool_choice"] = mapped
		}
	}

	return result, nil
}

// shapeForCodexBackend 对发往 ChatGPT Codex OAuth 后端的 Responses 请求做字段整形。
// Codex 后端（chatgpt.com/backend-api/codex）对 Responses API 请求有特殊要求：
//   - 必须 store:false
//   - 必须 include:["reasoning.encrypted_content"]
//   - service_tier:"priority"（fast 模式）
//   - 必须 stream:true
//   - 拒绝 max_output_tokens/temperature/top_p
//
// fastMode=true 时注入 service_tier:"priority"。
func shapeForCodexBackend(body map[string]interface{}, fastMode bool) {
	body["store"] = false

	const reasoningMarker = "reasoning.encrypted_content"
	var includes []interface{}
	if existing, ok := body["include"].([]interface{}); ok {
		includes = append(includes, existing...)
	}
	found := false
	for _, v := range includes {
		if s, ok := v.(string); ok && s == reasoningMarker {
			found = true
			break
		}
	}
	if !found {
		includes = append(includes, reasoningMarker)
	}
	body["include"] = includes

	if fastMode {
		body["service_tier"] = "priority"
	}

	// 兜底必填字段
	if _, ok := body["instructions"]; !ok {
		body["instructions"] = ""
	}
	if _, ok := body["tools"]; !ok {
		body["tools"] = []interface{}{}
	}
	if _, ok := body["parallel_tool_calls"]; !ok {
		body["parallel_tool_calls"] = false
	}

	body["stream"] = true

	delete(body, "max_output_tokens")
	delete(body, "temperature")
	delete(body, "top_p")
}

// shapeForCodexBackendInBytes 是 byte 级别的封装，供 gateway.go 调用。
func shapeForCodexBackendInBytes(body []byte, fastMode bool) ([]byte, error) {
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body, err
	}
	shapeForCodexBackend(parsed, fastMode)
	return json.Marshal(parsed)
}

// convertAnthropicMessagesToResponsesInput 将 Anthropic messages 转为 Responses API input。
// tool_use 提升为顶层 function_call 项，tool_result 提升为 function_call_output 项。
func convertAnthropicMessagesToResponsesInput(messages []interface{}) []interface{} {
	var input []interface{}
	for _, msg := range messages {
		m, ok := asMap(msg)
		if !ok {
			continue
		}
		role := getString(m, "role")
		if role == "system" {
			continue // system 已提取到 instructions
		}

		// content 为字符串
		if s, ok := asString(m["content"]); ok {
			input = append(input, map[string]interface{}{
				"type":    "message",
				"role":    role,
				"content": s,
			})
			continue
		}

		blocks, ok := asArray(m["content"])
		if !ok {
			continue
		}

		// 收集消息内容块
		var contentParts []interface{}
		for _, block := range blocks {
			bm, ok := asMap(block)
			if !ok {
				continue
			}
			blockType := getString(bm, "type")
			switch blockType {
			case "text":
				contentParts = append(contentParts, map[string]interface{}{
					"type": "input_text",
					"text": getString(bm, "text"),
				})
			case "image":
				source := getMap(bm, "source")
				if source != nil {
					mediaType := getString(source, "media_type")
					data := getString(source, "data")
					if getString(source, "type") == "base64" && data != "" {
						contentParts = append(contentParts, map[string]interface{}{
							"type":      "input_image",
							"image_url": "data:" + mediaType + ";base64," + data,
						})
					}
				}
			case "document":
				source := getMap(bm, "source")
				if source != nil {
					mediaType := getString(source, "media_type")
					if mediaType == "" {
						mediaType = "application/pdf"
					}
					data := getString(source, "data")
					if data != "" {
						contentParts = append(contentParts, map[string]interface{}{
							"type": "input_file",
							"file_data": map[string]interface{}{
								"mime_type": mediaType,
								"data":      data,
							},
						})
					}
				}
			case "tool_use":
				// 提升为顶层 function_call 项
				input = append(input, map[string]interface{}{
					"type":      "function_call",
					"call_id":   getString(bm, "id"),
					"name":      getString(bm, "name"),
					"arguments": canonicalizeToolArguments(bm["input"]),
				})
			case "tool_result":
				// 提升为顶层 function_call_output 项
				resultContent := convertToolResultContent(bm["content"])
				input = append(input, map[string]interface{}{
					"type":    "function_call_output",
					"call_id": getString(bm, "tool_use_id"),
					"output":  resultContent,
				})
			case "thinking":
				// skip
			case "redacted_thinking":
				contentParts = append(contentParts, map[string]interface{}{
					"type": "input_text",
					"text": redactedThinkingPlaceholder,
				})
			}
		}

		if len(contentParts) > 0 {
			input = append(input, map[string]interface{}{
				"type":    "message",
				"role":    role,
				"content": contentParts,
			})
		}
	}
	return input
}

// anthropicToGeminiRequest 将 Anthropic Messages 请求转为 Gemini generateContent 请求。
func anthropicToGeminiRequest(body map[string]interface{}) (map[string]interface{}, error) {
	result := make(map[string]interface{})

	// system → systemInstruction
	messages := getArray(body, "messages")
	systemInstruction := buildGeminiSystemInstruction(body["system"], messages)
	if systemInstruction != nil {
		result["systemInstruction"] = systemInstruction
	}

	// 构建 tool_use_id → name 映射，用于 tool_result 的 functionResponse 名字解析
	toolNames := buildToolNameMap(messages)

	// messages → contents
	if messages != nil {
		contents := convertAnthropicMessagesToGeminiContents(messages, toolNames)
		if len(contents) > 0 {
			result["contents"] = contents
		}
	}

	// generationConfig
	genConfig := buildGeminiGenerationConfig(body)
	if len(genConfig) > 0 {
		result["generationConfig"] = genConfig
	}

	// tools
	if tools := getArray(body, "tools"); tools != nil {
		var funcDecls []interface{}
		for _, tool := range tools {
			tm, ok := asMap(tool)
			if !ok {
				continue
			}
			if getString(tm, "type") == "BatchTool" {
				continue
			}
			name := getString(tm, "name")
			desc := getString(tm, "description")
			inputSchema := getMap(tm, "input_schema")
			if inputSchema == nil {
				inputSchema = map[string]interface{}{}
			}
			funcDecls = append(funcDecls, buildGeminiFunctionDeclaration(name, desc, inputSchema))
		}
		if len(funcDecls) > 0 {
			result["tools"] = []interface{}{
				map[string]interface{}{"functionDeclarations": funcDecls},
			}
		}
	}

	// toolConfig
	if tc, ok := body["tool_choice"]; ok {
		if mapped := mapToolChoiceToGemini(tc); mapped != nil {
			result["toolConfig"] = mapped
		}
	}

	return result, nil
}

func buildGeminiSystemInstruction(system interface{}, messages []interface{}) interface{} {
	var texts []string
	if system != nil {
		if s, ok := asString(system); ok && s != "" {
			texts = append(texts, s)
		}
		if arr, ok := asArray(system); ok {
			for _, block := range arr {
				if bm, ok := asMap(block); ok {
					if t, ok := asString(bm["text"]); ok && t != "" {
						texts = append(texts, t)
					}
				}
			}
		}
	}
	// 也从 messages 中的 system role 提取
	if messages != nil {
		for _, msg := range messages {
			m, ok := asMap(msg)
			if !ok {
				continue
			}
			if getString(m, "role") != "system" {
				continue
			}
			if s, ok := asString(m["content"]); ok && s != "" {
				texts = append(texts, s)
			}
			if arr, ok := asArray(m["content"]); ok {
				for _, block := range arr {
					if bm, ok := asMap(block); ok {
						if t, ok := asString(bm["text"]); ok && t != "" {
							texts = append(texts, t)
						}
					}
				}
			}
		}
	}
	if len(texts) == 0 {
		return nil
	}
	return map[string]interface{}{
		"parts": []interface{}{
			map[string]interface{}{"text": strings.Join(texts, "\n\n")},
		},
	}
}

func buildGeminiGenerationConfig(body map[string]interface{}) map[string]interface{} {
	config := make(map[string]interface{})
	if v, ok := body["max_tokens"]; ok {
		config["maxOutputTokens"] = v
	}
	if v, ok := body["temperature"]; ok {
		config["temperature"] = v
	}
	if v, ok := body["top_p"]; ok {
		config["topP"] = v
	}
	if v, ok := body["stop_sequences"]; ok {
		config["stopSequences"] = v
	}
	return config
}

func convertAnthropicMessagesToGeminiContents(messages []interface{}, toolNames map[string]string) []interface{} {
	var contents []interface{}
	for _, msg := range messages {
		m, ok := asMap(msg)
		if !ok {
			continue
		}
		role := getString(m, "role")
		if role == "system" {
			continue
		}
		geminiRole := "user"
		if role == "assistant" {
			geminiRole = "model"
		}
		// 影子存储回放：assistant 消息含 tool_use 时，尝试用原始 Gemini parts 替换，
		// 保留 thought:true 块和 thoughtSignature，避免 Gemini 校验失败
		var parts []interface{}
		if role == "assistant" {
			if stored := tryLookupGeminiAssistantParts(m["content"]); stored != nil {
				parts = stored
			}
		}
		if parts == nil {
			parts = convertAnthropicContentToGeminiParts(m["content"], toolNames)
		}
		if len(parts) > 0 {
			contents = append(contents, map[string]interface{}{
				"role":  geminiRole,
				"parts": parts,
			})
		}
	}
	return contents
}

// tryLookupGeminiAssistantParts 尝试从影子存储中查找 Anthropic assistant 消息
// 对应的原始 Gemini parts。通过第一个 tool_use 块的 id 查找。
// 返回 nil 表示未找到，调用方应回退到正常转换。
func tryLookupGeminiAssistantParts(content interface{}) []interface{} {
	blocks, ok := asArray(content)
	if !ok {
		return nil
	}
	for _, block := range blocks {
		bm, ok := asMap(block)
		if !ok {
			continue
		}
		if getString(bm, "type") == "tool_use" {
			id := getString(bm, "id")
			if id == "" {
				return nil
			}
			return lookupGeminiAssistantParts(id)
		}
	}
	return nil
}

func convertAnthropicContentToGeminiParts(content interface{}, toolNames map[string]string) []interface{} {
	if s, ok := asString(content); ok {
		if s == "" {
			return nil
		}
		return []interface{}{map[string]interface{}{"text": s}}
	}
	blocks, ok := asArray(content)
	if !ok {
		return nil
	}
	var parts []interface{}
	for _, block := range blocks {
		bm, ok := asMap(block)
		if !ok {
			continue
		}
		blockType := getString(bm, "type")
		switch blockType {
		case "text":
			if t, ok := asString(bm["text"]); ok && t != "" {
				parts = append(parts, map[string]interface{}{"text": t})
			}
		case "image":
			source := getMap(bm, "source")
			if source != nil {
				mediaType := getString(source, "media_type")
				data := getString(source, "data")
				if getString(source, "type") == "base64" && data != "" {
					parts = append(parts, map[string]interface{}{
						"inlineData": map[string]interface{}{
							"mimeType": mediaType,
							"data":     data,
						},
					})
				}
			}
		case "document":
			source := getMap(bm, "source")
			if source != nil {
				mediaType := getString(source, "media_type")
				if mediaType == "" {
					mediaType = "application/pdf"
				}
				data := getString(source, "data")
				if data != "" {
					parts = append(parts, map[string]interface{}{
						"inlineData": map[string]interface{}{
							"mimeType": mediaType,
							"data":     data,
						},
					})
				}
			}
		case "tool_use":
			toolCallID := getString(bm, "id")
			functionCall := map[string]interface{}{
				"id":   toolCallID,
				"name": getString(bm, "name"),
				"args": ensureArgsMap(bm["input"]),
			}
			// 影子存储：从历史响应中查回 thoughtSignature，附到 functionCall 上，
			// 让 Gemini 能校验多轮工具调用的 thinking 完整性
			if sig := lookupGeminiThoughtSignature(toolCallID); sig != "" {
				functionCall["thoughtSignature"] = sig
			}
			parts = append(parts, map[string]interface{}{
				"functionCall": functionCall,
			})
		case "redacted_thinking":
			parts = append(parts, map[string]interface{}{
				"text":    redactedThinkingPlaceholder,
				"thought": true,
			})
		case "tool_result":
			toolUseID := getString(bm, "tool_use_id")
			name := resolveToolName(toolUseID, toolNames)
			resultContent := convertToolResultContent(bm["content"])
			// Gemini 要求 functionResponse.response 为 JSON 对象。
			var response map[string]interface{}
			if resultContent != "" {
				var parsed interface{}
				if err := json.Unmarshal([]byte(resultContent), &parsed); err == nil {
					if m, ok := asMap(parsed); ok {
						response = m
					} else {
						response = map[string]interface{}{"result": parsed}
					}
				} else {
					response = map[string]interface{}{"result": resultContent}
				}
			} else {
				response = map[string]interface{}{}
			}
			parts = append(parts, map[string]interface{}{
				"functionResponse": map[string]interface{}{
					"id":       toolUseID,
					"name":     name,
					"response": response,
				},
			})
		}
	}
	return parts
}

// buildToolNameMap 扫描 messages 中所有 assistant 消息的 tool_use 块，
// 构建 tool_use_id → tool_name 的映射，供 tool_result 转换时解析函数名。
func buildToolNameMap(messages []interface{}) map[string]string {
	if messages == nil {
		return nil
	}
	result := make(map[string]string)
	for _, msg := range messages {
		m, ok := asMap(msg)
		if !ok {
			continue
		}
		if getString(m, "role") != "assistant" {
			continue
		}
		blocks, ok := asArray(m["content"])
		if !ok {
			continue
		}
		for _, block := range blocks {
			bm, ok := asMap(block)
			if !ok {
				continue
			}
			if getString(bm, "type") != "tool_use" {
				continue
			}
			id := getString(bm, "id")
			name := getString(bm, "name")
			if id != "" && name != "" {
				result[id] = name
			}
		}
	}
	return result
}

// resolveToolName 从 tool name map 中查找工具名；找不到时回退为 "unknown"。
func resolveToolName(id string, toolNames map[string]string) string {
	if id == "" {
		return "unknown"
	}
	if toolNames != nil {
		if name, ok := toolNames[id]; ok {
			return name
		}
	}
	return "unknown"
}

// ============================================================================
// 响应转换器：X → Anthropic（port 自 cc-switch）
// ============================================================================

// openaiChatToAnthropicResponse 将 OpenAI Chat 响应转为 Anthropic Messages 响应。
func openaiChatToAnthropicResponse(body map[string]interface{}) (map[string]interface{}, error) {
	id := getString(body, "id")
	model := getString(body, "model")

	choices := getArray(body, "choices")
	var content []interface{}
	stopReason := "end_turn"
	hasToolUse := false

	if len(choices) > 0 {
		if choice, ok := asMap(choices[0]); ok {
			msg := getMap(choice, "message")
			if msg != nil {
				// reasoning_content（DeepSeek/Kimi 等 thinking）
				if rc, ok := asString(msg["reasoning_content"]); ok && rc != "" {
					content = append(content, map[string]interface{}{
						"type":     "thinking",
						"thinking": rc,
					})
				}
				// content：字符串或数组
				if s, ok := asString(msg["content"]); ok && s != "" {
					content = append(content, map[string]interface{}{
						"type": "text",
						"text": s,
					})
				} else if contentArr := getArray(msg, "content"); contentArr != nil {
					for _, c := range contentArr {
						cm, ok := asMap(c)
						if !ok {
							continue
						}
						ct := getString(cm, "type")
						switch ct {
						case "text", "output_text":
							if t, ok := asString(cm["text"]); ok && t != "" {
								content = append(content, map[string]interface{}{
									"type": "text",
									"text": t,
								})
							}
						case "refusal":
							if t, ok := asString(cm["refusal"]); ok && t != "" {
								content = append(content, map[string]interface{}{
									"type": "text",
									"text": "[refusal] " + t,
								})
							}
						}
					}
				}
				// 顶层 refusal（有些上游放在 message.refusal 而非 content 数组）
				if r, ok := asString(msg["refusal"]); ok && r != "" {
					content = append(content, map[string]interface{}{
						"type": "text",
						"text": "[refusal] " + r,
					})
				}
				// tool_calls
				if toolCalls := getArray(msg, "tool_calls"); toolCalls != nil {
					for _, tc := range toolCalls {
						tcm, ok := asMap(tc)
						if !ok {
							continue
						}
						hasToolUse = true
						fn := getMap(tcm, "function")
						if fn == nil {
							continue
						}
						argsStr := canonicalizeToolArguments(fn["arguments"])
						var input interface{}
						if err := json.Unmarshal([]byte(argsStr), &input); err != nil {
							input = map[string]interface{}{}
						}
						content = append(content, map[string]interface{}{
							"type":  "tool_use",
							"id":    getString(tcm, "id"),
							"name":  getString(fn, "name"),
							"input": input,
						})
					}
				}
				// legacy function_call（老版 OpenAI 兼容）
				if fc := getMap(msg, "function_call"); fc != nil {
					hasToolUse = true
					argsStr := canonicalizeToolArguments(fc["arguments"])
					var input interface{}
					if err := json.Unmarshal([]byte(argsStr), &input); err != nil {
						input = map[string]interface{}{}
					}
					content = append(content, map[string]interface{}{
						"type":  "tool_use",
						"id":    getString(fc, "id"),
						"name":  getString(fc, "name"),
						"input": input,
					})
				}
			}
			if fr, ok := asString(choice["finish_reason"]); ok {
				stopReason = mapStopReasonToAnthropic(fr)
			}
		}
	}

	if content == nil {
		content = []interface{}{}
	}

	result := map[string]interface{}{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"content":       content,
		"model":         model,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         buildAnthropicUsageFromOpenAI(getMap(body, "usage")),
	}
	if hasToolUse && stopReason == "end_turn" {
		result["stop_reason"] = "tool_use"
	}
	return result, nil
}

// openaiResponsesToAnthropicResponse 将 OpenAI Responses API 响应转为 Anthropic Messages 响应。
func openaiResponsesToAnthropicResponse(body map[string]interface{}) (map[string]interface{}, error) {
	id := getString(body, "id")
	model := getString(body, "model")

	var content []interface{}
	hasToolUse := false

	outputArr := getArray(body, "output")
	for _, item := range outputArr {
		im, ok := asMap(item)
		if !ok {
			continue
		}
		itemType := getString(im, "type")
		switch itemType {
		case "message":
			role := getString(im, "role")
			if role == "assistant" {
				contentArr := getArray(im, "content")
				for _, c := range contentArr {
					cm, ok := asMap(c)
					if !ok {
						continue
					}
					ct := getString(cm, "type")
					switch ct {
					case "output_text":
						if t, ok := asString(cm["text"]); ok && t != "" {
							content = append(content, map[string]interface{}{
								"type": "text",
								"text": t,
							})
						}
					}
				}
			}
		case "function_call":
			hasToolUse = true
			argsStr := canonicalizeToolArguments(im["arguments"])
			var input interface{}
			if err := json.Unmarshal([]byte(argsStr), &input); err != nil {
				input = map[string]interface{}{}
			}
			content = append(content, map[string]interface{}{
				"type":  "tool_use",
				"id":    getString(im, "call_id"),
				"name":  getString(im, "name"),
				"input": input,
			})
		}
	}

	stopReason := mapResponsesStopReason(getString(body, "status"), getString(body, "incomplete_reason"), hasToolUse)

	if content == nil {
		content = []interface{}{}
	}

	result := map[string]interface{}{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"content":       content,
		"model":         model,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         buildAnthropicUsageFromResponses(getMap(body, "usage")),
	}
	return result, nil
}

// geminiToAnthropicResponse 将 Gemini GenerateContent 响应转为 Anthropic Messages 响应。
func geminiToAnthropicResponse(body map[string]interface{}) (map[string]interface{}, error) {
	// 检查 promptFeedback.blockReason
	if pf := getMap(body, "promptFeedback"); pf != nil {
		if blockReason, ok := asString(pf["blockReason"]); ok && blockReason != "" {
			text := "Request blocked by Gemini safety filters: " + blockReason
			return map[string]interface{}{
				"id":            getString(body, "responseId"),
				"type":          "message",
				"role":          "assistant",
				"content":       []interface{}{map[string]interface{}{"type": "text", "text": text}},
				"model":         getString(body, "modelVersion"),
				"stop_reason":   "end_turn",
				"stop_sequence": nil,
				"usage":         buildAnthropicUsageFromGemini(getMap(body, "usageMetadata")),
			}, nil
		}
	}

	candidates := getArray(body, "candidates")
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no candidates in Gemini response")
	}
	candidate, ok := asMap(candidates[0])
	if !ok {
		return nil, fmt.Errorf("invalid candidate in Gemini response")
	}

	contentObj := getMap(candidate, "content")
	var parts []interface{}
	if contentObj != nil {
		parts = getArray(contentObj, "parts")
	}

	var content []interface{}
	hasToolUse := false

	for _, part := range parts {
		pm, ok := asMap(part)
		if !ok {
			continue
		}
		// 跳过 thought blocks
		if b, ok := asBool(pm["thought"]); ok && b {
			continue
		}
		// text
		if t, ok := asString(pm["text"]); ok && t != "" {
			content = append(content, map[string]interface{}{
				"type": "text",
				"text": t,
			})
			continue
		}
		// functionCall
		if fc := getMap(pm, "functionCall"); fc != nil {
			hasToolUse = true
			id := getString(fc, "id")
			if id == "" {
				id = synthesizeToolCallID()
			}
			// 影子存储：抓取 thoughtSignature 供下一轮 Anthropic→Gemini 复用
			if sig := extractGeminiThoughtSignature(pm); sig != "" {
				storeGeminiThoughtSignature(id, sig)
			}
			content = append(content, map[string]interface{}{
				"type":  "tool_use",
				"id":    id,
				"name":  getString(fc, "name"),
				"input": ensureArgsMap(fc["args"]),
			})
		}
	}

	// 影子存储：若 assistant turn 含 functionCall，存完整 Gemini parts
	// （含 thought:true 块）供下一轮 Anthropic→Gemini 原样回放
	if hasToolUse {
		for _, part := range parts {
			pm, ok := asMap(part)
			if !ok {
				continue
			}
			if fc := getMap(pm, "functionCall"); fc != nil {
				if id := getString(fc, "id"); id != "" {
					storeGeminiAssistantTurn(id, parts)
					break
				}
			}
		}
	} else {
		// thinking-only turn（无 tool_use）：提取纯文本 part 的 thoughtSignature，
		// 用 responseId 作键存入 shadow store 供后续回放
		respID := getString(body, "responseId")
		if respID != "" {
			for _, part := range parts {
				pm, ok := asMap(part)
				if !ok {
					continue
				}
				if sig := extractGeminiTextPartThoughtSignature(pm); sig != "" {
					storeGeminiThoughtSignature(respID, sig)
					break
				}
			}
		}
	}

	stopReason := mapGeminiFinishReason(getString(candidate, "finishReason"), hasToolUse)

	if content == nil {
		content = []interface{}{}
	}

	result := map[string]interface{}{
		"id":            getString(body, "responseId"),
		"type":          "message",
		"role":          "assistant",
		"content":       content,
		"model":         getString(body, "modelVersion"),
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         buildAnthropicUsageFromGemini(getMap(body, "usageMetadata")),
	}
	return result, nil
}

// ============================================================================
// 逆向请求转换器：X → Anthropic（新写）
// ============================================================================

// openaiChatToAnthropicRequest 将 OpenAI Chat Completions 请求转为 Anthropic Messages 请求。
func openaiChatToAnthropicRequest(body map[string]interface{}) (map[string]interface{}, error) {
	result := make(map[string]interface{})
	model := getString(body, "model")
	if model != "" {
		result["model"] = model
	}

	var systemTexts []string
	var messages []interface{}

	rawMessages := getArray(body, "messages")
	if rawMessages != nil {
		rawMessages = normalizeOpenAISystemMessages(rawMessages)
	}

	for _, msg := range rawMessages {
		m, ok := asMap(msg)
		if !ok {
			continue
		}
		role := getString(m, "role")
		switch role {
		case "system":
			if s, ok := asString(m["content"]); ok && s != "" {
				systemTexts = append(systemTexts, s)
			}
		case "user":
			messages = append(messages, convertOpenAIChatMessageToAnthropic("user", m))
		case "assistant":
			messages = append(messages, convertOpenAIChatMessageToAnthropic("assistant", m))
		case "tool":
			// tool role → tool_result。连续的 tool 消息合并到同一个 user message，
			// 因为 Anthropic 要求同一 turn 的多个 tool_result 必须在同一个 user 消息内。
			toolResult := map[string]interface{}{
				"type":        "tool_result",
				"tool_use_id": getString(m, "tool_call_id"),
				"content":     extractOpenAIToolContent(m["content"]),
			}
			if merged := mergeToolResultIntoLastUser(messages, toolResult); merged {
				continue
			}
			messages = append(messages, map[string]interface{}{
				"role":    "user",
				"content": []interface{}{toolResult},
			})
		}
	}

	if len(systemTexts) > 0 {
		result["system"] = strings.Join(systemTexts, "\n\n")
	}
	result["messages"] = messages

	// max_tokens
	if mt, ok := body["max_tokens"]; ok {
		result["max_tokens"] = mt
	} else if mct, ok := body["max_completion_tokens"]; ok {
		result["max_tokens"] = mct
	}

	// reasoning_effort / reasoning.effort → thinking（反向映射，对称正向 resolveReasoningEffort）
	if supportsReasoningEffort(model) {
		maxTokens := intFromInterface(result["max_tokens"])
		if thinking := resolveAnthropicThinkingFromReasoning(body, maxTokens); thinking != nil {
			result["thinking"] = thinking
		}
	}

	for _, key := range []string{"temperature", "top_p", "stream"} {
		if v, ok := body[key]; ok {
			result[key] = v
		}
	}
	for _, key := range openaiExtraPassthroughFields {
		if v, ok := body[key]; ok {
			result[key] = v
		}
	}
	// stop：OpenAI 允许字符串或字符串数组，统一转为数组
	if stopArr := getArray(body, "stop"); stopArr != nil {
		result["stop_sequences"] = stopArr
	} else if stopStr, ok := asString(body["stop"]); ok && stopStr != "" {
		result["stop_sequences"] = []interface{}{stopStr}
	}

	// tools
	if tools := getArray(body, "tools"); tools != nil {
		var anthropicTools []interface{}
		for _, tool := range tools {
			tm, ok := asMap(tool)
			if !ok {
				continue
			}
			fn := getMap(tm, "function")
			if fn == nil {
				continue
			}
			anthropicTools = append(anthropicTools, map[string]interface{}{
				"name":         getString(fn, "name"),
				"description":  getString(fn, "description"),
				"input_schema": cleanSchema(getMap(fn, "parameters")),
			})
		}
		if len(anthropicTools) > 0 {
			result["tools"] = anthropicTools
		}
	}

	// tool_choice
	if tc, ok := body["tool_choice"]; ok {
		if mapped := mapOpenAIChatToolChoiceToAnthropic(tc); mapped != nil {
			result["tool_choice"] = mapped
		}
	}

	return result, nil
}

// extractOpenAIToolContent 提取 OpenAI tool 角色消息的 content。
// content 可为字符串或数组（OpenAI 规范允许 tool 角色返回多段文本），
// 数组时拼接所有 text/input_text 段，其它类型段忽略。
func extractOpenAIToolContent(content interface{}) string {
	if s, ok := asString(content); ok {
		return s
	}
	if arr, ok := asArray(content); ok {
		var sb strings.Builder
		for _, c := range arr {
			cm, ok := asMap(c)
			if !ok {
				continue
			}
			ct := getString(cm, "type")
			if ct == "text" || ct == "input_text" || ct == "output_text" || ct == "" {
				if t, ok := asString(cm["text"]); ok {
					sb.WriteString(t)
				}
			}
		}
		return sb.String()
	}
	return ""
}

// mergeToolResultIntoLastUser 尝试将 toolResult 合并到 messages 末尾的 user 消息中。
// 仅当末尾消息是 user 角色且其 content 全部为 tool_result block 时才合并，
// 保证不会与普通文本 user 消息混淆。返回是否成功合并。
func mergeToolResultIntoLastUser(messages []interface{}, toolResult map[string]interface{}) bool {
	if len(messages) == 0 {
		return false
	}
	last, ok := asMap(messages[len(messages)-1])
	if !ok || getString(last, "role") != "user" {
		return false
	}
	blocks, ok := asArray(last["content"])
	if !ok || len(blocks) == 0 {
		return false
	}
	for _, b := range blocks {
		bm, ok := asMap(b)
		if !ok || getString(bm, "type") != "tool_result" {
			return false
		}
	}
	last["content"] = append(blocks, toolResult)
	return true
}

func convertOpenAIChatMessageToAnthropic(role string, m map[string]interface{}) map[string]interface{} {
	// assistant 消息可能同时有 content 和 tool_calls，需要都处理；
	// 非 assistant 或无 tool_calls 时字符串 content 可直接返回。
	hasToolCalls := role == "assistant" && getArray(m, "tool_calls") != nil
	if s, ok := asString(m["content"]); ok && !hasToolCalls {
		return map[string]interface{}{
			"role":    role,
			"content": s,
		}
	}

	var blocks []interface{}

	// content 为字符串（assistant 有 tool_calls 的情况）：转为 text block
	if s, ok := asString(m["content"]); ok && s != "" {
		blocks = append(blocks, map[string]interface{}{
			"type": "text",
			"text": s,
		})
	}

	// content 为数组
	if contentArr := getArray(m, "content"); contentArr != nil {
		for _, c := range contentArr {
			cm, ok := asMap(c)
			if !ok {
				continue
			}
			ct := getString(cm, "type")
			switch ct {
			case "text":
				if t, ok := asString(cm["text"]); ok {
					blocks = append(blocks, map[string]interface{}{
						"type": "text",
						"text": t,
					})
				}
			case "image_url":
				url := getString(cm, "url")
				if url == "" {
					if um, ok := asMap(cm["image_url"]); ok {
						url = getString(um, "url")
					}
				}
				if strings.HasPrefix(url, "data:") {
					// data:image/png;base64,xxxx
					semi := strings.Index(url, ";")
					comma := strings.Index(url, ",")
					if semi > 5 && comma > semi {
						mediaType := url[5:semi]
						data := url[comma+1:]
						blocks = append(blocks, map[string]interface{}{
							"type": "image",
							"source": map[string]interface{}{
								"type":       "base64",
								"media_type": mediaType,
								"data":       data,
							},
						})
					}
				}
			}
		}
	}

	// assistant with tool_calls
	if hasToolCalls {
		if toolCalls := getArray(m, "tool_calls"); toolCalls != nil {
			for _, tc := range toolCalls {
				tcm, ok := asMap(tc)
				if !ok {
					continue
				}
				fn := getMap(tcm, "function")
				if fn == nil {
					continue
				}
				argsStr := canonicalizeToolArguments(fn["arguments"])
				var input interface{}
				if err := json.Unmarshal([]byte(argsStr), &input); err != nil {
					input = map[string]interface{}{}
				}
				blocks = append(blocks, map[string]interface{}{
					"type":  "tool_use",
					"id":    getString(tcm, "id"),
					"name":  getString(fn, "name"),
					"input": input,
				})
			}
		}
	}

	if len(blocks) == 0 {
		return map[string]interface{}{
			"role":    role,
			"content": "",
		}
	}
	return map[string]interface{}{
		"role":    role,
		"content": blocks,
	}
}

func mapOpenAIChatToolChoiceToAnthropic(tc interface{}) interface{} {
	if s, ok := asString(tc); ok {
		switch s {
		case "auto":
			return map[string]interface{}{"type": "auto"}
		case "none":
			return map[string]interface{}{"type": "none"}
		case "required":
			return map[string]interface{}{"type": "any"}
		}
	}
	if m, ok := asMap(tc); ok {
		if getString(m, "type") == "function" {
			if fn, ok := asMap(m["function"]); ok {
				return map[string]interface{}{
					"type": "tool",
					"name": getString(fn, "name"),
				}
			}
		}
	}
	return nil
}

// openaiResponsesToAnthropicRequest 将 OpenAI Responses API 请求转为 Anthropic Messages 请求。
func openaiResponsesToAnthropicRequest(body map[string]interface{}) (map[string]interface{}, error) {
	result := make(map[string]interface{})
	model := getString(body, "model")
	if model != "" {
		result["model"] = model
	}

	if instructions, ok := asString(body["instructions"]); ok && instructions != "" {
		result["system"] = instructions
	}

	// input 可为数组（标准）或字符串（单条 user 消息简写），统一成数组处理
	var inputArr []interface{}
	if arr := getArray(body, "input"); arr != nil {
		inputArr = arr
	} else if s, ok := asString(body["input"]); ok && s != "" {
		inputArr = []interface{}{
			map[string]interface{}{
				"type":    "message",
				"role":    "user",
				"content": s,
			},
		}
	}
	messages := convertResponsesInputToAnthropicMessages(inputArr)
	result["messages"] = messages

	if mt, ok := body["max_output_tokens"]; ok {
		result["max_tokens"] = mt
	}
	for _, key := range []string{"temperature", "top_p", "stream"} {
		if v, ok := body[key]; ok {
			result[key] = v
		}
	}
	for _, key := range openaiExtraPassthroughFields {
		if v, ok := body[key]; ok {
			result[key] = v
		}
	}

	// reasoning_effort / reasoning.effort → thinking（反向映射）
	if supportsReasoningEffort(model) {
		maxTokens := intFromInterface(result["max_tokens"])
		if thinking := resolveAnthropicThinkingFromReasoning(body, maxTokens); thinking != nil {
			result["thinking"] = thinking
		}
	}

	// tools
	if tools := getArray(body, "tools"); tools != nil {
		var anthropicTools []interface{}
		for _, tool := range tools {
			tm, ok := asMap(tool)
			if !ok {
				continue
			}
			name := getString(tm, "name")
			if name == "" {
				continue
			}
			anthropicTools = append(anthropicTools, map[string]interface{}{
				"name":         name,
				"description":  getString(tm, "description"),
				"input_schema": cleanSchema(getMap(tm, "parameters")),
			})
		}
		if len(anthropicTools) > 0 {
			result["tools"] = anthropicTools
		}
	}

	// tool_choice
	if tc, ok := body["tool_choice"]; ok {
		if mapped := mapResponsesToolChoiceToAnthropic(tc); mapped != nil {
			result["tool_choice"] = mapped
		}
	}

	return result, nil
}

func convertResponsesInputToAnthropicMessages(input []interface{}) []interface{} {
	var messages []interface{}
	// 按 message/function_call/function_call_output 重组
	var currentAssistantBlocks []interface{}
	var currentUserBlocks []interface{}

	flushUser := func() {
		if len(currentUserBlocks) > 0 {
			messages = append(messages, map[string]interface{}{
				"role":    "user",
				"content": currentUserBlocks,
			})
			currentUserBlocks = nil
		}
	}
	flushAssistant := func() {
		if len(currentAssistantBlocks) > 0 {
			messages = append(messages, map[string]interface{}{
				"role":    "assistant",
				"content": currentAssistantBlocks,
			})
			currentAssistantBlocks = nil
		}
	}

	for _, item := range input {
		im, ok := asMap(item)
		if !ok {
			continue
		}
		itemType := getString(im, "type")
		switch itemType {
		case "message":
			role := getString(im, "role")
			if role == "assistant" {
				flushUser()
				// 提取 content
				if s, ok := asString(im["content"]); ok {
					currentAssistantBlocks = append(currentAssistantBlocks, map[string]interface{}{
						"type": "text",
						"text": s,
					})
				}
				if contentArr := getArray(im, "content"); contentArr != nil {
					for _, c := range contentArr {
						cm, ok := asMap(c)
						if !ok {
							continue
						}
						ct := getString(cm, "type")
						switch ct {
						case "output_text", "text":
							if t, ok := asString(cm["text"]); ok {
								currentAssistantBlocks = append(currentAssistantBlocks, map[string]interface{}{
									"type": "text",
									"text": t,
								})
							}
						}
					}
				}
			} else {
				flushAssistant()
				if s, ok := asString(im["content"]); ok {
					currentUserBlocks = append(currentUserBlocks, map[string]interface{}{
						"type": "text",
						"text": s,
					})
				}
				if contentArr := getArray(im, "content"); contentArr != nil {
					for _, c := range contentArr {
						cm, ok := asMap(c)
						if !ok {
							continue
						}
						ct := getString(cm, "type")
						switch ct {
						case "input_text", "text":
							if t, ok := asString(cm["text"]); ok {
								currentUserBlocks = append(currentUserBlocks, map[string]interface{}{
									"type": "text",
									"text": t,
								})
							}
						case "input_image":
							url := getString(cm, "image_url")
							if strings.HasPrefix(url, "data:") {
								semi := strings.Index(url, ";")
								comma := strings.Index(url, ",")
								if semi > 5 && comma > semi {
									mediaType := url[5:semi]
									data := url[comma+1:]
									currentUserBlocks = append(currentUserBlocks, map[string]interface{}{
										"type": "image",
										"source": map[string]interface{}{
											"type":       "base64",
											"media_type": mediaType,
											"data":       data,
										},
									})
								}
							}
						}
					}
				}
			}
		case "function_call":
			flushUser()
			argsStr := canonicalizeToolArguments(im["arguments"])
			var input interface{}
			if err := json.Unmarshal([]byte(argsStr), &input); err != nil {
				input = map[string]interface{}{}
			}
			currentAssistantBlocks = append(currentAssistantBlocks, map[string]interface{}{
				"type":  "tool_use",
				"id":    getString(im, "call_id"),
				"name":  getString(im, "name"),
				"input": input,
			})
		case "function_call_output":
			flushAssistant()
			output, _ := asString(im["output"])
			currentUserBlocks = append(currentUserBlocks, map[string]interface{}{
				"type":        "tool_result",
				"tool_use_id": getString(im, "call_id"),
				"content":     output,
			})
		}
	}
	flushUser()
	flushAssistant()
	return messages
}

func mapResponsesToolChoiceToAnthropic(tc interface{}) interface{} {
	if s, ok := asString(tc); ok {
		switch s {
		case "auto":
			return map[string]interface{}{"type": "auto"}
		case "none":
			return map[string]interface{}{"type": "none"}
		case "required":
			return map[string]interface{}{"type": "any"}
		}
	}
	if m, ok := asMap(tc); ok {
		if getString(m, "type") == "function" {
			return map[string]interface{}{
				"type": "tool",
				"name": getString(m, "name"),
			}
		}
	}
	return nil
}

// geminiToAnthropicRequest 将 Gemini generateContent 请求转为 Anthropic Messages 请求。
func geminiToAnthropicRequest(body map[string]interface{}) (map[string]interface{}, error) {
	result := make(map[string]interface{})

	// model：Gemini body 不含 model 字段（模型名在 URL path），由 TransformRequest
	// 兜底回写到 body["model"]。此处显式复制到 result，保证链式（gemini→anthropic→X）
	// 下游转换器能从 anthropic body 读到 model。
	if m := getString(body, "model"); m != "" {
		result["model"] = m
	}

	// systemInstruction → system
	if si := getMap(body, "systemInstruction"); si != nil {
		if parts := getArray(si, "parts"); parts != nil {
			var texts []string
			for _, part := range parts {
				if pm, ok := asMap(part); ok {
					if t, ok := asString(pm["text"]); ok && t != "" {
						texts = append(texts, t)
					}
				}
			}
			if len(texts) > 0 {
				result["system"] = strings.Join(texts, "\n\n")
			}
		}
	}

	// contents → messages
	if contents := getArray(body, "contents"); contents != nil {
		var messages []interface{}
		for _, c := range contents {
			cm, ok := asMap(c)
			if !ok {
				continue
			}
			role := getString(cm, "role")
			anthropicRole := "user"
			if role == "model" {
				anthropicRole = "assistant"
			}
			parts := getArray(cm, "parts")
			blocks := convertGeminiPartsToAnthropicContent(parts)
			if len(blocks) > 0 {
				messages = append(messages, map[string]interface{}{
					"role":    anthropicRole,
					"content": blocks,
				})
			}
		}
		result["messages"] = messages
	}

	// generationConfig
	if gc := getMap(body, "generationConfig"); gc != nil {
		if v, ok := gc["maxOutputTokens"]; ok {
			result["max_tokens"] = v
		}
		if v, ok := gc["temperature"]; ok {
			result["temperature"] = v
		}
		if v, ok := gc["topP"]; ok {
			result["top_p"] = v
		}
		if v, ok := gc["stopSequences"]; ok {
			result["stop_sequences"] = v
		}
	}

	// tools
	if tools := getArray(body, "tools"); tools != nil {
		var anthropicTools []interface{}
		for _, tool := range tools {
			tm, ok := asMap(tool)
			if !ok {
				continue
			}
			funcDecls := getArray(tm, "functionDeclarations")
			for _, fd := range funcDecls {
				fdm, ok := asMap(fd)
				if !ok {
					continue
				}
				var schema map[string]interface{}
				if p := getMap(fdm, "parameters"); p != nil {
					schema = p
				} else if p := getMap(fdm, "parametersJsonSchema"); p != nil {
					schema = p
				}
				if schema == nil {
					schema = map[string]interface{}{}
				}
				anthropicTools = append(anthropicTools, map[string]interface{}{
					"name":         getString(fdm, "name"),
					"description":  getString(fdm, "description"),
					"input_schema": schema,
				})
			}
		}
		if len(anthropicTools) > 0 {
			result["tools"] = anthropicTools
		}
	}

	// toolConfig
	if tc := getMap(body, "toolConfig"); tc != nil {
		if mapped := mapGeminiToolChoiceToAnthropic(tc); mapped != nil {
			result["tool_choice"] = mapped
		}
	}

	return result, nil
}

func convertGeminiPartsToAnthropicContent(parts []interface{}) []interface{} {
	var blocks []interface{}
	for _, part := range parts {
		pm, ok := asMap(part)
		if !ok {
			continue
		}
		// text
		if t, ok := asString(pm["text"]); ok && t != "" {
			blocks = append(blocks, map[string]interface{}{
				"type": "text",
				"text": t,
			})
			continue
		}
		// inlineData
		if id := getMap(pm, "inlineData"); id != nil {
			mediaType := getString(id, "mimeType")
			data := getString(id, "data")
			if data != "" {
				blocks = append(blocks, map[string]interface{}{
					"type": "image",
					"source": map[string]interface{}{
						"type":       "base64",
						"media_type": mediaType,
						"data":       data,
					},
				})
			}
			continue
		}
		// functionCall
		if fc := getMap(pm, "functionCall"); fc != nil {
			blocks = append(blocks, map[string]interface{}{
				"type":  "tool_use",
				"id":    getString(fc, "id"),
				"name":  getString(fc, "name"),
				"input": ensureArgsMap(fc["args"]),
			})
			continue
		}
		// functionResponse
		if fr := getMap(pm, "functionResponse"); fr != nil {
			resp := fr["response"]
			var contentStr string
			if s, ok := asString(resp); ok {
				contentStr = s
			} else {
				contentStr = canonicalJSONString(resp)
			}
			blocks = append(blocks, map[string]interface{}{
				"type":        "tool_result",
				"tool_use_id": getString(fr, "id"),
				"content":     contentStr,
			})
		}
	}
	return blocks
}

func mapGeminiToolChoiceToAnthropic(tc map[string]interface{}) interface{} {
	mode := getString(tc, "mode")
	switch mode {
	case "AUTO":
		return map[string]interface{}{"type": "auto"}
	case "NONE":
		return map[string]interface{}{"type": "none"}
	case "ANY":
		names := getArray(tc, "allowedFunctionNames")
		if len(names) == 1 {
			if name, ok := asString(names[0]); ok {
				return map[string]interface{}{
					"type": "tool",
					"name": name,
				}
			}
		}
		return map[string]interface{}{"type": "any"}
	}
	return nil
}

// ============================================================================
// 逆向响应转换器：Anthropic → X（新写）
// ============================================================================

// anthropicToOpenAIChatResponse 将 Anthropic Messages 响应转为 OpenAI Chat 响应。
func anthropicToOpenAIChatResponse(body map[string]interface{}) (map[string]interface{}, error) {
	id := getString(body, "id")
	model := getString(body, "model")
	content := getArray(body, "content")

	var textContent string
	var toolCalls []interface{}
	for _, block := range content {
		bm, ok := asMap(block)
		if !ok {
			continue
		}
		blockType := getString(bm, "type")
		switch blockType {
		case "text":
			if t, ok := asString(bm["text"]); ok {
				textContent += t
			}
		case "tool_use":
			args := canonicalizeToolArguments(bm["input"])
			toolCalls = append(toolCalls, map[string]interface{}{
				"id":   getString(bm, "id"),
				"type": "function",
				"function": map[string]interface{}{
					"name":      getString(bm, "name"),
					"arguments": args,
				},
			})
		}
	}

	stopReason := getString(body, "stop_reason")
	finishReason := mapAnthropicStopReasonToOpenAI(stopReason)

	message := map[string]interface{}{
		"role":    "assistant",
		"content": textContent,
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}

	result := map[string]interface{}{
		"id":     id,
		"object": "chat.completion",
		"model":  model,
		"choices": []interface{}{
			map[string]interface{}{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
		"usage": buildOpenAIUsageFromAnthropic(getMap(body, "usage")),
	}
	return result, nil
}

// anthropicToOpenAIResponsesResponse 将 Anthropic Messages 响应转为 OpenAI Responses 响应。
func anthropicToOpenAIResponsesResponse(body map[string]interface{}) (map[string]interface{}, error) {
	id := getString(body, "id")
	model := getString(body, "model")
	content := getArray(body, "content")

	var output []interface{}
	for _, block := range content {
		bm, ok := asMap(block)
		if !ok {
			continue
		}
		blockType := getString(bm, "type")
		switch blockType {
		case "text":
			if t, ok := asString(bm["text"]); ok && t != "" {
				output = append(output, map[string]interface{}{
					"type": "message",
					"role": "assistant",
					"content": []interface{}{
						map[string]interface{}{
							"type": "output_text",
							"text": t,
						},
					},
				})
			}
		case "tool_use":
			args := canonicalizeToolArguments(bm["input"])
			output = append(output, map[string]interface{}{
				"type":      "function_call",
				"call_id":   getString(bm, "id"),
				"name":      getString(bm, "name"),
				"arguments": args,
			})
		}
	}

	stopReason := getString(body, "stop_reason")
	status := "completed"
	if stopReason == "max_tokens" {
		status = "incomplete"
	}

	result := map[string]interface{}{
		"id":     id,
		"object": "response",
		"model":  model,
		"output": output,
		"status": status,
		"usage":  buildResponsesUsageFromAnthropic(getMap(body, "usage")),
	}
	return result, nil
}

// anthropicToGeminiResponse 将 Anthropic Messages 响应转为 Gemini GenerateContent 响应。
func anthropicToGeminiResponse(body map[string]interface{}) (map[string]interface{}, error) {
	content := getArray(body, "content")
	var parts []interface{}
	for _, block := range content {
		bm, ok := asMap(block)
		if !ok {
			continue
		}
		blockType := getString(bm, "type")
		switch blockType {
		case "text":
			if t, ok := asString(bm["text"]); ok && t != "" {
				parts = append(parts, map[string]interface{}{"text": t})
			}
		case "tool_use":
			fc := map[string]interface{}{
				"name": getString(bm, "name"),
				"args": ensureArgsMap(bm["input"]),
			}
			id := getString(bm, "id")
			if id != "" && !isSynthesizedToolCallID(id) {
				fc["id"] = id
			}
			parts = append(parts, map[string]interface{}{
				"functionCall": fc,
			})
		}
	}

	finishReason := "STOP"
	stopReason := getString(body, "stop_reason")
	switch stopReason {
	case "max_tokens":
		finishReason = "MAX_TOKENS"
	case "end_turn", "stop_sequence":
		finishReason = "STOP"
	case "tool_use":
		finishReason = "STOP"
	}

	result := map[string]interface{}{
		"candidates": []interface{}{
			map[string]interface{}{
				"content": map[string]interface{}{
					"parts": parts,
				},
				"finishReason": finishReason,
			},
		},
		"usageMetadata": buildGeminiUsageFromAnthropic(getMap(body, "usage")),
	}
	return result, nil
}

// ============================================================================
// 合成工具调用 ID（Gemini 无状态模式）
// ============================================================================

const synthesizedIDPrefix = "gemini_synth_"

// synthesizedIDLen 是合成 ID 的固定总长度：前缀 + 16 字节 hex（32 字符）。
// isSynthesizedToolCallID 同时校验前缀与长度，避免真实 Gemini ID 偶以同前缀开头时被误判。
const synthesizedIDLen = len(synthesizedIDPrefix) + 32

func synthesizeToolCallID() string {
	return synthesizedIDPrefix + randomHex(16)
}

func isSynthesizedToolCallID(id string) bool {
	return strings.HasPrefix(id, synthesizedIDPrefix) && len(id) == synthesizedIDLen
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// rand.Read 极少失败；fallback 用确定性序列保证不 panic
		for i := range b {
			b[i] = byte(0x41 + (i * 7 % 26))
		}
	}
	return fmt.Sprintf("%x", b)
}

// ============================================================================
// 转换入口
// ============================================================================

// TransformOptions 控制请求转换的可选行为。
type TransformOptions struct {
	// PreserveReasoningContent=true 时，Anthropic→OpenAI Chat 转换将 thinking 块
	// 文本提取为 reasoning_content 字段（Kimi/DeepSeek/MiMo 等 reasoning vendor 需要）。
	PreserveReasoningContent bool
}

// TransformRequest 转换请求体。inFormat=客户端格式，outFormat=目标上游格式。
// 返回转换后的 body、新 model（可能改变）、可能的错误。
func TransformRequest(inFormat, outFormat string, body []byte, model string, isStream bool) ([]byte, string, error) {
	return TransformRequestWithOptions(inFormat, outFormat, body, model, isStream, TransformOptions{})
}

// TransformRequestWithOptions 同 TransformRequest，但接受 TransformOptions。
func TransformRequestWithOptions(inFormat, outFormat string, body []byte, model string, isStream bool, opts TransformOptions) ([]byte, string, error) {
	if !needsTransform(inFormat, outFormat) {
		return body, model, nil
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, "", fmt.Errorf("parse request body: %w", err)
	}
	if parsed == nil {
		parsed = map[string]interface{}{}
	}

	// 归一化 model：Gemini 风格请求的模型名位于 URL path（由调用方传入 model 参数），
	// body 中无 model 字段。此处兜底回写，确保下游转换器（含链式 anthropic 中转）
	// 能从 body["model"] 读到模型名，避免发往上游的请求体缺失 model。
	if m, ok := asString(parsed["model"]); !ok || m == "" {
		if model != "" {
			parsed["model"] = model
		}
	}

	result, err := transformRequestBody(inFormat, outFormat, parsed, opts.PreserveReasoningContent)
	if err != nil {
		return nil, "", err
	}

	// 提取 model
	newModel := model
	if m, ok := asString(result["model"]); ok && m != "" {
		newModel = m
	}

	// 对 gemini 目标，stream 不在 body 中而是 URL 参数
	if outFormat == formatGemini {
		delete(result, "stream")
	}

	out, err := json.Marshal(result)
	if err != nil {
		return nil, "", fmt.Errorf("marshal transformed request: %w", err)
	}
	return out, newModel, nil
}

func transformRequestBody(inFormat, outFormat string, body map[string]interface{}, preserveReasoningContent bool) (map[string]interface{}, error) {
	// 直接转换
	if inFormat == formatAnthropic {
		return anthropicToXRequest(outFormat, body, preserveReasoningContent)
	}
	if outFormat == formatAnthropic {
		return xToAnthropicRequest(inFormat, body)
	}
	// 链式：inFormat → anthropic → outFormat
	anthropicBody, err := xToAnthropicRequest(inFormat, body)
	if err != nil {
		return nil, fmt.Errorf("chain %s→anthropic: %w", inFormat, err)
	}
	return anthropicToXRequest(outFormat, anthropicBody, preserveReasoningContent)
}

func anthropicToXRequest(outFormat string, body map[string]interface{}, preserveReasoningContent bool) (map[string]interface{}, error) {
	switch outFormat {
	case formatOpenAIChat:
		return anthropicToOpenAIChatRequest(body, preserveReasoningContent)
	case formatOpenAIResponses:
		return anthropicToOpenAIResponsesRequest(body)
	case formatGemini:
		return anthropicToGeminiRequest(body)
	}
	return body, nil
}

func xToAnthropicRequest(inFormat string, body map[string]interface{}) (map[string]interface{}, error) {
	switch inFormat {
	case formatOpenAIChat:
		return openaiChatToAnthropicRequest(body)
	case formatOpenAIResponses:
		return openaiResponsesToAnthropicRequest(body)
	case formatGemini:
		return geminiToAnthropicRequest(body)
	}
	return body, nil
}

// TransformResponse 转换响应体。inFormat=客户端格式，outFormat=目标上游格式。
// 转换方向：outFormat（上游响应）→ inFormat（客户端期望）。
func TransformResponse(inFormat, outFormat string, body []byte) ([]byte, error) {
	if !needsTransform(inFormat, outFormat) {
		return body, nil
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse response body: %w", err)
	}
	if parsed == nil {
		return body, nil
	}

	result, err := transformResponseBody(inFormat, outFormat, parsed)
	if err != nil {
		return nil, err
	}

	out, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("marshal transformed response: %w", err)
	}
	return out, nil
}

func transformResponseBody(inFormat, outFormat string, body map[string]interface{}) (map[string]interface{}, error) {
	// 上游响应是 outFormat，需转为 inFormat
	// 如果 inFormat == anthropic：x→anthropic（port）
	// 如果 outFormat == anthropic：anthropic→x（inverse）
	// 否则链式：outFormat→anthropic→inFormat
	if inFormat == formatAnthropic {
		return xToAnthropicResponse(outFormat, body)
	}
	if outFormat == formatAnthropic {
		return anthropicToXResponse(inFormat, body)
	}
	// 链式
	anthropicResp, err := xToAnthropicResponse(outFormat, body)
	if err != nil {
		return nil, fmt.Errorf("chain %s→anthropic: %w", outFormat, err)
	}
	return anthropicToXResponse(inFormat, anthropicResp)
}

// TransformErrorResponse 转换错误响应体。inFormat=客户端格式，outFormat=上游格式。
// statusCode 为上游 HTTP 状态码，用于 Gemini 错误 body 的 code 字段与 gRPC status 映射；
// 传 0 时 Gemini code 字段兜底为 500。
// 上游错误响应结构各异（OpenAI/Anthropic/Gemini 各有不同 error 包装），
// 此函数提取 message 后按客户端期望格式重建，避免客户端解析失败。
// 解析失败或无法提取 message 时原样返回。
func TransformErrorResponse(inFormat, outFormat string, body []byte, statusCode int) []byte {
	if !needsTransform(inFormat, outFormat) {
		return body
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body
	}
	if parsed == nil {
		return body
	}

	msg, errType := extractErrorMessage(parsed, outFormat)
	if msg == "" {
		return body
	}

	result := buildErrorForFormat(inFormat, msg, errType, statusCode)
	out, err := json.Marshal(result)
	if err != nil {
		return body
	}
	return out
}

// extractErrorMessage 从错误响应中提取 message 和 type。
// outFormat 决定解析哪种错误结构。
func extractErrorMessage(body map[string]interface{}, outFormat string) (message, errType string) {
	switch outFormat {
	case formatAnthropic:
		// {"type":"error","error":{"type":"...","message":"..."}}
		if e, ok := asMap(body["error"]); ok {
			message, _ = asString(e["message"])
			errType, _ = asString(e["type"])
		}
	case formatOpenAIChat, formatOpenAIResponses:
		// {"error":{"message":"...","type":"...","code":"..."}}
		if e, ok := asMap(body["error"]); ok {
			message, _ = asString(e["message"])
			errType, _ = asString(e["type"])
		}
	case formatGemini:
		// {"error":{"code":500,"message":"...","status":"..."}}
		if e, ok := asMap(body["error"]); ok {
			message, _ = asString(e["message"])
			errType, _ = asString(e["status"])
		}
	}
	return
}

// buildErrorForFormat 按目标格式构建错误响应。statusCode 为上游 HTTP 状态码，
// 用于 Gemini 错误 body 的 code 字段与 gRPC status 映射；为 0 时兜底为 500。
// errType 来自上游错误体（如 OpenAI 的 type、Gemini 的 status、Anthropic 的 type），
// 为空时按 inFormat 给定默认值。
func buildErrorForFormat(inFormat, message, errType string, statusCode int) map[string]interface{} {
	switch inFormat {
	case formatAnthropic:
		if errType == "" {
			errType = "api_error"
		}
		return map[string]interface{}{
			"type":  "error",
			"error": map[string]interface{}{"type": errType, "message": message},
		}
	case formatOpenAIChat, formatOpenAIResponses:
		if errType == "" {
			errType = "api_error"
		}
		return map[string]interface{}{
			"error": map[string]interface{}{"message": message, "type": errType},
		}
	case formatGemini:
		if statusCode == 0 {
			statusCode = 500
		}
		// Gemini 错误 body 的 status 字段必须是 gRPC Code 大写名（如 INVALID_ARGUMENT）。
		// 上游 errType（Anthropic/OpenAI 的 type 字段）不是 gRPC status，故始终按
		// HTTP status 派生，保证客户端按 status 路由时与 HTTP 语义一致。
		return map[string]interface{}{
			"error": map[string]interface{}{
				"code":    statusCode,
				"message": message,
				"status":  httpStatusToGRPCStatus(statusCode),
			},
		}
	}
	return map[string]interface{}{"error": map[string]interface{}{"message": message}}
}

// httpStatusToGRPCStatus 把 HTTP 状态码映射为 Gemini 错误 body 期望的 gRPC status
// 字符串。Gemini 上游错误 body 的 status 字段使用 gRPC Code 大写名（如 INVALID_ARGUMENT），
// 直接复用上游 HTTP status 可让客户端按 status 字段路由时与 HTTP 语义一致。
// 未识别的 status 兜底为 INTERNAL。
func httpStatusToGRPCStatus(statusCode int) string {
	switch {
	case statusCode >= 400 && statusCode < 500:
		switch statusCode {
		case 400:
			return "INVALID_ARGUMENT"
		case 401:
			return "UNAUTHENTICATED"
		case 403:
			return "PERMISSION_DENIED"
		case 404:
			return "NOT_FOUND"
		case 409:
			return "ABORTED"
		case 429:
			return "RESOURCE_EXHAUSTED"
		}
		return "FAILED_PRECONDITION"
	case statusCode == 500:
		return "INTERNAL"
	case statusCode == 501:
		return "UNIMPLEMENTED"
	case statusCode == 502, statusCode == 503:
		return "UNAVAILABLE"
	case statusCode == 504:
		return "DEADLINE_EXCEEDED"
	case statusCode >= 500:
		return "INTERNAL"
	}
	return "INTERNAL"
}

func xToAnthropicResponse(outFormat string, body map[string]interface{}) (map[string]interface{}, error) {
	switch outFormat {
	case formatOpenAIChat:
		return openaiChatToAnthropicResponse(body)
	case formatOpenAIResponses:
		return openaiResponsesToAnthropicResponse(body)
	case formatGemini:
		return geminiToAnthropicResponse(body)
	}
	return body, nil
}

func anthropicToXResponse(inFormat string, body map[string]interface{}) (map[string]interface{}, error) {
	switch inFormat {
	case formatOpenAIChat:
		return anthropicToOpenAIChatResponse(body)
	case formatOpenAIResponses:
		return anthropicToOpenAIResponsesResponse(body)
	case formatGemini:
		return anthropicToGeminiResponse(body)
	}
	return body, nil
}

// ============================================================================
// Auth 头切换
// ============================================================================

// swapAuthForTarget 按目标格式重置 auth 注入方式。
func swapAuthForTarget(outHeaders http.Header, query url.Values, realToken, targetFormat string) {
	// 清除所有 auth 相关头
	outHeaders.Del("Authorization")
	outHeaders.Del("X-Goog-Api-Key")
	outHeaders.Del("X-Api-Key")
	query.Del("key")

	switch targetFormat {
	case formatAnthropic:
		outHeaders.Set("X-Api-Key", realToken)
		// Anthropic API 强制要求 anthropic-version 头；客户端未提供时填默认值
		if outHeaders.Get("anthropic-version") == "" {
			outHeaders.Set("anthropic-version", "2023-06-01")
		}
	case formatOpenAIChat, formatOpenAIResponses:
		outHeaders.Set("Authorization", "Bearer "+realToken)
	case formatGemini:
		outHeaders.Set("X-Goog-Api-Key", realToken)
		query.Set("key", realToken)
	}
}
