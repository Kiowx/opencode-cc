package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// ConvertRequest turns an Anthropic Messages request into an OpenAI Chat
// Completions request. It performs a structural translation of:
//   - system prompt (string or blocks) -> first system message
//   - message content (text / image / tool_use / tool_result)
//   - tools and tool_choice
//   - sampling params (temperature, top_p, max_tokens, stop)
//
// resolveModel is called to map the incoming model name to an upstream target.
func ConvertRequest(in *AnthropicRequest, resolveModel func(string) string) *OpenAIRequest {
	out := &OpenAIRequest{
		Model:           resolveModel(in.Model),
		Stream:          in.Stream,
		Stop:            in.Stop,
		MaxTokens:       &in.MaxTokens,
		Temperature:     in.Temperature,
		TopP:            in.TopP,
		PromptCacheHint: AnthropicPromptCacheHint(in),
	}

	// System prompt first, if present.
	if msgs := buildSystemMessages(in.System); len(msgs) > 0 {
		out.Messages = append(out.Messages, msgs...)
	}

	for _, m := range in.Messages {
		out.Messages = append(out.Messages, convertMessage(m)...)
	}

	// Tools.
	if len(in.Tools) > 0 {
		out.Tools = make([]OpenAITool, 0, len(in.Tools))
		for _, t := range in.Tools {
			out.Tools = append(out.Tools, openAIToolFromAnthropic(t))
		}
		sortOpenAITools(out.Tools)
	} else {
		// Be explicit for models that may hallucinate tool calls even when the
		// client did not declare any tools. A tool_use block for an undeclared
		// name makes clients enter a repeated "tool not found" loop.
		out.ToolChoice = "none"
	}

	// Tool choice. Anthropic shapes: {"type":"auto"|"any"|"tool","name":...}.
	// Ignore tool_choice when there are no declarations: "none" must remain in
	// force or an inconsistent client request can re-enable hallucinated calls.
	if len(in.Tools) > 0 {
		switch in.ToolChoice.Type {
		case "auto":
			out.ToolChoice = "auto"
		case "any":
			out.ToolChoice = "required"
		case "tool":
			if in.ToolChoice.Name != "" {
				out.ToolChoice = map[string]any{
					"type":     "function",
					"function": map[string]string{"name": in.ToolChoice.Name},
				}
			} else {
				out.ToolChoice = "auto"
			}
		case "none":
			out.ToolChoice = "none"
		}
	}

	// Ask for usage in the final streamed chunk.
	if out.Stream {
		out.StreamOptions = &OpenAIStreamOptions{IncludeUsage: true}
	}

	return out
}

func openAIToolFromAnthropic(t AnthropicTool) OpenAITool {
	description := t.Description
	if description == "" && isAnthropicWebSearchTool(t) {
		description = "Search the web for current information. Use the query field for the search terms."
	}
	return OpenAITool{
		Type: "function",
		Function: OpenAIToolFunction{
			Name:        t.Name,
			Description: description,
			Parameters:  anthropicToolSchema(t),
		},
	}
}

func anthropicToolSchema(t AnthropicTool) jsonRawMessage {
	if isAnthropicWebSearchTool(t) {
		return jsonRawMessage(`{"additionalProperties":false,"properties":{"allowed_domains":{"description":"Optional list of domains that may be searched.","items":{"type":"string"},"type":"array"},"blocked_domains":{"description":"Optional list of domains to exclude from search.","items":{"type":"string"},"type":"array"},"query":{"description":"The search query.","type":"string"}},"required":["query"],"type":"object"}`)
	}
	return ensureObjectSchema(t.InputSchema)
}

func isAnthropicWebSearchTool(t AnthropicTool) bool {
	return IsAnthropicWebSearchTool(t)
}

func IsAnthropicWebSearchTool(t AnthropicTool) bool {
	typ := strings.ToLower(t.Type)
	name := strings.ToLower(t.Name)
	return strings.HasPrefix(typ, "web_search_") || (name == "web_search" && strings.HasPrefix(typ, "web_search"))
}

// buildSystemMessages turns the system field into one or more system messages.
// Multi-block system prompts are concatenated into a single string, since the
// OpenAI chat schema has no concept of multiple system blocks.
func buildSystemMessages(sys AnthropicSystem) []OpenAIMessage {
	if len(sys.Blocks) == 0 {
		return nil
	}
	var sb []byte
	for _, b := range sys.Blocks {
		if b.Text != "" {
			sb = append(sb, b.Text...)
			sb = append(sb, '\n')
		}
	}
	if len(sb) == 0 {
		return nil
	}
	// drop trailing newline
	if sb[len(sb)-1] == '\n' {
		sb = sb[:len(sb)-1]
	}
	return []OpenAIMessage{{Role: "system", Content: string(sb)}}
}

// convertMessage maps a single Anthropic message to one or more OpenAI
// messages. A user/assistant message with mixed content blocks may produce
// multiple OpenAI messages (e.g. tool_result blocks become separate
// role:"tool" messages followed by the remaining content as the user message).
func convertMessage(m AnthropicMessage) []OpenAIMessage {
	// Simple string content short-circuit.
	if m.Content.IsStr {
		return []OpenAIMessage{{Role: m.Role, Content: m.Content.Text}}
	}

	var out []OpenAIMessage
	switch m.Role {
	case "assistant":
		out = append(out, convertAssistantBlocks(m)...)
	case "user":
		out = append(out, convertUserBlocks(m)...)
	default:
		// Pass through as a single text message.
		out = append(out, OpenAIMessage{Role: m.Role, Content: contentBlocksToText(m.Content.Blocks)})
	}
	return out
}

// convertAssistantBlocks handles assistant messages, which may contain text and
// tool_use blocks. Tool calls are emitted as part of the assistant message.
func convertAssistantBlocks(m AnthropicMessage) []OpenAIMessage {
	msg := OpenAIMessage{Role: "assistant"}
	var toolCalls []OpenAIToolCall
	var parts []OpenAIContentPart
	hasText := false
	for _, b := range m.Content.Blocks {
		switch b.Type {
		case "text":
			parts = append(parts, OpenAIContentPart{Type: "text", Text: b.Text})
			hasText = true
		case "tool_use":
			toolCalls = append(toolCalls, OpenAIToolCall{
				ID:   b.ID,
				Type: "function",
				Function: OpenAIFunctionCall{
					Name:      b.Name,
					Arguments: compactJSON(b.Input),
				},
			})
		case "thinking":
			// Reasoning-capable OpenAI-compatible providers such as DeepSeek
			// require their previous reasoning_content to be replayed on the
			// assistant message. Claude Code sends that back as an Anthropic
			// thinking block, so preserve it structurally instead of emitting it
			// as user-visible text.
			msg.ReasoningContent += thinkingText(b)
		default:
			parts = append(parts, OpenAIContentPart{Type: "text", Text: b.Text})
			hasText = true
		}
	}
	if msg.ReasoningContent == "" && len(toolCalls) > 0 {
		msg.ReasoningContent = cachedReasoningForToolCalls(toolCallIDs(toolCalls))
	} else if msg.ReasoningContent != "" && len(toolCalls) > 0 {
		cacheReasoningForToolCalls(msg.ReasoningContent, toolCallIDs(toolCalls)...)
	}
	if len(parts) > 0 {
		if len(parts) == 1 {
			msg.Content = parts[0].Text
		} else {
			msg.Content = parts
		}
	} else if !hasText {
		// Assistant message with only tool calls still needs a (possibly
		// empty) content field for some upstreams; leave nil.
		msg.Content = ""
	}
	msg.ToolCalls = toolCalls
	return []OpenAIMessage{msg}
}

func toolCallIDs(calls []OpenAIToolCall) []string {
	ids := make([]string, 0, len(calls))
	for _, call := range calls {
		if call.ID != "" {
			ids = append(ids, call.ID)
		}
	}
	return ids
}

func thinkingText(b AnthropicContent) string {
	if b.Thinking != "" {
		return b.Thinking
	}
	return b.Text
}

// convertUserBlocks handles user messages. tool_result blocks become standalone
// role:"tool" messages; text/image blocks stay on the user message.
func convertUserBlocks(m AnthropicMessage) []OpenAIMessage {
	var out []OpenAIMessage
	var parts []OpenAIContentPart

	for _, b := range m.Content.Blocks {
		switch b.Type {
		case "tool_result":
			out = append(out, OpenAIMessage{
				Role:       "tool",
				ToolCallID: b.ToolUseID,
				Content:    toolResultText(b),
			})
		case "text":
			parts = append(parts, OpenAIContentPart{Type: "text", Text: b.Text})
		case "image":
			if part := imageBlockToPart(b); part != nil {
				parts = append(parts, *part)
			}
		default:
			parts = append(parts, OpenAIContentPart{Type: "text", Text: b.Text})
		}
	}

	if len(parts) > 0 {
		um := OpenAIMessage{Role: "user"}
		if len(parts) == 1 && parts[0].Type == "text" {
			um.Content = parts[0].Text
		} else {
			um.Content = parts
		}
		// The user message goes AFTER the tool results so the model sees them
		// in order. Append at the end.
		out = append(out, um)
	}
	return out
}

// imageBlockToPart maps an Anthropic image block to an OpenAI image_url part.
func imageBlockToPart(b AnthropicContent) *OpenAIContentPart {
	if b.Source == nil {
		return nil
	}
	var url string
	switch b.Source.Type {
	case "base64":
		url = fmt.Sprintf("data:%s;base64,%s", b.Source.MediaType, b.Source.Data)
	case "url":
		url = b.Source.URL
	default:
		return nil
	}
	return &OpenAIContentPart{
		Type:     "image_url",
		ImageURL: &OpenAIImageURL{URL: url},
	}
}

// toolResultText flattens a tool_result block's content to a string.
func toolResultText(b AnthropicContent) string {
	if b.Content == nil {
		if b.IsError {
			return "[error]"
		}
		return ""
	}
	if b.Content.IsStr {
		if b.IsError {
			return "[error] " + b.Content.Text
		}
		return b.Content.Text
	}
	// Concatenate text blocks.
	var sb []byte
	for _, cb := range b.Content.Blocks {
		if cb.Type == "text" && cb.Text != "" {
			sb = append(sb, cb.Text...)
			sb = append(sb, '\n')
		}
	}
	return string(sb)
}

// contentBlocksToText joins text blocks into one string (best-effort).
func contentBlocksToText(blocks []AnthropicContent) string {
	var sb []byte
	for _, b := range blocks {
		if b.Text != "" {
			sb = append(sb, b.Text...)
			sb = append(sb, ' ')
		}
	}
	return string(sb)
}

// compactJSON re-serialises a JSON blob with no extraneous whitespace. If the
// input isn't valid JSON it is returned verbatim (used as a string).
func compactJSON(raw jsonRawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	canonical, ok := canonicalJSON(raw)
	if !ok {
		return string(raw)
	}
	return string(canonical)
}

// ensureObjectSchema guarantees the schema is a JSON object (the OpenAI spec
// requires {"type":"object", ...}). Anthropic schemas sometimes omit "type".
//
// The output is canonicalised via canonicalJSON so the byte form is stable
// regardless of the input key order — this keeps the tools prefix (which sits
// at the very start of the prompt) byte-stable for upstream prompt-cache hits.
func ensureObjectSchema(raw jsonRawMessage) jsonRawMessage {
	if len(raw) == 0 {
		return jsonRawMessage(`{"type":"object"}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var probe map[string]any
	if err := decoder.Decode(&probe); err != nil {
		// Not valid JSON; hand back a minimal object to be safe.
		return jsonRawMessage(`{"type":"object"}`)
	}
	if _, ok := probe["type"]; !ok {
		probe["type"] = "object"
	}
	// Re-serialise to bytes first, then run through canonicalJSON for
	// recursive, order-stable output (handles nested objects/arrays too).
	reenc, err := json.Marshal(probe)
	if err != nil {
		return jsonRawMessage(`{"type":"object"}`)
	}
	canonical, ok := canonicalJSON(reenc)
	if !ok {
		return reenc
	}
	return canonical
}

// canonicalJSON parses raw into a generic value (with json.Number preserved so
// numeric precision is not lost) and re-emits it with a deterministic byte
// form: object keys sorted recursively (including inside arrays), compact
// whitespace. Returns (bytes, true) on success, (nil, false) if raw is not a
// single valid JSON value (rejecting trailing content). Byte stability is what
// lets the upstream token-prefix prompt cache hit across rounds.
func canonicalJSON(raw jsonRawMessage) (jsonRawMessage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, false
	}
	out, err := json.Marshal(canonicalizeValue(value))
	if err != nil {
		return nil, false
	}
	return out, true
}

// canonicalizeValue returns v with all nested maps' keys sorted, so that
// json.Marshal produces a stable byte form. Numbers are kept as json.Number to
// avoid float re-encoding losing precision (e.g. large integers).
func canonicalizeValue(v any) any {
	switch val := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		ordered := make(map[string]any, len(val))
		for _, k := range keys {
			ordered[k] = canonicalizeValue(val[k])
		}
		// json.Marshal sorts map keys lexicographically, so the explicit
		// ordering above is preserved on output.
		return ordered
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = canonicalizeValue(item)
		}
		return out
	default:
		// string / json.Number / bool / nil — return as-is.
		return v
	}
}
