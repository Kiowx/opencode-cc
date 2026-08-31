package proxy

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ---- Anthropic SSE wire payloads (exact field names) ----
//
// Each is marshalled and wrapped as:
//   event: <Type>\n
//   data: <json>\n\n

type sseMessageStartData struct {
	Type string `json:"type"`
}

// streamMessageStart is the full {"type":"message_start","message":{...}}.
type streamMessageStart struct {
	Type    string        `json:"type"`
	Message streamMessage `json:"message"`
}

type streamMessage struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Model        string         `json:"model"`
	StopReason   *string        `json:"stop_reason"`
	StopSequence *string        `json:"stop_sequence"`
	Usage        AnthropicUsage `json:"usage"`
	Content      []any          `json:"content"`
}

type streamContentBlockStart struct {
	Type         string           `json:"type"`
	Index        int              `json:"index"`
	ContentBlock streamContentRef `json:"content_block"`
}

// streamContentRef is the minimal block description sent on block start. For
// tool_use we send {type,id,name,input:{}}; the body comes via input_json_delta.
type streamContentRef struct {
	Type     string         `json:"type"`
	Text     *string        `json:"text,omitempty"`
	Thinking *string        `json:"thinking,omitempty"`
	ID       string         `json:"id,omitempty"`
	Name     string         `json:"name,omitempty"`
	Input    jsonRawMessage `json:"input,omitempty"`
}

type streamContentBlockDelta struct {
	Type  string      `json:"type"`
	Index int         `json:"index"`
	Delta streamDelta `json:"delta"`
}

// streamDelta carries either text_delta or input_json_delta.
type streamDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
}

type streamContentBlockStop struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type streamMessageDelta struct {
	Type  string            `json:"type"`
	Delta streamMessageBody `json:"delta"`
	Usage streamDeltaUsage  `json:"usage"`
}

type streamMessageBody struct {
	StopReason   *string `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
}

type streamDeltaUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

type streamMessageStop struct {
	Type string `json:"type"`
}

type streamPing struct {
	Type string `json:"type"`
}

type streamErrorEvent struct {
	Type  string         `json:"type"`
	Error AnthropicError `json:"error"`
}

// ---- The converter ----

// blockState tracks one in-progress content block so we can emit
// content_block_stop when it changes or ends.
type blockState struct {
	index  int
	kind   string // "text" | "thinking" | "tool_use"
	toolid string
}

// StreamConverter maintains state while translating an OpenAI SSE stream into
// an Anthropic SSE stream.
type StreamConverter struct {
	w       *bufio.Writer
	model   string
	stopSeq *string

	cur              *blockState
	nextIdx          int
	input            int // input tokens (from final usage chunk, if upstream sends one)
	output           int // output tokens tally (from final usage chunk)
	cachedInput      int // cache-read input tokens (from final usage chunk)
	restrictTools    bool
	allowedTools     map[string]struct{}
	acceptedToolCall bool
	reasoning        strings.Builder
	reasoningPresent bool
	toolIDs          []string

	// OpenAI streams send finish_reason on the last content chunk and usage in
	// a trailing empty-choices chunk. We remember the finish reason and emit
	// message_delta/message_stop only when the stream ends, so usage is always
	// included.
	pendingFinish string
	finalized     bool
}

// NewStreamConverter wraps w (the HTTP response body writer) and emits the
// leading message_start event immediately. model is echoed in events; stopSeq
// is forwarded as stop_sequence (nil = none).
func NewStreamConverter(w io.Writer, model string, stopSeq *string) (*StreamConverter, error) {
	c := &StreamConverter{
		w:       bufio.NewWriter(w),
		model:   model,
		stopSeq: stopSeq,
	}
	if err := c.emitMessageStart(); err != nil {
		return nil, err
	}
	if err := c.emitPing(); err != nil {
		return nil, err
	}
	return c, nil
}

// RestrictTools limits emitted tool_use blocks to names declared by the
// incoming Anthropic request. Passing an empty slice rejects every upstream
// tool call, which prevents clients from trying to execute hallucinated tools.
func (c *StreamConverter) RestrictTools(names []string) {
	c.restrictTools = true
	c.allowedTools = make(map[string]struct{}, len(names))
	for _, name := range names {
		if name != "" {
			c.allowedTools[name] = struct{}{}
		}
	}
}

func (c *StreamConverter) emitMessageStart() error {
	payload := streamMessageStart{
		Type: "message_start",
		Message: streamMessage{
			ID:           "msg_" + randHex(24),
			Type:         "message",
			Role:         "assistant",
			Model:        c.model,
			StopReason:   nil, // null at message_start per spec; set later in message_delta
			StopSequence: c.stopSeq,
			Usage:        AnthropicUsage{InputTokens: 0, OutputTokens: 0},
			Content:      []any{},
		},
	}
	return c.writeEvent("message_start", payload)
}

func (c *StreamConverter) emitPing() error {
	return c.writeEvent("ping", streamPing{Type: "ping"})
}

// HandleChunk processes one decoded OpenAI streaming chunk and emits the
// corresponding Anthropic events. It must be called for every chunk in order.
func (c *StreamConverter) HandleChunk(chunk *OpenAIStreamChunk) error {
	if chunk == nil {
		return nil
	}

	// Usage typically arrives in a final empty-choices chunk.
	if chunk.Usage != nil {
		c.input = chunk.Usage.PromptTokens
		c.output = chunk.Usage.CompletionTokens
		c.cachedInput = chunk.Usage.CachedPromptTokens()
	}

	for _, ch := range chunk.Choices {
		// 1. reasoning_content is provider-required hidden thinking state.
		// Emit it as an Anthropic thinking block so Claude Code can replay it
		// on the next request without mixing it into user-visible text.
		if ch.Delta.ReasoningContentSet || ch.Delta.ReasoningContent != "" {
			c.reasoningPresent = true
		}
		if ch.Delta.ReasoningContent != "" {
			if err := c.handleThinking(ch.Delta.ReasoningContent); err != nil {
				return err
			}
		}
		// 2. Text delta — this is the model's actual reply.
		if ch.Delta.Content != "" {
			if err := c.handleText(ch.Delta.Content); err != nil {
				return err
			}
		}
		// 3. Tool call deltas.
		for _, tc := range ch.Delta.ToolCalls {
			name, ok := c.canonicalToolName(tc.Function.Name)
			if !ok {
				continue
			}
			tc.Function.Name = name
			if err := c.handleToolCall(tc); err != nil {
				return err
			}
		}
		// Legacy OpenAI-compatible providers may stream function_call instead
		// of tool_calls. Treat it as a single function tool call.
		if ch.Delta.FunctionCall != nil {
			tc := OpenAIToolCall{Type: "function", Function: *ch.Delta.FunctionCall}
			name, ok := c.canonicalToolName(tc.Function.Name)
			if ok {
				tc.Function.Name = name
				if err := c.handleToolCall(tc); err != nil {
					return err
				}
			}
		}
		// 4. Role-only first delta (no content) — nothing to emit.
		if ch.Delta.Role != "" && ch.Delta.Content == "" &&
			len(ch.Delta.ToolCalls) == 0 && ch.Delta.FunctionCall == nil {
			continue
		}
		// 5. Finish reason — remember it; we finalize at stream end so the
		// trailing usage chunk (if any) is captured.
		if ch.FinishReason != nil {
			c.pendingFinish = *ch.FinishReason
		}
	}
	return nil
}

func (c *StreamConverter) handleThinking(text string) error {
	c.reasoning.WriteString(text)
	if c.cur == nil || c.cur.kind != "thinking" {
		if err := c.closeCurrent(); err != nil {
			return err
		}
		idx := c.nextIdx
		c.nextIdx++
		c.cur = &blockState{index: idx, kind: "thinking"}
		if err := c.writeEvent("content_block_start", streamContentBlockStart{
			Type:         "content_block_start",
			Index:        idx,
			ContentBlock: streamContentRef{Type: "thinking", Thinking: stringRef("")},
		}); err != nil {
			return err
		}
	}
	return c.writeEvent("content_block_delta", streamContentBlockDelta{
		Type:  "content_block_delta",
		Index: c.cur.index,
		Delta: streamDelta{Type: "thinking_delta", Thinking: text},
	})
}

// handleText emits text_delta events, opening a text block if needed.
func (c *StreamConverter) handleText(text string) error {
	if c.cur == nil || c.cur.kind != "text" {
		if err := c.closeCurrent(); err != nil {
			return err
		}
		idx := c.nextIdx
		c.nextIdx++
		c.cur = &blockState{index: idx, kind: "text"}
		if err := c.writeEvent("content_block_start", streamContentBlockStart{
			Type:         "content_block_start",
			Index:        idx,
			ContentBlock: streamContentRef{Type: "text", Text: stringRef("")},
		}); err != nil {
			return err
		}
	}
	return c.writeEvent("content_block_delta", streamContentBlockDelta{
		Type:  "content_block_delta",
		Index: c.cur.index,
		Delta: streamDelta{Type: "text_delta", Text: text},
	})
}

func stringRef(s string) *string {
	return &s
}

// handleToolCall opens a tool_use block on first sight of a tool call id, then
// forwards argument fragments as input_json_delta.
//
// IMPORTANT: continuation detection uses the RAW upstream id (tc.ID), because
// OpenAI ids are not toolu_-prefixed and ensureToolID generates a fresh random
// id on every call (so it can't be used to match deltas across one call). We
// only normalise to toolu_ at the moment we emit content_block_start, then keep
// the normalised id on the block so the next tool_result can echo it.
func (c *StreamConverter) handleToolCall(tc OpenAIToolCall) error {
	rawID := tc.ID
	name := tc.Function.Name
	args := tc.Function.Arguments

	// A brand-new tool call: name and/or id present, no current tool block,
	// or the current block is a different tool id.
	startedNew := false
	if c.cur == nil || c.cur.kind != "tool_use" || c.cur.toolid != rawID {
		// If we only have argument fragments (some upstreams reuse the id
		// across deltas with the same index), keep the current block.
		if c.cur != nil && c.cur.kind == "tool_use" && name == "" && args != "" && rawID == "" {
			// continuation of existing tool call (id-less continuation deltas)
		} else {
			if err := c.closeCurrent(); err != nil {
				return err
			}
			normalised := ensureToolID(rawID)
			idx := c.nextIdx
			c.nextIdx++
			c.cur = &blockState{index: idx, kind: "tool_use", toolid: rawID}
			c.toolIDs = append(c.toolIDs, normalised)
			c.acceptedToolCall = true
			if err := c.writeEvent("content_block_start", streamContentBlockStart{
				Type:  "content_block_start",
				Index: idx,
				ContentBlock: streamContentRef{
					Type:  "tool_use",
					ID:    normalised,
					Name:  name,
					Input: jsonRawMessage(`{}`),
				},
			}); err != nil {
				return err
			}
			startedNew = true
		}
	}

	if args != "" {
		_ = startedNew
		if err := c.writeEvent("content_block_delta", streamContentBlockDelta{
			Type:  "content_block_delta",
			Index: c.cur.index,
			Delta: streamDelta{Type: "input_json_delta", PartialJSON: args},
		}); err != nil {
			return err
		}
	}
	return nil
}

func (c *StreamConverter) canonicalToolName(name string) (string, bool) {
	if !c.restrictTools {
		return name, true
	}
	if name == "" {
		// Empty names are continuation deltas and are only valid while an
		// accepted tool block is already open.
		return "", c.cur != nil && c.cur.kind == "tool_use"
	}
	if _, ok := c.allowedTools[name]; ok {
		return name, true
	}
	for allowed := range c.allowedTools {
		if strings.EqualFold(allowed, name) {
			return allowed, true
		}
	}
	return "", false
}

// closeCurrent emits content_block_stop for the active block, if any.
func (c *StreamConverter) closeCurrent() error {
	if c.cur == nil {
		return nil
	}
	err := c.writeEvent("content_block_stop", streamContentBlockStop{
		Type:  "content_block_stop",
		Index: c.cur.index,
	})
	c.cur = nil
	return err
}

// Finalize closes any open block and emits message_delta + message_stop. It
// must be called exactly once when the upstream stream ends (or errors out),
// so the trailing usage chunk — if the upstream sent one — is reflected in the
// message_delta usage. Idempotent.
func (c *StreamConverter) Finalize(stopReason string) error {
	if c.finalized {
		return nil
	}
	c.finalized = true
	if err := c.closeCurrent(); err != nil {
		return err
	}
	cacheReasoningStateForToolCalls(c.reasoning.String(), c.reasoningPresent, c.toolIDs...)
	// Prefer the finish_reason the upstream reported; fall back to the caller's
	// stopReason (e.g. "stream_error").
	reason := c.pendingFinish
	if reason == "" {
		reason = "stop"
	}
	if c.acceptedToolCall && reason == "stop" {
		reason = "tool_calls"
	}
	if (reason == "tool_calls" || reason == "function_call") && !c.acceptedToolCall {
		reason = "stop"
	}
	stop := mapFinishReason(reason)
	payload := streamMessageDelta{
		Type:  "message_delta",
		Delta: streamMessageBody{StopReason: stop, StopSequence: c.stopSeq},
		// Usage is emitted unconditionally (output_tokens may be 0 if the
		// upstream never reported usage). Claude Code relies on its presence.
		Usage: streamDeltaUsage{
			InputTokens:              c.input,
			CacheCreationInputTokens: 0,
			CacheReadInputTokens:     c.cachedInput,
			OutputTokens:             c.output,
		},
	}
	if err := c.writeEvent("message_delta", payload); err != nil {
		return err
	}
	return c.writeEvent("message_stop", streamMessageStop{Type: "message_stop"})
}

// mapFinishReason mirrors response.go but returns a pointer.
func mapFinishReason(reason string) *string {
	var s string
	switch reason {
	case "stop":
		s = "end_turn"
	case "length":
		s = "max_tokens"
	case "tool_calls", "function_call":
		s = "tool_use"
	default:
		s = "end_turn"
	}
	return &s
}

// EmitError writes an error event. Used when the upstream stream errors out.
func (c *StreamConverter) EmitError(typ, msg string) error {
	return c.writeEvent("error", streamErrorEvent{
		Type:  "error",
		Error: AnthropicError{Type: typ, Message: msg},
	})
}

// Flush flushes the underlying buffered writer.
func (c *StreamConverter) Flush() error { return c.w.Flush() }

// writeEvent marshals payload and writes the SSE framing for one event.
func (c *StreamConverter) writeEvent(event string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(c.w, "event: %s\ndata: %s\n\n", event, b); err != nil {
		return err
	}
	return c.w.Flush()
}

// ---- Upstream SSE parser ----

type OpenAIStreamScanResult struct {
	SawChunk  bool
	SawDone   bool
	SawFinish bool
}

// ScanOpenAIStream reads an OpenAI SSE stream from r and invokes onChunk for
// each decoded chunk. It returns io.EOF when the stream terminates cleanly.
// Some OpenAI-compatible upstreams end the HTTP stream without a final
// "data: [DONE]"; if at least one valid chunk was seen, that is accepted as a
// clean EOF.
func ScanOpenAIStream(r io.Reader, onChunk func(*OpenAIStreamChunk) error) error {
	_, err := ScanOpenAIStreamWithStatus(r, onChunk)
	return err
}

func ScanOpenAIStreamWithStatus(r io.Reader, onChunk func(*OpenAIStreamChunk) error) (OpenAIStreamScanResult, error) {
	sc := bufio.NewScanner(r)
	// Some upstreams send very large chunks; bump the buffer.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var result OpenAIStreamScanResult
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			// ignore event:/id:/comments
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			result.SawDone = true
			break
		}
		var chunk OpenAIStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			// Skip malformed line rather than killing the whole stream.
			continue
		}
		result.SawChunk = true
		for _, choice := range chunk.Choices {
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				result.SawFinish = true
			}
		}
		if err := onChunk(&chunk); err != nil {
			return result, err
		}
	}
	if err := sc.Err(); err != nil {
		return result, err
	}
	if !result.SawDone && !result.SawChunk {
		return result, errors.New("upstream stream ended without [DONE]")
	}
	return result, io.EOF
}
