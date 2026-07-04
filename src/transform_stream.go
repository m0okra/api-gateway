package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"
)

// ============================================================================
// SSE 解析原语（port 自 cc-switch proxy/sse.rs）
// ============================================================================

// stripSSEField 从行中提取 "field:" 或 "field: " 后的值。
func stripSSEField(line, field string) (string, bool) {
	prefixSpace := field + ": "
	if strings.HasPrefix(line, prefixSpace) {
		return line[len(prefixSpace):], true
	}
	prefixNoSpace := field + ":"
	if strings.HasPrefix(line, prefixNoSpace) {
		return line[len(prefixNoSpace):], true
	}
	return "", false
}

// takeSSEBlock 从 buffer 中取出一个完整的 SSE 事件块（以 \n\n 或 \r\n\r\n 分隔）。
// 返回块内容和是否找到。找到时从 buffer 中移除块和分隔符。
func takeSSEBlock(buffer *bytes.Buffer) (string, bool) {
	buf := buffer.Bytes()
	var bestPos, bestLen int = -1, 0

	// 查找最早出现的分隔符
	for _, sep := range []struct {
		s string
		l int
	}{{"\r\n\r\n", 4}, {"\n\n", 2}} {
		if idx := bytes.Index(buf, []byte(sep.s)); idx >= 0 {
			if bestPos < 0 || idx < bestPos {
				bestPos = idx
				bestLen = sep.l
			}
		}
	}

	if bestPos < 0 {
		return "", false
	}

	block := string(buf[:bestPos])
	buffer.Next(bestPos + bestLen)
	return block, true
}

// appendUTF8Safe 将 chunk 追加到 buffer，只保留完整的 UTF-8 字符。
// 返回尾部不完整字节（留待下次拼接），防止多字节字符被 TCP chunk 边界拆分
// 导致 JSON 参数损坏。
func appendUTF8Safe(buf *bytes.Buffer, chunk []byte) []byte {
	if len(chunk) == 0 {
		return nil
	}
	valid := len(chunk)
	for valid > 0 && !utf8.Valid(chunk[:valid]) {
		valid--
	}
	if valid > 0 {
		buf.Write(chunk[:valid])
	}
	return chunk[valid:]
}

// ============================================================================
// StreamConverter 接口
// ============================================================================

// StreamConverter 将一种格式的 SSE 流转换为另一种格式。
type StreamConverter interface {
	Feed(chunk []byte) ([]byte, error)
	Flush() ([]byte, error)
}

// NewStreamConverter 创建流式转换器。inFormat=客户端格式，outFormat=目标上游格式。
// 转换方向：outFormat SSE → inFormat SSE（上游响应→客户端）。
func NewStreamConverter(inFormat, outFormat string) StreamConverter {
	if !needsTransform(inFormat, outFormat) {
		return &passthroughStreamConverter{}
	}

	// 如果 inFormat == anthropic：x→anthropic（port）
	if inFormat == formatAnthropic {
		return newxToAnthropicStream(outFormat)
	}
	// 如果 outFormat == anthropic：anthropic→x（inverse）
	if outFormat == formatAnthropic {
		return newAnthropicToXStream(inFormat)
	}
	// 链式：outFormat→anthropic→inFormat
	first := newxToAnthropicStream(outFormat)
	second := newAnthropicToXStream(inFormat)
	return &chainedStreamConverter{first: first, second: second}
}

// passthroughStreamConverter 不做任何转换。
type passthroughStreamConverter struct{}

func (p *passthroughStreamConverter) Feed(chunk []byte) ([]byte, error) { return chunk, nil }
func (p *passthroughStreamConverter) Flush() ([]byte, error)            { return nil, nil }

// chainedStreamConverter 串联两个转换器（经 anthropic pivot）。
type chainedStreamConverter struct {
	first  StreamConverter
	second StreamConverter
}

func (c *chainedStreamConverter) Feed(chunk []byte) ([]byte, error) {
	intermediate, err := c.first.Feed(chunk)
	if err != nil {
		return nil, err
	}
	if len(intermediate) == 0 {
		return nil, nil
	}
	return c.second.Feed(intermediate)
}

func (c *chainedStreamConverter) Flush() ([]byte, error) {
	firstOut, err := c.first.Flush()
	if err != nil {
		return nil, err
	}
	var allOut []byte
	if len(firstOut) > 0 {
		secondOut, err := c.second.Feed(firstOut)
		if err != nil {
			return nil, err
		}
		allOut = append(allOut, secondOut...)
	}
	finalOut, err := c.second.Flush()
	if err != nil {
		return nil, err
	}
	return append(allOut, finalOut...), nil
}

func newxToAnthropicStream(outFormat string) StreamConverter {
	switch outFormat {
	case formatOpenAIChat:
		return &openaiChatToAnthropicStream{}
	case formatOpenAIResponses:
		return &openaiResponsesToAnthropicStream{}
	case formatGemini:
		return &geminiToAnthropicStream{}
	}
	return &passthroughStreamConverter{}
}

func newAnthropicToXStream(inFormat string) StreamConverter {
	switch inFormat {
	case formatOpenAIChat:
		return &anthropicToOpenAIChatStream{}
	case formatOpenAIResponses:
		return &anthropicToOpenAIResponsesStream{}
	case formatGemini:
		return &anthropicToGeminiStream{}
	}
	return &passthroughStreamConverter{}
}

// ============================================================================
// SSE 输出辅助
// ============================================================================

func sseEvent(eventType, data string) []byte {
	return []byte("event: " + eventType + "\ndata: " + data + "\n\n")
}

func sseData(data string) []byte {
	return []byte("data: " + data + "\n\n")
}

func mustJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// sortedIntKeys 返回 map[int]bool 的键升序切片，用于按确定顺序发送 content_block_stop
// 等事件，避免 map 迭代乱序导致事件顺序与 content_block_start 不匹配。
func sortedIntKeys(m map[int]bool) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}

// parseSSEBlock 解析单个 SSE 事件块，返回 event 类型与 data。
// 多行 data: 按 SSE 规范以 \n 拼接（主流厂商只发单行 JSON，行为兼容）。
func parseSSEBlock(block string) (eventType, data string) {
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimRight(line, "\r")
		if e, ok := stripSSEField(line, "event"); ok {
			eventType = strings.TrimSpace(e)
		} else if d, ok := stripSSEField(line, "data"); ok {
			if data == "" {
				data = d
			} else {
				data += "\n" + d
			}
		}
	}
	return eventType, data
}

// ============================================================================
// OpenAI Chat SSE → Anthropic SSE（port 自 streaming.rs）
// ============================================================================

// infiniteWhitespaceThreshold 是工具调用 arguments 流中允许的连续空白字符上限。
// 超过该阈值视为上游异常（卡死/吐空格），中止该 tool block 以防客户端无限等待。
const infiniteWhitespaceThreshold = 500

type toolBlockState struct {
	anthropicIndex        int
	id                    string
	name                  string
	started               bool
	consecutiveWhitespace int
	aborted               bool
}

// feedToolArgs 处理 OpenAI Chat/Responses 流式 arguments 增量。
// 检测连续空白异常：若超过 infiniteWhitespaceThreshold 则标记 block.aborted=true，
// 后续 delta 不再转发，避免某些上游在 tool 调用陷入循环时空吐空白导致客户端挂起。
// 返回值：
//   - "emit": 本次 delta 正常发射
//   - "abort": 本次触发了中止（首次），调用方应发射 content_block_stop 关闭 block
//   - "skip": 已中止，调用方跳过，不发射任何事件
func (b *toolBlockState) feedToolArgs(args string) string {
	if b.aborted {
		return "skip"
	}
	for _, r := range args {
		switch r {
		case ' ', '\t', '\n', '\r':
			b.consecutiveWhitespace++
			if b.consecutiveWhitespace > infiniteWhitespaceThreshold {
				b.aborted = true
				return "abort"
			}
		default:
			b.consecutiveWhitespace = 0
		}
	}
	return "emit"
}

type openaiChatToAnthropicStream struct {
	buffer               bytes.Buffer
	utf8Remainder        []byte
	messageID            string
	model                string
	nextContentIndex     int
	hasSentMessageStart  bool
	hasEmittedDelta      bool
	pendingStopReason    string
	pendingUsage         map[string]interface{}
	hasSentStop          bool
	openTextBlockIndex   *int
	openThinkingBlockIdx *int
	toolBlocks           map[int]*toolBlockState
	openToolIndices      map[int]bool
}

func (s *openaiChatToAnthropicStream) Feed(chunk []byte) ([]byte, error) {
	if len(s.utf8Remainder) > 0 {
		chunk = append(s.utf8Remainder, chunk...)
		s.utf8Remainder = nil
	}
	s.utf8Remainder = appendUTF8Safe(&s.buffer, chunk)
	var output []byte

	for {
		block, ok := takeSSEBlock(&s.buffer)
		if !ok {
			break
		}
		if strings.TrimSpace(block) == "" {
			continue
		}

		// 提取 data: 行（多行按 SSE 规范以 \n 拼接）
		_, dataStr := parseSSEBlock(block)
		if dataStr == "" {
			continue
		}

		if strings.TrimSpace(dataStr) == "[DONE]" {
			out := s.handleDone()
			output = append(output, out...)
			continue
		}

		var chunkData map[string]interface{}
		if err := json.Unmarshal([]byte(dataStr), &chunkData); err != nil {
			continue
		}

		out := s.handleChunk(chunkData)
		output = append(output, out...)
	}
	return output, nil
}

func (s *openaiChatToAnthropicStream) Flush() ([]byte, error) {
	var output []byte
	// 处理 buffer 残留
	if s.buffer.Len() > 0 {
		block := s.buffer.String()
		s.buffer.Reset()
		if strings.TrimSpace(block) != "" {
			for _, line := range strings.Split(block, "\n") {
				if d, ok := stripSSEField(strings.TrimRight(line, "\r"), "data"); ok {
					if strings.TrimSpace(d) == "[DONE]" {
						output = append(output, s.handleDone()...)
					} else {
						var chunkData map[string]interface{}
						if json.Unmarshal([]byte(d), &chunkData) == nil {
							output = append(output, s.handleChunk(chunkData)...)
						}
					}
				}
			}
		}
	}
	if !s.hasSentStop {
		output = append(output, s.handleDone()...)
	}
	return output, nil
}

func (s *openaiChatToAnthropicStream) ensureMessageStart(output *[]byte) {
	if !s.hasSentMessageStart {
		s.hasSentMessageStart = true
		event := map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"id":    s.messageID,
				"type":  "message",
				"role":  "assistant",
				"model": s.model,
				"usage": map[string]interface{}{"input_tokens": 0, "output_tokens": 0},
			},
		}
		*output = append(*output, sseEvent("message_start", mustJSON(event))...)
	}
}

func (s *openaiChatToAnthropicStream) handleChunk(chunk map[string]interface{}) []byte {
	var output []byte

	if id, ok := asString(chunk["id"]); ok && s.messageID == "" {
		s.messageID = id
	}
	if m, ok := asString(chunk["model"]); ok && s.model == "" {
		s.model = m
	}

	if s.toolBlocks == nil {
		s.toolBlocks = make(map[int]*toolBlockState)
	}
	if s.openToolIndices == nil {
		s.openToolIndices = make(map[int]bool)
	}

	choices := getArray(chunk, "choices")
	for _, c := range choices {
		choice, ok := asMap(c)
		if !ok {
			continue
		}
		delta := getMap(choice, "delta")
		if delta == nil {
			delta = map[string]interface{}{}
		}

		// reasoning / reasoning_content（DeepSeek/Kimi/o1 等 thinking 流）
		reasoning := ""
		if r, ok := asString(delta["reasoning"]); ok && r != "" {
			reasoning = r
		} else if r, ok := asString(delta["reasoning_content"]); ok && r != "" {
			reasoning = r
		}
		if reasoning != "" {
			s.ensureMessageStart(&output)
			// thinking 与 text 互斥：先关闭已打开的 text block
			if s.openTextBlockIndex != nil {
				output = append(output, sseEvent("content_block_stop", mustJSON(map[string]interface{}{
					"type": "content_block_stop", "index": *s.openTextBlockIndex,
				}))...)
				s.openTextBlockIndex = nil
			}
			if s.openThinkingBlockIdx == nil {
				idx := s.nextContentIndex
				s.nextContentIndex++
				s.openThinkingBlockIdx = &idx
				output = append(output, sseEvent("content_block_start", mustJSON(map[string]interface{}{
					"type":          "content_block_start",
					"index":         idx,
					"content_block": map[string]interface{}{"type": "thinking", "thinking": ""},
				}))...)
			}
			output = append(output, sseEvent("content_block_delta", mustJSON(map[string]interface{}{
				"type":  "content_block_delta",
				"index": *s.openThinkingBlockIdx,
				"delta": map[string]interface{}{"type": "thinking_delta", "thinking": reasoning},
			}))...)
		}

		// text content
		if content, ok := asString(delta["content"]); ok && content != "" {
			s.ensureMessageStart(&output)
			// thinking 与 text 互斥：先关闭已打开的 thinking block
			if s.openThinkingBlockIdx != nil {
				output = append(output, sseEvent("content_block_stop", mustJSON(map[string]interface{}{
					"type": "content_block_stop", "index": *s.openThinkingBlockIdx,
				}))...)
				s.openThinkingBlockIdx = nil
			}
			if s.openTextBlockIndex == nil {
				idx := s.nextContentIndex
				s.nextContentIndex++
				s.openTextBlockIndex = &idx
				startEvent := map[string]interface{}{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]interface{}{
						"type": "text",
						"text": "",
					},
				}
				output = append(output, sseEvent("content_block_start", mustJSON(startEvent))...)
			}
			deltaEvent := map[string]interface{}{
				"type":  "content_block_delta",
				"index": *s.openTextBlockIndex,
				"delta": map[string]interface{}{
					"type": "text_delta",
					"text": content,
				},
			}
			output = append(output, sseEvent("content_block_delta", mustJSON(deltaEvent))...)
		}

		// tool calls
		if toolCalls := getArray(delta, "tool_calls"); toolCalls != nil {
			// 关闭已打开的 text block
			if s.openTextBlockIndex != nil {
				stopEvent := map[string]interface{}{
					"type":  "content_block_stop",
					"index": *s.openTextBlockIndex,
				}
				output = append(output, sseEvent("content_block_stop", mustJSON(stopEvent))...)
				s.openTextBlockIndex = nil
			}
			// 关闭已打开的 thinking block（thinking 与 tool_use 互斥）
			if s.openThinkingBlockIdx != nil {
				output = append(output, sseEvent("content_block_stop", mustJSON(map[string]interface{}{
					"type": "content_block_stop", "index": *s.openThinkingBlockIdx,
				}))...)
				s.openThinkingBlockIdx = nil
			}

			for _, tc := range toolCalls {
				tcm, ok := asMap(tc)
				if !ok {
					continue
				}
				idx := intFromInterface(tcm["index"])
				fn := getMap(tcm, "function")
				if fn == nil {
					fn = map[string]interface{}{}
				}

				block, exists := s.toolBlocks[idx]
				if !exists {
					block = &toolBlockState{
						anthropicIndex: s.nextContentIndex,
					}
					s.nextContentIndex++
					s.toolBlocks[idx] = block
					s.openToolIndices[block.anthropicIndex] = true
				}

				if id, ok := asString(tcm["id"]); ok && id != "" {
					block.id = id
				}
				if name, ok := asString(fn["name"]); ok && name != "" {
					block.name = name
				}

				if !block.started {
					block.started = true
					s.ensureMessageStart(&output)
					startEvent := map[string]interface{}{
						"type":  "content_block_start",
						"index": block.anthropicIndex,
						"content_block": map[string]interface{}{
							"type":  "tool_use",
							"id":    block.id,
							"name":  block.name,
							"input": map[string]interface{}{},
						},
					}
					output = append(output, sseEvent("content_block_start", mustJSON(startEvent))...)
				}

				if args, ok := asString(fn["arguments"]); ok && args != "" {
					switch block.feedToolArgs(args) {
					case "abort":
						stopEvent := map[string]interface{}{
							"type":  "content_block_stop",
							"index": block.anthropicIndex,
						}
						output = append(output, sseEvent("content_block_stop", mustJSON(stopEvent))...)
						delete(s.openToolIndices, block.anthropicIndex)
					case "emit":
						deltaEvent := map[string]interface{}{
							"type":  "content_block_delta",
							"index": block.anthropicIndex,
							"delta": map[string]interface{}{
								"type":         "input_json_delta",
								"partial_json": args,
							},
						}
						output = append(output, sseEvent("content_block_delta", mustJSON(deltaEvent))...)
					case "skip":
						// 已中止，跳过
					}
				}
			}
		}

		// deprecated function_call (single function call without tool_calls array)
		if fnCall := getMap(delta, "function_call"); fnCall != nil {
			// 关闭已打开的 text block
			if s.openTextBlockIndex != nil {
				stopEvent := map[string]interface{}{
					"type":  "content_block_stop",
					"index": *s.openTextBlockIndex,
				}
				output = append(output, sseEvent("content_block_stop", mustJSON(stopEvent))...)
				s.openTextBlockIndex = nil
			}

			const fnIndex = 0
			block, exists := s.toolBlocks[fnIndex]
			if !exists {
				block = &toolBlockState{
					anthropicIndex: s.nextContentIndex,
				}
				s.nextContentIndex++
				s.toolBlocks[fnIndex] = block
				s.openToolIndices[block.anthropicIndex] = true
			}

			if block.id == "" {
				block.id = "fn_" + s.messageID
			}
			if name, ok := asString(fnCall["name"]); ok && name != "" {
				block.name = name
			}

			if !block.started {
				block.started = true
				s.ensureMessageStart(&output)
				startEvent := map[string]interface{}{
					"type":  "content_block_start",
					"index": block.anthropicIndex,
					"content_block": map[string]interface{}{
						"type":  "tool_use",
						"id":    block.id,
						"name":  block.name,
						"input": map[string]interface{}{},
					},
				}
				output = append(output, sseEvent("content_block_start", mustJSON(startEvent))...)
			}

			if args, ok := asString(fnCall["arguments"]); ok && args != "" {
				switch block.feedToolArgs(args) {
				case "abort":
					stopEvent := map[string]interface{}{
						"type":  "content_block_stop",
						"index": block.anthropicIndex,
					}
					output = append(output, sseEvent("content_block_stop", mustJSON(stopEvent))...)
					delete(s.openToolIndices, block.anthropicIndex)
				case "emit":
					deltaEvent := map[string]interface{}{
						"type":  "content_block_delta",
						"index": block.anthropicIndex,
						"delta": map[string]interface{}{
							"type":         "input_json_delta",
							"partial_json": args,
						},
					}
					output = append(output, sseEvent("content_block_delta", mustJSON(deltaEvent))...)
				case "skip":
					// 已中止，跳过
				}
			}
		}

		// finish_reason
		if fr, ok := asString(choice["finish_reason"]); ok && fr != "" && !s.hasEmittedDelta {
			s.hasEmittedDelta = true
			s.pendingStopReason = mapStopReasonToAnthropic(fr)
		}
	}

	// usage
	if usage := getMap(chunk, "usage"); usage != nil {
		s.pendingUsage = buildAnthropicUsageFromOpenAI(usage)
	}

	return output
}

func (s *openaiChatToAnthropicStream) handleDone() []byte {
	if s.hasSentStop {
		return nil
	}
	s.hasSentStop = true
	var output []byte
	s.ensureMessageStart(&output)

	// 关闭已打开的 thinking block
	if s.openThinkingBlockIdx != nil {
		output = append(output, sseEvent("content_block_stop", mustJSON(map[string]interface{}{
			"type": "content_block_stop", "index": *s.openThinkingBlockIdx,
		}))...)
		s.openThinkingBlockIdx = nil
	}

	// 关闭已打开的 text block
	if s.openTextBlockIndex != nil {
		stopEvent := map[string]interface{}{
			"type":  "content_block_stop",
			"index": *s.openTextBlockIndex,
		}
		output = append(output, sseEvent("content_block_stop", mustJSON(stopEvent))...)
		s.openTextBlockIndex = nil
	}

	// 关闭已打开的 tool blocks（按 anthropicIndex 升序，保证与 content_block_start 顺序匹配）
	var openIdxs []int
	for _, block := range s.toolBlocks {
		if s.openToolIndices[block.anthropicIndex] {
			openIdxs = append(openIdxs, block.anthropicIndex)
		}
	}
	sort.Ints(openIdxs)
	for _, idx := range openIdxs {
		stopEvent := map[string]interface{}{
			"type":  "content_block_stop",
			"index": idx,
		}
		output = append(output, sseEvent("content_block_stop", mustJSON(stopEvent))...)
		delete(s.openToolIndices, idx)
	}

	// message_delta
	stopReason := s.pendingStopReason
	if stopReason == "" {
		stopReason = "end_turn"
	}
	usage := s.pendingUsage
	if usage == nil {
		usage = map[string]interface{}{"input_tokens": 0, "output_tokens": 0}
	}
	deltaEvent := map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": usage,
	}
	output = append(output, sseEvent("message_delta", mustJSON(deltaEvent))...)

	// message_stop
	stopEvt := map[string]interface{}{"type": "message_stop"}
	output = append(output, sseEvent("message_stop", mustJSON(stopEvt))...)

	return output
}

// ============================================================================
// OpenAI Responses SSE → Anthropic SSE（port 自 streaming_responses.rs）
// ============================================================================

type openaiResponsesToAnthropicStream struct {
	buffer               bytes.Buffer
	utf8Remainder        []byte
	messageID            string
	model                string
	hasSentMessageStart  bool
	nextContentIndex     int
	openTextBlockIndex   *int
	openThinkingBlockIdx *int
	toolBlocks           map[string]*responsesToolBlock
	openToolIndices      map[int]bool
	hasSentStop          bool
	hasSetStopReason     bool
	pendingStopReason    string
	pendingUsage         map[string]interface{}
	startUsage           map[string]interface{}
}

type responsesToolBlock struct {
	anthropicIndex        int
	callID                string
	name                  string
	started               bool
	consecutiveWhitespace int
	aborted               bool
}

// feedToolArgs 检测 OpenAI Responses 流式 arguments 的连续空白异常，
// 行为与 toolBlockState.feedToolArgs 一致。
func (b *responsesToolBlock) feedToolArgs(args string) string {
	if b.aborted {
		return "skip"
	}
	for _, r := range args {
		switch r {
		case ' ', '\t', '\n', '\r':
			b.consecutiveWhitespace++
			if b.consecutiveWhitespace > infiniteWhitespaceThreshold {
				b.aborted = true
				return "abort"
			}
		default:
			b.consecutiveWhitespace = 0
		}
	}
	return "emit"
}

func (s *openaiResponsesToAnthropicStream) Feed(chunk []byte) ([]byte, error) {
	if len(s.utf8Remainder) > 0 {
		chunk = append(s.utf8Remainder, chunk...)
		s.utf8Remainder = nil
	}
	s.utf8Remainder = appendUTF8Safe(&s.buffer, chunk)
	var output []byte

	for {
		block, ok := takeSSEBlock(&s.buffer)
		if !ok {
			break
		}
		if strings.TrimSpace(block) == "" {
			continue
		}

		eventType, dataStr := parseSSEBlock(block)
		if dataStr == "" {
			continue
		}

		var data map[string]interface{}
		if err := json.Unmarshal([]byte(dataStr), &data); err != nil {
			continue
		}

		out := s.handleEvent(eventType, data)
		output = append(output, out...)
	}
	return output, nil
}

func (s *openaiResponsesToAnthropicStream) ensureMessageStart(output *[]byte) {
	if !s.hasSentMessageStart {
		s.hasSentMessageStart = true
		usage := s.startUsage
		if usage == nil {
			usage = map[string]interface{}{"input_tokens": 0, "output_tokens": 0}
		}
		event := map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"id":    s.messageID,
				"type":  "message",
				"role":  "assistant",
				"model": s.model,
				"usage": usage,
			},
		}
		*output = append(*output, sseEvent("message_start", mustJSON(event))...)
	}
}

func (s *openaiResponsesToAnthropicStream) Flush() ([]byte, error) {
	if !s.hasSentStop {
		s.hasSentStop = true
		var output []byte
		s.ensureMessageStart(&output)
		if s.openThinkingBlockIdx != nil {
			output = append(output, sseEvent("content_block_stop", mustJSON(map[string]interface{}{
				"type": "content_block_stop", "index": *s.openThinkingBlockIdx,
			}))...)
			s.openThinkingBlockIdx = nil
		}
		if s.openTextBlockIndex != nil {
			output = append(output, sseEvent("content_block_stop", mustJSON(map[string]interface{}{
				"type": "content_block_stop", "index": *s.openTextBlockIndex,
			}))...)
			s.openTextBlockIndex = nil
		}
		for _, idx := range sortedIntKeys(s.openToolIndices) {
			output = append(output, sseEvent("content_block_stop", mustJSON(map[string]interface{}{
				"type": "content_block_stop", "index": idx,
			}))...)
		}
		stopReason := s.pendingStopReason
		if stopReason == "" {
			stopReason = "end_turn"
		}
		usage := s.pendingUsage
		if usage == nil {
			usage = map[string]interface{}{"input_tokens": 0, "output_tokens": 0}
		}
		output = append(output, sseEvent("message_delta", mustJSON(map[string]interface{}{
			"type":  "message_delta",
			"delta": map[string]interface{}{"stop_reason": stopReason, "stop_sequence": nil},
			"usage": usage,
		}))...)
		output = append(output, sseEvent("message_stop", mustJSON(map[string]interface{}{"type": "message_stop"}))...)
		return output, nil
	}
	return nil, nil
}

func (s *openaiResponsesToAnthropicStream) handleEvent(eventType string, data map[string]interface{}) []byte {
	var output []byte

	if s.toolBlocks == nil {
		s.toolBlocks = make(map[string]*responsesToolBlock)
	}
	if s.openToolIndices == nil {
		s.openToolIndices = make(map[int]bool)
	}

	switch eventType {
	case "response.created":
		resp := getMap(data, "response")
		if resp == nil {
			resp = data
		}
		if id, ok := asString(resp["id"]); ok {
			s.messageID = id
		}
		if m, ok := asString(resp["model"]); ok {
			s.model = m
		}
		// 懒发送：存储 startUsage 但不立即发 message_start，
		// 等到首个内容 delta 或 emitStop 时才发。
		s.startUsage = buildAnthropicUsageFromResponses(getMap(resp, "usage"))

	case "response.output_text.delta":
		s.ensureMessageStart(&output)
		// thinking 与 text 互斥：先关闭已打开的 thinking block
		if s.openThinkingBlockIdx != nil {
			output = append(output, sseEvent("content_block_stop", mustJSON(map[string]interface{}{
				"type": "content_block_stop", "index": *s.openThinkingBlockIdx,
			}))...)
			s.openThinkingBlockIdx = nil
		}
		if s.openTextBlockIndex == nil {
			idx := s.nextContentIndex
			s.nextContentIndex++
			s.openTextBlockIndex = &idx
			output = append(output, sseEvent("content_block_start", mustJSON(map[string]interface{}{
				"type":          "content_block_start",
				"index":         idx,
				"content_block": map[string]interface{}{"type": "text", "text": ""},
			}))...)
		}
		delta := getString(data, "delta")
		if delta != "" {
			output = append(output, sseEvent("content_block_delta", mustJSON(map[string]interface{}{
				"type":  "content_block_delta",
				"index": *s.openTextBlockIndex,
				"delta": map[string]interface{}{"type": "text_delta", "text": delta},
			}))...)
		}

	case "response.output_text.done":
		if s.openTextBlockIndex != nil {
			output = append(output, sseEvent("content_block_stop", mustJSON(map[string]interface{}{
				"type": "content_block_stop", "index": *s.openTextBlockIndex,
			}))...)
			s.openTextBlockIndex = nil
		}

	case "response.reasoning.delta":
		s.ensureMessageStart(&output)
		// thinking 与 text 互斥：先关闭已打开的 text block
		if s.openTextBlockIndex != nil {
			output = append(output, sseEvent("content_block_stop", mustJSON(map[string]interface{}{
				"type": "content_block_stop", "index": *s.openTextBlockIndex,
			}))...)
			s.openTextBlockIndex = nil
		}
		if s.openThinkingBlockIdx == nil {
			idx := s.nextContentIndex
			s.nextContentIndex++
			s.openThinkingBlockIdx = &idx
			output = append(output, sseEvent("content_block_start", mustJSON(map[string]interface{}{
				"type":          "content_block_start",
				"index":         idx,
				"content_block": map[string]interface{}{"type": "thinking", "thinking": ""},
			}))...)
		}
		if delta := getString(data, "delta"); delta != "" {
			output = append(output, sseEvent("content_block_delta", mustJSON(map[string]interface{}{
				"type":  "content_block_delta",
				"index": *s.openThinkingBlockIdx,
				"delta": map[string]interface{}{"type": "thinking_delta", "thinking": delta},
			}))...)
		}

	case "response.reasoning.done":
		if s.openThinkingBlockIdx != nil {
			output = append(output, sseEvent("content_block_stop", mustJSON(map[string]interface{}{
				"type": "content_block_stop", "index": *s.openThinkingBlockIdx,
			}))...)
			s.openThinkingBlockIdx = nil
		}

	case "response.function_call_arguments.delta":
		itemID := getString(data, "item_id")
		if itemID == "" {
			itemID = getString(data, "output_index")
		}
		block, exists := s.toolBlocks[itemID]
		if !exists {
			block = &responsesToolBlock{anthropicIndex: s.nextContentIndex}
			s.nextContentIndex++
			s.toolBlocks[itemID] = block
			s.openToolIndices[block.anthropicIndex] = true
		}
		if item := getMap(data, "item"); item != nil {
			if id, ok := asString(item["call_id"]); ok && block.callID == "" {
				block.callID = id
			}
			if name, ok := asString(item["name"]); ok && block.name == "" {
				block.name = name
			}
		}
		if !block.started {
			block.started = true
			s.ensureMessageStart(&output)
			output = append(output, sseEvent("content_block_start", mustJSON(map[string]interface{}{
				"type":  "content_block_start",
				"index": block.anthropicIndex,
				"content_block": map[string]interface{}{
					"type":  "tool_use",
					"id":    block.callID,
					"name":  block.name,
					"input": map[string]interface{}{},
				},
			}))...)
		}
		delta := getString(data, "delta")
		if delta != "" {
			switch block.feedToolArgs(delta) {
			case "abort":
				output = append(output, sseEvent("content_block_stop", mustJSON(map[string]interface{}{
					"type":  "content_block_stop",
					"index": block.anthropicIndex,
				}))...)
				delete(s.openToolIndices, block.anthropicIndex)
			case "emit":
				output = append(output, sseEvent("content_block_delta", mustJSON(map[string]interface{}{
					"type":  "content_block_delta",
					"index": block.anthropicIndex,
					"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": delta},
				}))...)
			case "skip":
				// 已中止，跳过
			}
		}

	case "response.output_item.added":
		// OpenAI Responses 规范：完整 item 通过 output_item.added 投递，
		// function_call_arguments.delta 只携带 item_id/output_index/delta。
		// 在此事件中初始化 tool block 的 callID/name，避免 content_block_start
		// 发出空 id/name。
		item := getMap(data, "item")
		if item == nil || getString(item, "type") != "function_call" {
			break
		}
		itemID := getString(item, "id")
		if itemID == "" {
			itemID = getString(data, "output_index")
		}
		if itemID == "" {
			break
		}
		if s.toolBlocks == nil {
			s.toolBlocks = make(map[string]*responsesToolBlock)
		}
		block, exists := s.toolBlocks[itemID]
		if !exists {
			block = &responsesToolBlock{anthropicIndex: s.nextContentIndex}
			s.nextContentIndex++
			s.toolBlocks[itemID] = block
			s.openToolIndices[block.anthropicIndex] = true
		}
		if id, ok := asString(item["call_id"]); ok && id != "" {
			block.callID = id
		}
		if name, ok := asString(item["name"]); ok && name != "" {
			block.name = name
		}

	case "response.output_item.done":
		item := getMap(data, "item")
		if item != nil && getString(item, "type") == "function_call" {
			itemID := getString(item, "id")
			if itemID == "" {
				itemID = getString(data, "output_index")
			}
			if block, exists := s.toolBlocks[itemID]; exists {
				if s.openToolIndices[block.anthropicIndex] {
					output = append(output, sseEvent("content_block_stop", mustJSON(map[string]interface{}{
						"type": "content_block_stop", "index": block.anthropicIndex,
					}))...)
					delete(s.openToolIndices, block.anthropicIndex)
				}
			}
		}

	case "response.completed":
		resp := getMap(data, "response")
		if resp != nil {
			if !s.hasSetStopReason {
				s.hasSetStopReason = true
				status := getString(resp, "status")
				hasToolUse := len(s.toolBlocks) > 0
				s.pendingStopReason = mapResponsesStopReason(status, "", hasToolUse)
			}
			if usage := getMap(resp, "usage"); usage != nil {
				s.pendingUsage = buildAnthropicUsageFromResponses(usage)
			}
		}
		// 发出终止序列
		output = append(output, s.emitStop()...)

	case "response.incomplete":
		resp := getMap(data, "response")
		if !s.hasSetStopReason {
			s.hasSetStopReason = true
			hasToolUse := len(s.toolBlocks) > 0
			incompleteReason := ""
			if resp != nil {
				incompleteReason = getString(resp, "incomplete_reason")
			}
			s.pendingStopReason = mapResponsesStopReason("incomplete", incompleteReason, hasToolUse)
		}
		if resp != nil {
			if usage := getMap(resp, "usage"); usage != nil {
				s.pendingUsage = buildAnthropicUsageFromResponses(usage)
			}
		}
		output = append(output, s.emitStop()...)
	}

	return output
}

func (s *openaiResponsesToAnthropicStream) emitStop() []byte {
	if s.hasSentStop {
		return nil
	}
	s.hasSentStop = true
	var output []byte
	if s.openThinkingBlockIdx != nil {
		output = append(output, sseEvent("content_block_stop", mustJSON(map[string]interface{}{
			"type": "content_block_stop", "index": *s.openThinkingBlockIdx,
		}))...)
		s.openThinkingBlockIdx = nil
	}
	if s.openTextBlockIndex != nil {
		output = append(output, sseEvent("content_block_stop", mustJSON(map[string]interface{}{
			"type": "content_block_stop", "index": *s.openTextBlockIndex,
		}))...)
		s.openTextBlockIndex = nil
	}
	for _, idx := range sortedIntKeys(s.openToolIndices) {
		output = append(output, sseEvent("content_block_stop", mustJSON(map[string]interface{}{
			"type": "content_block_stop", "index": idx,
		}))...)
	}
	stopReason := s.pendingStopReason
	if stopReason == "" {
		stopReason = "end_turn"
	}
	usage := s.pendingUsage
	if usage == nil {
		usage = map[string]interface{}{"input_tokens": 0, "output_tokens": 0}
	}
	output = append(output, sseEvent("message_delta", mustJSON(map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": usage,
	}))...)
	output = append(output, sseEvent("message_stop", mustJSON(map[string]interface{}{"type": "message_stop"}))...)
	return output
}

// ============================================================================
// Gemini SSE → Anthropic SSE（port 自 streaming_gemini.rs）
// ============================================================================

type geminiToolCallSnapshot struct {
	anthropicIndex        int
	id                    string
	name                  string
	args                  string
	thoughtSignature      string
	consecutiveWhitespace int
	aborted               bool
}

// feedToolArgs 检测 Gemini 流式 tool args 的连续空白异常，
// 行为与 toolBlockState.feedToolArgs 一致。
func (b *geminiToolCallSnapshot) feedToolArgs(args string) string {
	if b.aborted {
		return "skip"
	}
	for _, r := range args {
		switch r {
		case ' ', '\t', '\n', '\r':
			b.consecutiveWhitespace++
			if b.consecutiveWhitespace > infiniteWhitespaceThreshold {
				b.aborted = true
				return "abort"
			}
		default:
			b.consecutiveWhitespace = 0
		}
	}
	return "emit"
}

type geminiToAnthropicStream struct {
	buffer              bytes.Buffer
	utf8Remainder       []byte
	messageID           string
	model               string
	hasSentMessageStart bool
	nextContentIndex    int
	accumulatedText     string
	openTextBlockIndex  *int
	toolSnapshots       []geminiToolCallSnapshot
	openToolIndices     map[int]bool
	hasSentStop         bool
	hasSetStopReason    bool
	pendingStopReason   string
	pendingUsage        map[string]interface{}
	startUsage          map[string]interface{}
}

func (s *geminiToAnthropicStream) Feed(chunk []byte) ([]byte, error) {
	if len(s.utf8Remainder) > 0 {
		chunk = append(s.utf8Remainder, chunk...)
		s.utf8Remainder = nil
	}
	s.utf8Remainder = appendUTF8Safe(&s.buffer, chunk)
	var output []byte

	for {
		block, ok := takeSSEBlock(&s.buffer)
		if !ok {
			break
		}
		if strings.TrimSpace(block) == "" {
			continue
		}

		_, dataStr := parseSSEBlock(block)
		if dataStr == "" {
			continue
		}

		var data map[string]interface{}
		if err := json.Unmarshal([]byte(dataStr), &data); err != nil {
			continue
		}

		out := s.handleGeminiChunk(data)
		output = append(output, out...)
	}
	return output, nil
}

func (s *geminiToAnthropicStream) Flush() ([]byte, error) {
	if !s.hasSentStop {
		return s.emitStop(), nil
	}
	return nil, nil
}

func (s *geminiToAnthropicStream) ensureMessageStart(output *[]byte) {
	if !s.hasSentMessageStart {
		s.hasSentMessageStart = true
		usage := s.startUsage
		if usage == nil {
			usage = map[string]interface{}{"input_tokens": 0, "output_tokens": 0}
		}
		event := map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"id":    s.messageID,
				"type":  "message",
				"role":  "assistant",
				"model": s.model,
				"usage": usage,
			},
		}
		*output = append(*output, sseEvent("message_start", mustJSON(event))...)
	}
}

func (s *geminiToAnthropicStream) handleGeminiChunk(data map[string]interface{}) []byte {
	var output []byte

	if s.openToolIndices == nil {
		s.openToolIndices = make(map[int]bool)
	}

	// 首次 chunk：存储 id/model/usage 但不立即发 message_start（懒发送）。
	if s.messageID == "" {
		s.messageID = getString(data, "responseId")
	}
	if s.model == "" {
		s.model = getString(data, "modelVersion")
	}
	if s.startUsage == nil {
		s.startUsage = buildAnthropicUsageFromGemini(getMap(data, "usageMetadata"))
	}

	candidates := getArray(data, "candidates")
	if len(candidates) == 0 {
		return output
	}
	candidate, ok := asMap(candidates[0])
	if !ok {
		return output
	}

	contentObj := getMap(candidate, "content")
	var parts []interface{}
	if contentObj != nil {
		parts = getArray(contentObj, "parts")
	}

	// 提取累积文本
	currentText := extractGeminiVisibleText(parts)
	// diff 取增量：仅当当前快照不短于已累积长度时才视为单调增长并取增量。
	// Gemini 规范下累积快照应单调增长，但偶发短快照（非规范）时若无条件覆盖，
	// 下一轮增长会从截断基底 diff 导致文本重复。
	var newText string
	if len(currentText) > len(s.accumulatedText) {
		newText = currentText[len(s.accumulatedText):]
		s.accumulatedText = currentText
	} else if currentText == s.accumulatedText {
		// 等长且相同：无增量，保持基底
	} else if len(currentText) < len(s.accumulatedText) {
		// 短快照：不回退基底，忽略该快照的文本差异
	}

	// 输出文本增量
	if newText != "" {
		s.ensureMessageStart(&output)
		if s.openTextBlockIndex == nil {
			idx := s.nextContentIndex
			s.nextContentIndex++
			s.openTextBlockIndex = &idx
			output = append(output, sseEvent("content_block_start", mustJSON(map[string]interface{}{
				"type":          "content_block_start",
				"index":         idx,
				"content_block": map[string]interface{}{"type": "text", "text": ""},
			}))...)
		}
		output = append(output, sseEvent("content_block_delta", mustJSON(map[string]interface{}{
			"type":  "content_block_delta",
			"index": *s.openTextBlockIndex,
			"delta": map[string]interface{}{"type": "text_delta", "text": newText},
		}))...)
	}

	// 处理工具调用
	incomingToolCalls := extractGeminiToolCalls(parts)
	hasToolUse := len(incomingToolCalls) > 0

	// 合并快照并输出增量
	for i, tc := range incomingToolCalls {
		var block *geminiToolCallSnapshot
		if i < len(s.toolSnapshots) {
			block = &s.toolSnapshots[i]
		} else {
			// 新工具调用 → content_block_start
			s.ensureMessageStart(&output)
			idx := s.nextContentIndex
			s.nextContentIndex++
			s.openToolIndices[idx] = true
			s.toolSnapshots = append(s.toolSnapshots, geminiToolCallSnapshot{})

			// 关闭已打开的 text block
			if s.openTextBlockIndex != nil {
				output = append(output, sseEvent("content_block_stop", mustJSON(map[string]interface{}{
					"type": "content_block_stop", "index": *s.openTextBlockIndex,
				}))...)
				s.openTextBlockIndex = nil
			}

			id := tc.id
			if id == "" {
				id = synthesizeToolCallID()
			}
			// 影子存储：抓取 thoughtSignature 供下一轮 Anthropic→Gemini 复用
			if tc.thoughtSignature != "" {
				storeGeminiThoughtSignature(id, tc.thoughtSignature)
			}
			output = append(output, sseEvent("content_block_start", mustJSON(map[string]interface{}{
				"type":  "content_block_start",
				"index": idx,
				"content_block": map[string]interface{}{
					"type":  "tool_use",
					"id":    id,
					"name":  tc.name,
					"input": map[string]interface{}{},
				},
			}))...)
			block = &s.toolSnapshots[i]
			block.id = id
			block.name = tc.name
			block.anthropicIndex = idx
			block.thoughtSignature = tc.thoughtSignature
		}

		// 输出 args 增量
		newArgs := tc.args
		if len(newArgs) > len(block.args) {
			delta := newArgs[len(block.args):]
			block.args = newArgs
			switch block.feedToolArgs(delta) {
			case "abort":
				output = append(output, sseEvent("content_block_stop", mustJSON(map[string]interface{}{
					"type":  "content_block_stop",
					"index": block.anthropicIndex,
				}))...)
				delete(s.openToolIndices, block.anthropicIndex)
			case "emit":
				output = append(output, sseEvent("content_block_delta", mustJSON(map[string]interface{}{
					"type":  "content_block_delta",
					"index": block.anthropicIndex,
					"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": delta},
				}))...)
			case "skip":
				// 已中止，跳过
			}
		}
	}

	// finishReason
	if fr := getString(candidate, "finishReason"); fr != "" {
		if !s.hasSetStopReason {
			s.hasSetStopReason = true
			s.pendingStopReason = mapGeminiFinishReason(fr, hasToolUse)
		}
		// 更新 usage
		if usage := getMap(data, "usageMetadata"); usage != nil {
			s.pendingUsage = buildAnthropicUsageFromGemini(usage)
		}
		output = append(output, s.emitStop()...)
	}

	return output
}

func (s *geminiToAnthropicStream) emitStop() []byte {
	if s.hasSentStop {
		return nil
	}
	s.hasSentStop = true
	var output []byte
	s.ensureMessageStart(&output)

	if s.openTextBlockIndex != nil {
		output = append(output, sseEvent("content_block_stop", mustJSON(map[string]interface{}{
			"type": "content_block_stop", "index": *s.openTextBlockIndex,
		}))...)
		s.openTextBlockIndex = nil
	}
	for _, idx := range sortedIntKeys(s.openToolIndices) {
		output = append(output, sseEvent("content_block_stop", mustJSON(map[string]interface{}{
			"type": "content_block_stop", "index": idx,
		}))...)
	}

	stopReason := s.pendingStopReason
	if stopReason == "" {
		stopReason = "end_turn"
	}
	usage := s.pendingUsage
	if usage == nil {
		usage = map[string]interface{}{"input_tokens": 0, "output_tokens": 0}
	}
	output = append(output, sseEvent("message_delta", mustJSON(map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": usage,
	}))...)
	output = append(output, sseEvent("message_stop", mustJSON(map[string]interface{}{"type": "message_stop"}))...)
	return output
}

func extractGeminiVisibleText(parts []interface{}) string {
	var sb strings.Builder
	for _, part := range parts {
		pm, ok := asMap(part)
		if !ok {
			continue
		}
		if b, ok := asBool(pm["thought"]); ok && b {
			continue
		}
		if t, ok := asString(pm["text"]); ok {
			sb.WriteString(t)
		}
	}
	return sb.String()
}

func extractGeminiToolCalls(parts []interface{}) []geminiToolCallSnapshot {
	var calls []geminiToolCallSnapshot
	for _, part := range parts {
		pm, ok := asMap(part)
		if !ok {
			continue
		}
		if fc := getMap(pm, "functionCall"); fc != nil {
			id := getString(fc, "id")
			name := getString(fc, "name")
			args := canonicalJSONString(fc["args"])
			sig := extractGeminiThoughtSignature(fc)
			calls = append(calls, geminiToolCallSnapshot{
				id:               id,
				name:             name,
				args:             args,
				thoughtSignature: sig,
			})
		}
	}
	return calls
}

// ============================================================================
// Anthropic SSE → OpenAI Chat SSE（新写 inverse）
// ============================================================================

type anthropicToOpenAIChatStream struct {
	buffer            bytes.Buffer
	utf8Remainder     []byte
	messageID         string
	model             string
	nextToolCallIndex int
	// anthropic block index → openai tool_calls.index，用于并行工具调用的 delta 路由
	toolCallIdxByBlock map[int]int
	pendingStopReason  string
	pendingUsage       map[string]interface{}
	startUsage         map[string]interface{} // message_start.message.usage（含 input_tokens）
	hasSentDone        bool
}

func (s *anthropicToOpenAIChatStream) Feed(chunk []byte) ([]byte, error) {
	if len(s.utf8Remainder) > 0 {
		chunk = append(s.utf8Remainder, chunk...)
		s.utf8Remainder = nil
	}
	s.utf8Remainder = appendUTF8Safe(&s.buffer, chunk)
	var output []byte

	for {
		block, ok := takeSSEBlock(&s.buffer)
		if !ok {
			break
		}
		if strings.TrimSpace(block) == "" {
			continue
		}

		eventType, dataStr := parseSSEBlock(block)
		if dataStr == "" {
			continue
		}

		var data map[string]interface{}
		if err := json.Unmarshal([]byte(dataStr), &data); err != nil {
			continue
		}

		out := s.handleAnthropicEvent(eventType, data)
		output = append(output, out...)
	}
	return output, nil
}

func (s *anthropicToOpenAIChatStream) Flush() ([]byte, error) {
	if !s.hasSentDone {
		return s.emitDone(), nil
	}
	return nil, nil
}

func (s *anthropicToOpenAIChatStream) handleAnthropicEvent(eventType string, data map[string]interface{}) []byte {
	var output []byte

	switch eventType {
	case "message_start":
		msg := getMap(data, "message")
		if msg != nil {
			s.messageID = getString(msg, "id")
			s.model = getString(msg, "model")
			// message_start.usage 携带 input_tokens（及 cache 字段），message_delta.usage
			// 仅含 output_tokens，需在此捕获以便最终合并。
			if u := getMap(msg, "usage"); u != nil {
				s.startUsage = u
			}
		}
		// 发出初始 chunk（role only）
		chunk := map[string]interface{}{
			"id":     s.messageID,
			"object": "chat.completion.chunk",
			"model":  s.model,
			"choices": []interface{}{
				map[string]interface{}{
					"index":         0,
					"delta":         map[string]interface{}{"role": "assistant"},
					"finish_reason": nil,
				},
			},
		}
		output = append(output, sseData(mustJSON(chunk))...)

	case "content_block_start":
		block := getMap(data, "content_block")
		if block != nil {
			blockType := getString(block, "type")
			if blockType == "tool_use" {
				idxf, _ := data["index"].(float64)
				blockIdx := int(idxf)
				idx := s.nextToolCallIndex
				s.nextToolCallIndex++
				if s.toolCallIdxByBlock == nil {
					s.toolCallIdxByBlock = make(map[int]int)
				}
				s.toolCallIdxByBlock[blockIdx] = idx
				callID := getString(block, "id")
				callName := getString(block, "name")
				chunk := map[string]interface{}{
					"id":     s.messageID,
					"object": "chat.completion.chunk",
					"model":  s.model,
					"choices": []interface{}{
						map[string]interface{}{
							"index": 0,
							"delta": map[string]interface{}{
								"tool_calls": []interface{}{
									map[string]interface{}{
										"index": idx,
										"id":    callID,
										"type":  "function",
										"function": map[string]interface{}{
											"name":      callName,
											"arguments": "",
										},
									},
								},
							},
							"finish_reason": nil,
						},
					},
				}
				output = append(output, sseData(mustJSON(chunk))...)
			}
		}

	case "content_block_delta":
		delta := getMap(data, "delta")
		if delta == nil {
			break
		}
		deltaType := getString(delta, "type")
		switch deltaType {
		case "text_delta":
			text := getString(delta, "text")
			if text != "" {
				chunk := map[string]interface{}{
					"id":     s.messageID,
					"object": "chat.completion.chunk",
					"model":  s.model,
					"choices": []interface{}{
						map[string]interface{}{
							"index":         0,
							"delta":         map[string]interface{}{"content": text},
							"finish_reason": nil,
						},
					},
				}
				output = append(output, sseData(mustJSON(chunk))...)
			}
		case "input_json_delta":
			// 按 delta 事件自身的 index 路由到对应 tool_calls 槽位，支持并行工具调用交错发送
			idxf, _ := data["index"].(float64)
			blockIdx := int(idxf)
			toolIdx, ok := s.toolCallIdxByBlock[blockIdx]
			partial := getString(delta, "partial_json")
			if partial != "" && ok {
				chunk := map[string]interface{}{
					"id":     s.messageID,
					"object": "chat.completion.chunk",
					"model":  s.model,
					"choices": []interface{}{
						map[string]interface{}{
							"index": 0,
							"delta": map[string]interface{}{
								"tool_calls": []interface{}{
									map[string]interface{}{
										"index":    toolIdx,
										"function": map[string]interface{}{"arguments": partial},
									},
								},
							},
							"finish_reason": nil,
						},
					},
				}
				output = append(output, sseData(mustJSON(chunk))...)
			}
		}

	case "content_block_stop":
		// 无状态：delta 已按自身 index 路由，无需在此调整

	case "message_delta":
		delta := getMap(data, "delta")
		if delta != nil {
			sr := getString(delta, "stop_reason")
			if sr != "" {
				s.pendingStopReason = sr
			}
		}
		if usage := getMap(data, "usage"); usage != nil {
			s.pendingUsage = usage
		}

	case "message_stop":
		output = append(output, s.emitDone()...)
	}

	return output
}

func (s *anthropicToOpenAIChatStream) emitDone() []byte {
	if s.hasSentDone {
		return nil
	}
	s.hasSentDone = true
	var output []byte

	finishReason := mapAnthropicStopReasonToOpenAI(s.pendingStopReason)
	chunk := map[string]interface{}{
		"id":     s.messageID,
		"object": "chat.completion.chunk",
		"model":  s.model,
		"choices": []interface{}{
			map[string]interface{}{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": finishReason,
			},
		},
	}
	if s.pendingUsage != nil {
		chunk["usage"] = buildOpenAIUsageFromAnthropic(mergeAnthropicUsage(s.startUsage, s.pendingUsage))
	} else if s.startUsage != nil {
		chunk["usage"] = buildOpenAIUsageFromAnthropic(s.startUsage)
	}
	output = append(output, sseData(mustJSON(chunk))...)
	output = append(output, sseData("[DONE]")...)
	return output
}

// ============================================================================
// Anthropic SSE → OpenAI Responses SSE（新写 inverse）
// ============================================================================

type anthropicToOpenAIResponsesStream struct {
	buffer          bytes.Buffer
	utf8Remainder   []byte
	messageID       string
	model           string
	hasSentCreated  bool
	nextOutputIndex int
	// 当前打开的文本块 output_index（nil 表示无打开文本块）
	openTextOutputIdx *int
	// anthropic block index → output item index
	blockOutputIdx map[int]int
	// 跟踪哪些 output_index 是 function_call 类型
	funcCallItems map[int]bool
	// 跟踪哪些 output_index 已打开（未关闭）
	openOutputIndices map[int]bool
	// output_index → 累积文本（message item）
	messageText map[int]string
	// output_index → {call_id, name}（function_call item）
	funcCallMeta map[int]map[string]string
	// output_index → 累积 partial_json（function_call item）
	funcCallArgs      map[int]string
	pendingStopReason string
	pendingUsage      map[string]interface{}
	startUsage        map[string]interface{} // message_start.message.usage（含 input_tokens）
	hasSentCompleted  bool
}

func (s *anthropicToOpenAIResponsesStream) Feed(chunk []byte) ([]byte, error) {
	if len(s.utf8Remainder) > 0 {
		chunk = append(s.utf8Remainder, chunk...)
		s.utf8Remainder = nil
	}
	s.utf8Remainder = appendUTF8Safe(&s.buffer, chunk)
	var output []byte

	for {
		block, ok := takeSSEBlock(&s.buffer)
		if !ok {
			break
		}
		if strings.TrimSpace(block) == "" {
			continue
		}

		eventType, dataStr := parseSSEBlock(block)
		if dataStr == "" {
			continue
		}

		var data map[string]interface{}
		if err := json.Unmarshal([]byte(dataStr), &data); err != nil {
			continue
		}

		out := s.handleAnthropicEvent(eventType, data)
		output = append(output, out...)
	}
	return output, nil
}

func (s *anthropicToOpenAIResponsesStream) Flush() ([]byte, error) {
	if !s.hasSentCompleted {
		return s.emitCompleted(), nil
	}
	return nil, nil
}

func (s *anthropicToOpenAIResponsesStream) handleAnthropicEvent(eventType string, data map[string]interface{}) []byte {
	var output []byte

	switch eventType {
	case "message_start":
		msg := getMap(data, "message")
		if msg != nil {
			s.messageID = getString(msg, "id")
			s.model = getString(msg, "model")
			// message_start.usage 携带 input_tokens，message_delta.usage 仅含 output_tokens
			if u := getMap(msg, "usage"); u != nil {
				s.startUsage = u
			}
		}
		s.hasSentCreated = true
		s.blockOutputIdx = make(map[int]int)
		s.funcCallItems = make(map[int]bool)
		s.openOutputIndices = make(map[int]bool)
		s.messageText = make(map[int]string)
		s.funcCallMeta = make(map[int]map[string]string)
		s.funcCallArgs = make(map[int]string)
		// response.created
		created := map[string]interface{}{
			"type": "response.created",
			"response": map[string]interface{}{
				"id":     s.messageID,
				"object": "response",
				"model":  s.model,
				"status": "in_progress",
				"output": []interface{}{},
			},
		}
		output = append(output, sseEvent("response.created", mustJSON(created))...)

	case "content_block_start":
		block := getMap(data, "content_block")
		if block == nil {
			break
		}
		idxf, _ := data["index"].(float64)
		idx := int(idxf)
		blockType := getString(block, "type")
		outputIdx := s.nextOutputIndex
		s.nextOutputIndex++
		s.blockOutputIdx[idx] = outputIdx
		s.openOutputIndices[outputIdx] = true

		switch blockType {
		case "text":
			// output_item.added (message)
			itemAdded := map[string]interface{}{
				"type":         "response.output_item.added",
				"output_index": outputIdx,
				"item": map[string]interface{}{
					"type": "message",
					"role": "assistant",
				},
			}
			output = append(output, sseEvent("response.output_item.added", mustJSON(itemAdded))...)
			// content_part.added：每个 message item 仅含一个 output_text part，content_index 恒为 0
			partAdded := map[string]interface{}{
				"type":          "response.content_part.added",
				"output_index":  outputIdx,
				"content_index": 0,
				"part":          map[string]interface{}{"type": "output_text", "text": ""},
			}
			output = append(output, sseEvent("response.content_part.added", mustJSON(partAdded))...)
			oi := outputIdx
			s.openTextOutputIdx = &oi

		case "tool_use":
			s.funcCallItems[outputIdx] = true
			callID := getString(block, "id")
			callName := getString(block, "name")
			s.funcCallMeta[outputIdx] = map[string]string{"call_id": callID, "name": callName}
			// output_item.added (function_call)
			itemAdded := map[string]interface{}{
				"type":         "response.output_item.added",
				"output_index": outputIdx,
				"item": map[string]interface{}{
					"type":    "function_call",
					"call_id": callID,
					"name":    callName,
				},
			}
			output = append(output, sseEvent("response.output_item.added", mustJSON(itemAdded))...)
		}

	case "content_block_delta":
		idxf, _ := data["index"].(float64)
		idx := int(idxf)
		outputIdx, ok := s.blockOutputIdx[idx]
		if !ok {
			break
		}
		delta := getMap(data, "delta")
		if delta == nil {
			break
		}
		deltaType := getString(delta, "type")

		switch deltaType {
		case "text_delta":
			text := getString(delta, "text")
			if text != "" {
				s.messageText[outputIdx] += text
				evt := map[string]interface{}{
					"type":          "response.output_text.delta",
					"output_index":  outputIdx,
					"content_index": 0,
					"delta":         text,
				}
				output = append(output, sseEvent("response.output_text.delta", mustJSON(evt))...)
			}
		case "input_json_delta":
			partialJSON := getString(delta, "partial_json")
			if partialJSON != "" {
				s.funcCallArgs[outputIdx] += partialJSON
				evt := map[string]interface{}{
					"type":         "response.function_call_arguments.delta",
					"output_index": outputIdx,
					"delta":        partialJSON,
				}
				output = append(output, sseEvent("response.function_call_arguments.delta", mustJSON(evt))...)
			}
		}

	case "content_block_stop":
		idxf, _ := data["index"].(float64)
		idx := int(idxf)
		outputIdx, ok := s.blockOutputIdx[idx]
		if !ok {
			break
		}
		delete(s.openOutputIndices, outputIdx)

		if s.funcCallItems[outputIdx] {
			// output_item.done (function_call) — 回填 call_id/name/arguments/status
			output = append(output, sseEvent("response.output_item.done", mustJSON(map[string]interface{}{
				"type":         "response.output_item.done",
				"output_index": outputIdx,
				"item":         s.buildDoneItem(outputIdx),
			}))...)
		} else {
			// content_part.done + output_item.done (text) — 回填 role/content
			output = append(output, sseEvent("response.content_part.done", mustJSON(map[string]interface{}{
				"type":          "response.content_part.done",
				"output_index":  outputIdx,
				"content_index": 0,
			}))...)
			output = append(output, sseEvent("response.output_item.done", mustJSON(map[string]interface{}{
				"type":         "response.output_item.done",
				"output_index": outputIdx,
				"item":         s.buildDoneItem(outputIdx),
			}))...)
		}
		if s.openTextOutputIdx != nil && *s.openTextOutputIdx == outputIdx {
			s.openTextOutputIdx = nil
		}

	case "message_delta":
		delta := getMap(data, "delta")
		if delta != nil {
			sr := getString(delta, "stop_reason")
			if sr != "" {
				s.pendingStopReason = sr
			}
		}
		if usage := getMap(data, "usage"); usage != nil {
			s.pendingUsage = usage
		}

	case "message_stop":
		output = append(output, s.emitCompleted()...)
	}

	return output
}

func (s *anthropicToOpenAIResponsesStream) emitCompleted() []byte {
	if s.hasSentCompleted {
		return nil
	}
	s.hasSentCompleted = true
	var output []byte

	// 关闭所有仍打开的 output items（回填完整字段）
	for outputIdx := range s.openOutputIndices {
		if s.funcCallItems[outputIdx] {
			output = append(output, sseEvent("response.output_item.done", mustJSON(map[string]interface{}{
				"type":         "response.output_item.done",
				"output_index": outputIdx,
				"item":         s.buildDoneItem(outputIdx),
			}))...)
		} else {
			output = append(output, sseEvent("response.content_part.done", mustJSON(map[string]interface{}{
				"type":          "response.content_part.done",
				"output_index":  outputIdx,
				"content_index": 0,
			}))...)
			output = append(output, sseEvent("response.output_item.done", mustJSON(map[string]interface{}{
				"type":         "response.output_item.done",
				"output_index": outputIdx,
				"item":         s.buildDoneItem(outputIdx),
			}))...)
		}
	}

	status := "completed"
	if s.pendingStopReason == "max_tokens" {
		status = "incomplete"
	}
	usage := map[string]interface{}{"input_tokens": 0, "output_tokens": 0}
	if s.pendingUsage != nil {
		usage = buildResponsesUsageFromAnthropic(mergeAnthropicUsage(s.startUsage, s.pendingUsage))
	} else if s.startUsage != nil {
		usage = buildResponsesUsageFromAnthropic(s.startUsage)
	}
	// response.completed：output 回填所有 item（按 output_index 升序）
	finalOutput := make([]interface{}, 0, s.nextOutputIndex)
	for outputIdx := 0; outputIdx < s.nextOutputIndex; outputIdx++ {
		finalOutput = append(finalOutput, s.buildDoneItem(outputIdx))
	}
	completed := map[string]interface{}{
		"type": "response.completed",
		"response": map[string]interface{}{
			"id":     s.messageID,
			"object": "response",
			"model":  s.model,
			"status": status,
			"output": finalOutput,
			"usage":  usage,
		},
	}
	output = append(output, sseEvent("response.completed", mustJSON(completed))...)
	return output
}

// buildDoneItem 构造 output_item.done / response.completed 中完整的 item。
// function_call 回填 call_id/name/arguments/status；message 回填 role/content。
func (s *anthropicToOpenAIResponsesStream) buildDoneItem(outputIdx int) map[string]interface{} {
	if s.funcCallItems[outputIdx] {
		item := map[string]interface{}{
			"type":   "function_call",
			"status": "completed",
		}
		if meta, ok := s.funcCallMeta[outputIdx]; ok {
			if meta["call_id"] != "" {
				item["call_id"] = meta["call_id"]
			}
			if meta["name"] != "" {
				item["name"] = meta["name"]
			}
		}
		args := s.funcCallArgs[outputIdx]
		if args == "" {
			item["arguments"] = "{}"
		} else {
			item["arguments"] = args
		}
		return item
	}
	content := s.messageText[outputIdx]
	return map[string]interface{}{
		"type": "message",
		"role": "assistant",
		"content": []interface{}{
			map[string]interface{}{
				"type": "output_text",
				"text": content,
			},
		},
	}
}

// ============================================================================
// Anthropic SSE → Gemini SSE（新写 inverse）
// ============================================================================

type anthropicToGeminiStream struct {
	buffer          bytes.Buffer
	utf8Remainder   []byte
	messageID       string
	model           string
	accumulatedText strings.Builder
	toolCalls       []map[string]interface{}
	toolArgBuf      []strings.Builder // 每个 tool call 的 partial_json 累积
	// anthropic block index → toolCalls 切片索引，用于并行工具调用的 delta 路由
	toolBlockIdx      map[int]int
	pendingStopReason string
	pendingUsage      map[string]interface{}
	startUsage        map[string]interface{} // message_start.message.usage（含 input_tokens）
	hasSentFinal      bool
}

func (s *anthropicToGeminiStream) Feed(chunk []byte) ([]byte, error) {
	if len(s.utf8Remainder) > 0 {
		chunk = append(s.utf8Remainder, chunk...)
		s.utf8Remainder = nil
	}
	s.utf8Remainder = appendUTF8Safe(&s.buffer, chunk)
	var output []byte

	for {
		block, ok := takeSSEBlock(&s.buffer)
		if !ok {
			break
		}
		if strings.TrimSpace(block) == "" {
			continue
		}

		eventType, dataStr := parseSSEBlock(block)
		if dataStr == "" {
			continue
		}

		var data map[string]interface{}
		if err := json.Unmarshal([]byte(dataStr), &data); err != nil {
			continue
		}

		out := s.handleAnthropicEvent(eventType, data)
		output = append(output, out...)
	}
	return output, nil
}

func (s *anthropicToGeminiStream) Flush() ([]byte, error) {
	if !s.hasSentFinal {
		return s.emitFinal(), nil
	}
	return nil, nil
}

func (s *anthropicToGeminiStream) handleAnthropicEvent(eventType string, data map[string]interface{}) []byte {
	var output []byte

	switch eventType {
	case "message_start":
		msg := getMap(data, "message")
		if msg != nil {
			s.messageID = getString(msg, "id")
			s.model = getString(msg, "model")
			// message_start.usage 携带 input_tokens，message_delta.usage 仅含 output_tokens
			if u := getMap(msg, "usage"); u != nil {
				s.startUsage = u
			}
		}

	case "content_block_start":
		block := getMap(data, "content_block")
		if block != nil && getString(block, "type") == "tool_use" {
			idxf, _ := data["index"].(float64)
			idx := int(idxf)
			tc := map[string]interface{}{
				"name": getString(block, "name"),
			}
			id := getString(block, "id")
			if id != "" && !isSynthesizedToolCallID(id) {
				tc["id"] = id
			}
			if s.toolBlockIdx == nil {
				s.toolBlockIdx = make(map[int]int)
			}
			s.toolBlockIdx[idx] = len(s.toolCalls)
			s.toolCalls = append(s.toolCalls, tc)
			s.toolArgBuf = append(s.toolArgBuf, strings.Builder{})
		}

	case "content_block_delta":
		delta := getMap(data, "delta")
		if delta == nil {
			break
		}
		switch getString(delta, "type") {
		case "text_delta":
			text := getString(delta, "text")
			if text != "" {
				s.accumulatedText.WriteString(text)
				// Gemini SSE 每块是累积快照，需要发送完整累积内容
				chunk := s.buildGeminiChunk()
				output = append(output, sseData(mustJSON(chunk))...)
			}
		case "input_json_delta":
			// 按 delta 事件自身的 index 路由到对应 tool 块，支持并行工具调用交错发送
			idxf, _ := data["index"].(float64)
			idx := int(idxf)
			toolIdx, ok := s.toolBlockIdx[idx]
			partial := getString(delta, "partial_json")
			if partial != "" && ok && toolIdx >= 0 && toolIdx < len(s.toolArgBuf) {
				s.toolArgBuf[toolIdx].WriteString(partial)
			}
		}

	case "content_block_stop":
		// 无状态：delta 已按自身 index 路由，无需在此调整

	case "message_delta":
		delta := getMap(data, "delta")
		if delta != nil {
			sr := getString(delta, "stop_reason")
			if sr != "" {
				s.pendingStopReason = sr
			}
		}
		if usage := getMap(data, "usage"); usage != nil {
			s.pendingUsage = usage
		}

	case "message_stop":
		output = append(output, s.emitFinal()...)
	}

	return output
}

func (s *anthropicToGeminiStream) buildGeminiChunk() map[string]interface{} {
	var parts []interface{}
	if s.accumulatedText.Len() > 0 {
		parts = append(parts, map[string]interface{}{"text": s.accumulatedText.String()})
	}
	for _, tc := range s.toolCalls {
		// Gemini 累积快照要求 functionCall 带 args；中间块未完成时填 {}
		if _, ok := tc["args"]; !ok {
			tc["args"] = map[string]interface{}{}
		}
		parts = append(parts, map[string]interface{}{"functionCall": tc})
	}
	chunk := map[string]interface{}{
		"candidates": []interface{}{
			map[string]interface{}{
				"content": map[string]interface{}{"parts": parts},
			},
		},
	}
	return chunk
}

func (s *anthropicToGeminiStream) emitFinal() []byte {
	if s.hasSentFinal {
		return nil
	}
	s.hasSentFinal = true

	finishReason := "STOP"
	switch s.pendingStopReason {
	case "max_tokens":
		finishReason = "MAX_TOKENS"
	case "tool_use", "end_turn", "stop_sequence":
		finishReason = "STOP"
	}

	var parts []interface{}
	if s.accumulatedText.Len() > 0 {
		parts = append(parts, map[string]interface{}{"text": s.accumulatedText.String()})
	}
	for i := range s.toolCalls {
		if i < len(s.toolArgBuf) {
			raw := s.toolArgBuf[i].String()
			if raw == "" {
				s.toolCalls[i]["args"] = map[string]interface{}{}
			} else {
				var args map[string]interface{}
				if err := json.Unmarshal([]byte(raw), &args); err == nil {
					s.toolCalls[i]["args"] = args
				} else {
					s.toolCalls[i]["args"] = map[string]interface{}{}
				}
			}
		} else {
			s.toolCalls[i]["args"] = map[string]interface{}{}
		}
		parts = append(parts, map[string]interface{}{"functionCall": s.toolCalls[i]})
	}

	usage := map[string]interface{}{"promptTokenCount": 0, "candidatesTokenCount": 0}
	if s.pendingUsage != nil {
		usage = buildGeminiUsageFromAnthropic(mergeAnthropicUsage(s.startUsage, s.pendingUsage))
	} else if s.startUsage != nil {
		usage = buildGeminiUsageFromAnthropic(s.startUsage)
	}

	chunk := map[string]interface{}{
		"candidates": []interface{}{
			map[string]interface{}{
				"content":      map[string]interface{}{"parts": parts},
				"finishReason": finishReason,
			},
		},
		"usageMetadata": usage,
	}
	return sseData(mustJSON(chunk))
}

// ============================================================================
// transformStreamReader：将 StreamConverter 包装为 io.ReadCloser
// ============================================================================

type transformStreamReader struct {
	upstream             io.ReadCloser
	converter            StreamConverter
	buf                  bytes.Buffer
	done                 bool
	pendingErr           error // upstream 非 EOF 错误，待 buf 排空后再返回给调用方
	tmpBuf               [4096]byte
	streamEndedWithError bool // upstream 以错误结束，抑制合成的成功终止事件
}

// transformStreamReader 创建一个 io.ReadCloser，从 upstream 读取 SSE 字节，
// 经 StreamConverter 转换后返回。inFormat/outFormat 语义同 NewStreamConverter。
func newTransformStreamReader(inFormat, outFormat string, upstream io.ReadCloser) io.ReadCloser {
	return &transformStreamReader{
		upstream:  upstream,
		converter: NewStreamConverter(inFormat, outFormat),
	}
}

func (r *transformStreamReader) Read(p []byte) (int, error) {
	for {
		// 缓冲区有数据 → 立即返回（优先排空已转换数据）
		if r.buf.Len() > 0 {
			return r.buf.Read(p)
		}
		// buf 排空后再暴露 pendingErr，避免 upstream 最后一次带数据的错误读
		// 导致已转换尾部数据被丢弃。
		if r.pendingErr != nil {
			return 0, r.pendingErr
		}
		if r.done {
			return 0, io.EOF
		}

		// 从 upstream 读取（阻塞直到有数据或 EOF/错误）
		n, err := r.upstream.Read(r.tmpBuf[:])
		if n > 0 {
			out, convErr := r.converter.Feed(r.tmpBuf[:n])
			if convErr != nil {
				return 0, fmt.Errorf("stream convert: %w", convErr)
			}
			r.buf.Write(out)
		}

		if err != nil {
			if err == io.EOF {
				// 若上游先出错再 EOF，不再 Flush 合成的成功终止事件，
				// 避免把失败伪装成正常完成。
				if !r.streamEndedWithError {
					flushOut, flushErr := r.converter.Flush()
					if flushErr != nil {
						return 0, fmt.Errorf("stream flush: %w", flushErr)
					}
					r.buf.Write(flushOut)
				}
				r.done = true
				// 不在此处 Close upstream，由 Close() 统一处理，避免双重关闭
				if r.buf.Len() > 0 {
					return r.buf.Read(p)
				}
				return 0, io.EOF
			}
			// 非 EOF 错误：发 Anthropic 格式 error 事件，让客户端看到错误而非静默断流。
			// 同时标记 streamEndedWithError，抑制后续 Flush 的 message_delta/message_stop。
			if !r.streamEndedWithError {
				r.streamEndedWithError = true
				errEvent := map[string]interface{}{
					"type": "error",
					"error": map[string]interface{}{
						"type":    "stream_error",
						"message": fmt.Sprintf("upstream stream error: %v", err),
					},
				}
				r.buf.Write(sseEvent("error", mustJSON(errEvent)))
			}
			r.pendingErr = err
			continue
		}

		// converter 可能因等待完整 SSE 事件而未输出 → 循环继续读 upstream，
		// 避免 (0, nil) 导致调用方 busy loop。
	}
}

func (r *transformStreamReader) Close() error {
	return r.upstream.Close()
}
