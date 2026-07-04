package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
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
		return &anthropicToGeminiStream{currentToolIdx: -1}
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

// ============================================================================
// OpenAI Chat SSE → Anthropic SSE（port 自 streaming.rs）
// ============================================================================

type toolBlockState struct {
	anthropicIndex int
	id             string
	name           string
	started        bool
}

type openaiChatToAnthropicStream struct {
	buffer              bytes.Buffer
	messageID           string
	model               string
	nextContentIndex    int
	hasSentMessageStart bool
	hasEmittedDelta     bool
	pendingStopReason   string
	pendingUsage        map[string]interface{}
	hasSentStop         bool
	openTextBlockIndex  *int
	toolBlocks          map[int]*toolBlockState
	openToolIndices     map[int]bool
}

func (s *openaiChatToAnthropicStream) Feed(chunk []byte) ([]byte, error) {
	s.buffer.Write(chunk)
	var output []byte

	for {
		block, ok := takeSSEBlock(&s.buffer)
		if !ok {
			break
		}
		if strings.TrimSpace(block) == "" {
			continue
		}

		// 提取 data: 行
		var dataStr string
		for _, line := range strings.Split(block, "\n") {
			if d, ok := stripSSEField(strings.TrimRight(line, "\r"), "data"); ok {
				dataStr = d
			}
		}
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

func (s *openaiChatToAnthropicStream) handleChunk(chunk map[string]interface{}) []byte {
	var output []byte

	if id, ok := asString(chunk["id"]); ok && s.messageID == "" {
		s.messageID = id
	}
	if m, ok := asString(chunk["model"]); ok && s.model == "" {
		s.model = m
	}

	// 首次 chunk → message_start
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
		output = append(output, sseEvent("message_start", mustJSON(event))...)
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

		// text content
		if content, ok := asString(delta["content"]); ok && content != "" {
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
					deltaEvent := map[string]interface{}{
						"type":  "content_block_delta",
						"index": block.anthropicIndex,
						"delta": map[string]interface{}{
							"type":         "input_json_delta",
							"partial_json": args,
						},
					}
					output = append(output, sseEvent("content_block_delta", mustJSON(deltaEvent))...)
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
				deltaEvent := map[string]interface{}{
					"type":  "content_block_delta",
					"index": block.anthropicIndex,
					"delta": map[string]interface{}{
						"type":         "input_json_delta",
						"partial_json": args,
					},
				}
				output = append(output, sseEvent("content_block_delta", mustJSON(deltaEvent))...)
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

	// 关闭已打开的 text block
	if s.openTextBlockIndex != nil {
		stopEvent := map[string]interface{}{
			"type":  "content_block_stop",
			"index": *s.openTextBlockIndex,
		}
		output = append(output, sseEvent("content_block_stop", mustJSON(stopEvent))...)
		s.openTextBlockIndex = nil
	}

	// 关闭已打开的 tool blocks
	for _, block := range s.toolBlocks {
		if s.openToolIndices[block.anthropicIndex] {
			stopEvent := map[string]interface{}{
				"type":  "content_block_stop",
				"index": block.anthropicIndex,
			}
			output = append(output, sseEvent("content_block_stop", mustJSON(stopEvent))...)
			delete(s.openToolIndices, block.anthropicIndex)
		}
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
	buffer              bytes.Buffer
	messageID           string
	model               string
	hasSentMessageStart bool
	nextContentIndex    int
	openTextBlockIndex  *int
	toolBlocks          map[string]*responsesToolBlock
	openToolIndices     map[int]bool
	hasSentStop         bool
	pendingStopReason   string
	pendingUsage        map[string]interface{}
}

type responsesToolBlock struct {
	anthropicIndex int
	callID         string
	name           string
	started        bool
}

func (s *openaiResponsesToAnthropicStream) Feed(chunk []byte) ([]byte, error) {
	s.buffer.Write(chunk)
	var output []byte

	for {
		block, ok := takeSSEBlock(&s.buffer)
		if !ok {
			break
		}
		if strings.TrimSpace(block) == "" {
			continue
		}

		var eventType, dataStr string
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimRight(line, "\r")
			if e, ok := stripSSEField(line, "event"); ok {
				eventType = strings.TrimSpace(e)
			} else if d, ok := stripSSEField(line, "data"); ok {
				dataStr = d
			}
		}
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

func (s *openaiResponsesToAnthropicStream) Flush() ([]byte, error) {
	if !s.hasSentStop {
		s.hasSentStop = true
		var output []byte
		if s.openTextBlockIndex != nil {
			output = append(output, sseEvent("content_block_stop", mustJSON(map[string]interface{}{
				"type": "content_block_stop", "index": *s.openTextBlockIndex,
			}))...)
			s.openTextBlockIndex = nil
		}
		for idx := range s.openToolIndices {
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
		s.hasSentMessageStart = true
		startUsage := buildAnthropicUsageFromResponses(getMap(resp, "usage"))
		event := map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"id":    s.messageID,
				"type":  "message",
				"role":  "assistant",
				"model": s.model,
				"usage": startUsage,
			},
		}
		output = append(output, sseEvent("message_start", mustJSON(event))...)

	case "response.output_text.delta":
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
			output = append(output, sseEvent("content_block_delta", mustJSON(map[string]interface{}{
				"type":  "content_block_delta",
				"index": block.anthropicIndex,
				"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": delta},
			}))...)
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
			status := getString(resp, "status")
			hasToolUse := len(s.toolBlocks) > 0
			s.pendingStopReason = mapResponsesStopReason(status, hasToolUse)
			if usage := getMap(resp, "usage"); usage != nil {
				s.pendingUsage = buildAnthropicUsageFromResponses(usage)
			}
		}
		// 发出终止序列
		output = append(output, s.emitStop()...)

	case "response.incomplete":
		s.pendingStopReason = "max_tokens"
		resp := getMap(data, "response")
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
	if s.openTextBlockIndex != nil {
		output = append(output, sseEvent("content_block_stop", mustJSON(map[string]interface{}{
			"type": "content_block_stop", "index": *s.openTextBlockIndex,
		}))...)
		s.openTextBlockIndex = nil
	}
	for idx := range s.openToolIndices {
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
	anthropicIndex int
	id             string
	name           string
	args           string
}

type geminiToAnthropicStream struct {
	buffer              bytes.Buffer
	messageID           string
	model               string
	hasSentMessageStart bool
	nextContentIndex    int
	accumulatedText     string
	openTextBlockIndex  *int
	toolSnapshots       []geminiToolCallSnapshot
	openToolIndices     map[int]bool
	hasSentStop         bool
	pendingStopReason   string
	pendingUsage        map[string]interface{}
}

func (s *geminiToAnthropicStream) Feed(chunk []byte) ([]byte, error) {
	s.buffer.Write(chunk)
	var output []byte

	for {
		block, ok := takeSSEBlock(&s.buffer)
		if !ok {
			break
		}
		if strings.TrimSpace(block) == "" {
			continue
		}

		var dataStr string
		for _, line := range strings.Split(block, "\n") {
			if d, ok := stripSSEField(strings.TrimRight(line, "\r"), "data"); ok {
				dataStr = d
			}
		}
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

func (s *geminiToAnthropicStream) handleGeminiChunk(data map[string]interface{}) []byte {
	var output []byte

	if s.openToolIndices == nil {
		s.openToolIndices = make(map[int]bool)
	}

	// 首次 → message_start
	if !s.hasSentMessageStart {
		s.hasSentMessageStart = true
		s.messageID = getString(data, "responseId")
		s.model = getString(data, "modelVersion")
		usage := buildAnthropicUsageFromGemini(getMap(data, "usageMetadata"))
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
		output = append(output, sseEvent("message_start", mustJSON(event))...)
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
	// diff 取增量
	var newText string
	if len(currentText) > len(s.accumulatedText) {
		newText = currentText[len(s.accumulatedText):]
	}
	s.accumulatedText = currentText

	// 输出文本增量
	if newText != "" {
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
		}

		// 输出 args 增量
		newArgs := tc.args
		if len(newArgs) > len(block.args) {
			delta := newArgs[len(block.args):]
			block.args = newArgs
			output = append(output, sseEvent("content_block_delta", mustJSON(map[string]interface{}{
				"type":  "content_block_delta",
				"index": block.anthropicIndex,
				"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": delta},
			}))...)
		}
	}

	// finishReason
	if fr := getString(candidate, "finishReason"); fr != "" {
		s.pendingStopReason = mapGeminiFinishReason(fr, hasToolUse)
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

	if s.openTextBlockIndex != nil {
		output = append(output, sseEvent("content_block_stop", mustJSON(map[string]interface{}{
			"type": "content_block_stop", "index": *s.openTextBlockIndex,
		}))...)
		s.openTextBlockIndex = nil
	}
	for idx := range s.openToolIndices {
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
			calls = append(calls, geminiToolCallSnapshot{id: id, name: name, args: args})
		}
	}
	return calls
}

// ============================================================================
// Anthropic SSE → OpenAI Chat SSE（新写 inverse）
// ============================================================================

type anthropicToOpenAIChatStream struct {
	buffer            bytes.Buffer
	messageID         string
	model             string
	hasSentFirstChunk bool
	currentToolIndex  *int
	nextToolCallIndex int
	toolCallID        string
	toolCallName      string
	pendingStopReason string
	pendingUsage      map[string]interface{}
	hasSentDone       bool
}

func (s *anthropicToOpenAIChatStream) Feed(chunk []byte) ([]byte, error) {
	s.buffer.Write(chunk)
	var output []byte

	for {
		block, ok := takeSSEBlock(&s.buffer)
		if !ok {
			break
		}
		if strings.TrimSpace(block) == "" {
			continue
		}

		var eventType, dataStr string
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimRight(line, "\r")
			if e, ok := stripSSEField(line, "event"); ok {
				eventType = strings.TrimSpace(e)
			} else if d, ok := stripSSEField(line, "data"); ok {
				dataStr = d
			}
		}
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
		}
		// 发出初始 chunk（role only）
		s.hasSentFirstChunk = true
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
				idx := s.nextToolCallIndex
				s.nextToolCallIndex++
				s.currentToolIndex = &idx
				s.toolCallID = getString(block, "id")
				s.toolCallName = getString(block, "name")
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
										"id":    s.toolCallID,
										"type":  "function",
										"function": map[string]interface{}{
											"name":      s.toolCallName,
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
			partial := getString(delta, "partial_json")
			if partial != "" && s.currentToolIndex != nil {
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
										"index":    *s.currentToolIndex,
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
		s.currentToolIndex = nil

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
		chunk["usage"] = buildOpenAIUsageFromAnthropic(s.pendingUsage)
	}
	output = append(output, sseData(mustJSON(chunk))...)
	output = append(output, sseData("[DONE]")...)
	return output
}

// ============================================================================
// Anthropic SSE → OpenAI Responses SSE（新写 inverse）
// ============================================================================

type anthropicToOpenAIResponsesStream struct {
	buffer           bytes.Buffer
	messageID        string
	model            string
	hasSentCreated   bool
	nextOutputIndex  int
	nextContentIndex int
	// 当前打开的文本块 output_index（nil 表示无打开文本块）
	openTextOutputIdx *int
	// anthropic block index → output item index
	blockOutputIdx map[int]int
	// 跟踪哪些 output_index 是 function_call 类型
	funcCallItems map[int]bool
	// 跟踪哪些 output_index 已打开（未关闭）
	openOutputIndices map[int]bool
	pendingStopReason string
	pendingUsage      map[string]interface{}
	hasSentCompleted  bool
}

func (s *anthropicToOpenAIResponsesStream) Feed(chunk []byte) ([]byte, error) {
	s.buffer.Write(chunk)
	var output []byte

	for {
		block, ok := takeSSEBlock(&s.buffer)
		if !ok {
			break
		}
		if strings.TrimSpace(block) == "" {
			continue
		}

		var eventType, dataStr string
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimRight(line, "\r")
			if e, ok := stripSSEField(line, "event"); ok {
				eventType = strings.TrimSpace(e)
			} else if d, ok := stripSSEField(line, "data"); ok {
				dataStr = d
			}
		}
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
		}
		s.hasSentCreated = true
		s.blockOutputIdx = make(map[int]int)
		s.funcCallItems = make(map[int]bool)
		s.openOutputIndices = make(map[int]bool)
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
			// content_part.added
			contentIdx := s.nextContentIndex
			s.nextContentIndex++
			partAdded := map[string]interface{}{
				"type":          "response.content_part.added",
				"output_index":  outputIdx,
				"content_index": contentIdx,
				"part":          map[string]interface{}{"type": "output_text", "text": ""},
			}
			output = append(output, sseEvent("response.content_part.added", mustJSON(partAdded))...)
			oi := outputIdx
			s.openTextOutputIdx = &oi

		case "tool_use":
			s.funcCallItems[outputIdx] = true
			callID := getString(block, "id")
			callName := getString(block, "name")
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
			// output_item.done (function_call)
			output = append(output, sseEvent("response.output_item.done", mustJSON(map[string]interface{}{
				"type":         "response.output_item.done",
				"output_index": outputIdx,
				"item": map[string]interface{}{
					"type": "function_call",
				},
			}))...)
		} else {
			// content_part.done + output_item.done (text)
			output = append(output, sseEvent("response.content_part.done", mustJSON(map[string]interface{}{
				"type":          "response.content_part.done",
				"output_index":  outputIdx,
				"content_index": 0,
			}))...)
			output = append(output, sseEvent("response.output_item.done", mustJSON(map[string]interface{}{
				"type":         "response.output_item.done",
				"output_index": outputIdx,
				"item": map[string]interface{}{
					"type": "message",
				},
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

	// 关闭所有仍打开的 output items
	for outputIdx := range s.openOutputIndices {
		if s.funcCallItems[outputIdx] {
			output = append(output, sseEvent("response.output_item.done", mustJSON(map[string]interface{}{
				"type":         "response.output_item.done",
				"output_index": outputIdx,
				"item":         map[string]interface{}{"type": "function_call"},
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
				"item":         map[string]interface{}{"type": "message"},
			}))...)
		}
	}

	status := "completed"
	if s.pendingStopReason == "max_tokens" {
		status = "incomplete"
	}
	usage := map[string]interface{}{"input_tokens": 0, "output_tokens": 0}
	if s.pendingUsage != nil {
		usage = buildResponsesUsageFromAnthropic(s.pendingUsage)
	}
	// response.completed
	completed := map[string]interface{}{
		"type": "response.completed",
		"response": map[string]interface{}{
			"id":     s.messageID,
			"object": "response",
			"model":  s.model,
			"status": status,
			"output": []interface{}{},
			"usage":  usage,
		},
	}
	output = append(output, sseEvent("response.completed", mustJSON(completed))...)
	return output
}

// ============================================================================
// Anthropic SSE → Gemini SSE（新写 inverse）
// ============================================================================

type anthropicToGeminiStream struct {
	buffer            bytes.Buffer
	messageID         string
	model             string
	accumulatedText   strings.Builder
	toolCalls         []map[string]interface{}
	toolArgBuf        []strings.Builder // 每个 tool call 的 partial_json 累积
	currentToolIdx    int               // 当前 input_json_delta 对应的工具索引，-1 表示无
	pendingStopReason string
	pendingUsage      map[string]interface{}
	hasSentFinal      bool
}

func (s *anthropicToGeminiStream) Feed(chunk []byte) ([]byte, error) {
	s.buffer.Write(chunk)
	var output []byte

	for {
		block, ok := takeSSEBlock(&s.buffer)
		if !ok {
			break
		}
		if strings.TrimSpace(block) == "" {
			continue
		}

		var eventType, dataStr string
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimRight(line, "\r")
			if e, ok := stripSSEField(line, "event"); ok {
				eventType = strings.TrimSpace(e)
			} else if d, ok := stripSSEField(line, "data"); ok {
				dataStr = d
			}
		}
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
		}

	case "content_block_start":
		block := getMap(data, "content_block")
		if block != nil && getString(block, "type") == "tool_use" {
			tc := map[string]interface{}{
				"name": getString(block, "name"),
			}
			id := getString(block, "id")
			if id != "" && !isSynthesizedToolCallID(id) {
				tc["id"] = id
			}
			s.toolCalls = append(s.toolCalls, tc)
			s.toolArgBuf = append(s.toolArgBuf, strings.Builder{})
			s.currentToolIdx = len(s.toolCalls) - 1
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
			partial := getString(delta, "partial_json")
			if partial != "" && s.currentToolIdx >= 0 && s.currentToolIdx < len(s.toolArgBuf) {
				s.toolArgBuf[s.currentToolIdx].WriteString(partial)
			}
		}

	case "content_block_stop":
		s.currentToolIdx = -1

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
		usage = buildGeminiUsageFromAnthropic(s.pendingUsage)
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
	upstream  io.ReadCloser
	converter StreamConverter
	buf       bytes.Buffer
	done      bool
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
		// 缓冲区有数据 → 立即返回
		if r.buf.Len() > 0 {
			return r.buf.Read(p)
		}
		if r.done {
			return 0, io.EOF
		}

		// 从 upstream 读取（阻塞直到有数据或 EOF/错误）
		tmpBuf := make([]byte, 4096)
		n, err := r.upstream.Read(tmpBuf)
		if n > 0 {
			out, convErr := r.converter.Feed(tmpBuf[:n])
			if convErr != nil {
				return 0, fmt.Errorf("stream convert: %w", convErr)
			}
			r.buf.Write(out)
		}

		if err != nil {
			if err == io.EOF {
				// 刷新转换器（输出残留状态，如 message_stop 等）
				flushOut, flushErr := r.converter.Flush()
				if flushErr != nil {
					return 0, fmt.Errorf("stream flush: %w", flushErr)
				}
				r.buf.Write(flushOut)
				r.done = true
				// 不在此处 Close upstream，由 Close() 统一处理，避免双重关闭
				if r.buf.Len() > 0 {
					return r.buf.Read(p)
				}
				return 0, io.EOF
			}
			return 0, err
		}

		// converter 可能因等待完整 SSE 事件而未输出 → 循环继续读 upstream，
		// 避免 (0, nil) 导致调用方 busy loop。
	}
}

func (r *transformStreamReader) Close() error {
	return r.upstream.Close()
}
