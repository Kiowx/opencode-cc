package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ConvertResponse turns a non-streaming OpenAI Chat Completions response into
// an Anthropic Messages response. requestModel is the model name echoed back
// in the Anthropic response (the original incoming name).
func ConvertResponse(in *OpenAIResponse, requestModel string) *AnthropicResponse {
	out := &AnthropicResponse{
		ID:    "msg_" + stripPrefix(in.ID),
		Type:  "message",
		Role:  "assistant",
		Model: requestModel,
		Usage: AnthropicUsage{
			InputTokens:          in.Usage.PromptTokens,
			OutputTokens:         in.Usage.CompletionTokens,
			CacheReadInputTokens: in.Usage.CachedPromptTokens(),
		},
	}

	if len(in.Choices) == 0 {
		empty := ""
		out.StopReason = &empty
		return out
	}

	choice := in.Choices[0]
	hasToolUse := false
	if choice.Message != nil {
		if choice.Message.ReasoningContent != "" {
			out.Content = append(out.Content, AnthropicContent{
				Type:     "thinking",
				Thinking: choice.Message.ReasoningContent,
			})
		}
		// Text content — the model's actual reply.
		if txt := messageContentString(choice.Message); txt != "" {
			out.Content = append(out.Content, AnthropicContent{Type: "text", Text: txt})
		}
		// Tool calls.
		for _, tc := range choice.Message.ToolCalls {
			block := toolCallToBlock(tc)
			out.Content = append(out.Content, block)
			cacheReasoningForToolCalls(choice.Message.ReasoningContent, block.ID)
			hasToolUse = true
		}
		if choice.Message.FunctionCall != nil {
			block := toolCallToBlock(OpenAIToolCall{
				Type:     "function",
				Function: *choice.Message.FunctionCall,
			})
			out.Content = append(out.Content, block)
			cacheReasoningForToolCalls(choice.Message.ReasoningContent, block.ID)
			hasToolUse = true
		}
	}

	out.StopReason = finishReasonToStopForMessage(choice.FinishReason, hasToolUse)
	return out
}

// messageContentString extracts text from an assistant message, handling both
// string and []OpenAIContentPart content shapes.
func messageContentString(m *OpenAIMessage) string {
	if m == nil {
		return ""
	}
	switch v := m.Content.(type) {
	case string:
		return v
	case []OpenAIContentPart:
		var sb strings.Builder
		for _, p := range v {
			if p.Type == "text" {
				sb.WriteString(p.Text)
			}
		}
		return sb.String()
	case []any:
		var sb strings.Builder
		for _, item := range v {
			if mp, ok := item.(map[string]any); ok {
				if t, _ := mp["type"].(string); t == "text" {
					if s, _ := mp["text"].(string); s != "" {
						sb.WriteString(s)
					}
				}
			}
		}
		return sb.String()
	default:
		return ""
	}
}

// toolCallToBlock maps an OpenAI tool call to an Anthropic tool_use content
// block. Arguments is a JSON string on the OpenAI side and an object on the
// Anthropic side.
func toolCallToBlock(tc OpenAIToolCall) AnthropicContent {
	args := jsonRawMessage(tc.Function.Arguments)
	if len(args) == 0 {
		args = jsonRawMessage(`{}`)
	}
	// Validate; fall back to {} on parse error so we never send invalid JSON.
	var probe any
	if err := json.Unmarshal(args, &probe); err != nil {
		args = jsonRawMessage(`{}`)
	}
	return AnthropicContent{
		Type:  "tool_use",
		ID:    ensureToolID(tc.ID),
		Name:  tc.Function.Name,
		Input: args,
	}
}

// ensureToolID normalises a tool id to Anthropic's required shape: it MUST be
// non-empty and the SDK expects a "toolu_"-prefixed id (it keys tool_result
// matching off this). OpenAI ids like "chatcmpl-tool-..." pass through unchanged
// by the wire but break Claude Code's accumulator, so we rewrite anything that
// isn't already toolu_-prefixed to a fresh "toolu_"+24 hex chars. If the input
// is already toolu_-prefixed we keep it verbatim so a follow-up tool_result can
// reference the same id.
func ensureToolID(id string) string {
	if strings.HasPrefix(id, "toolu_") {
		return id
	}
	return "toolu_" + randHex(24)
}

// randHex returns n hex characters from a crypto-random source.
func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := readRand(b); err != nil {
		// Fallback: not crypto-grade but always available. Unlikely path.
		for i := range b {
			b[i] = byte(i * 7)
		}
	}
	const hexd = "0123456789abcdef"
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		out[i] = hexd[b[i/2]>>uint((i%2)*4)&0xf]
	}
	return string(out)
}

// finishReasonToStop maps OpenAI finish_reason to Anthropic stop_reason.
func finishReasonToStop(reason *string) *string {
	return finishReasonToStopForMessage(reason, false)
}

func finishReasonToStopForMessage(reason *string, hasToolUse bool) *string {
	if hasToolUse && (reason == nil || *reason == "stop") {
		s := "tool_use"
		return &s
	}
	if reason == nil {
		s := "end_turn"
		return &s
	}
	var mapped string
	switch *reason {
	case "stop":
		mapped = "end_turn"
	case "length":
		mapped = "max_tokens"
	case "tool_calls", "function_call":
		mapped = "tool_use"
	case "content_filter":
		mapped = "end_turn"
	default:
		mapped = "end_turn"
	}
	return &mapped
}

// stripPrefix removes a leading "chatcmpl-" or similar prefix for tidier ids.
func stripPrefix(id string) string {
	for _, p := range []string{"chatcmpl-", "chatcmpl"} {
		if strings.HasPrefix(id, p) {
			return strings.TrimPrefix(id, p)
		}
	}
	if id == "" {
		return "empty"
	}
	return id
}

// ParseOpenAIResponse decodes a non-streaming upstream body.
func ParseOpenAIResponse(b []byte) (*OpenAIResponse, error) {
	var r OpenAIResponse
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("decode openai response: %w", err)
	}
	return &r, nil
}

// AnthropicToOpenAIJSON converts a native Anthropic Messages response body to
// an OpenAI Chat Completions JSON body, suitable for local response-cache storage.
func AnthropicToOpenAIJSON(raw []byte) ([]byte, error) {
	var aresp AnthropicResponse
	if err := json.Unmarshal(raw, &aresp); err != nil {
		return nil, fmt.Errorf("decode anthropic response: %w", err)
	}

	var text, reasoning string
	var toolCalls []OpenAIToolCall
	for _, block := range aresp.Content {
		switch block.Type {
		case "text":
			text += block.Text
		case "thinking":
			reasoning += block.Thinking
		case "tool_use":
			args := block.Input
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			toolCalls = append(toolCalls, OpenAIToolCall{
				ID:   block.ID,
				Type: "function",
				Function: OpenAIFunctionCall{
					Name:      block.Name,
					Arguments: string(args),
				},
			})
		}
	}

	stopReason := ""
	if aresp.StopReason != nil {
		stopReason = *aresp.StopReason
	}
	finishReason := anthropicStopToFinish(stopReason, len(toolCalls) > 0)

	msg := OpenAIMessage{
		Role:    aresp.Role,
		Content: text,
	}
	if reasoning != "" {
		msg.ReasoningContent = reasoning
	}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}

	out := OpenAIResponse{
		ID: "msg_" + stripPrefix(aresp.ID),
		Choices: []OpenAIChoice{{
			Index:        0,
			Message:      &msg,
			FinishReason: &finishReason,
		}},
		Usage: OpenAIUsage{
			PromptTokens:     aresp.Usage.InputTokens,
			CompletionTokens: aresp.Usage.OutputTokens,
		},
	}
	if aresp.Usage.CacheReadInputTokens > 0 {
		out.Usage.PromptTokensDetails = &OpenAIPromptTokensDetails{
			CachedTokens: aresp.Usage.CacheReadInputTokens,
		}
	}

	return json.Marshal(out)
}

func anthropicStopToFinish(stop string, hasToolUse bool) string {
	if hasToolUse && (stop == "" || stop == "end_turn") {
		return "tool_calls"
	}
	switch stop {
	case "end_turn":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "stop_sequence":
		return "stop"
	default:
		return "stop"
	}
}
