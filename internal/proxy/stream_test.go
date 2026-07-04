package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"testing"
)

// TestStreamConversion feeds a synthetic OpenAI SSE stream (text + tool call)
// through the converter and checks the emitted Anthropic events are well-formed
// and in the right order.
func TestStreamConversion(t *testing.T) {
	// Build a fake OpenAI stream.
	chunks := []OpenAIStreamChunk{
		{Choices: []OpenAIChoice{{Index: 0, Delta: OpenAIDelta{Role: "assistant"}}}},
		{Choices: []OpenAIChoice{{Index: 0, Delta: OpenAIDelta{Content: "Hello"}}}},
		{Choices: []OpenAIChoice{{Index: 0, Delta: OpenAIDelta{Content: ", world"}}}},
		{Choices: []OpenAIChoice{{Index: 0, Delta: OpenAIDelta{ToolCalls: []OpenAIToolCall{
			{ID: "call_1", Type: "function", Function: OpenAIFunctionCall{Name: "read_file", Arguments: `{"path"`}},
		}}}}},
		{Choices: []OpenAIChoice{{Index: 0, Delta: OpenAIDelta{ToolCalls: []OpenAIToolCall{
			{ID: "call_1", Type: "function", Function: OpenAIFunctionCall{Arguments: `:"a.txt"}`}},
		}}}}},
		{Choices: []OpenAIChoice{{Index: 0, FinishReason: strPtr("tool_calls")}}},
		{Usage: &OpenAIUsage{
			PromptTokens:     10,
			CompletionTokens: 20,
			PromptTokensDetails: &OpenAIPromptTokensDetails{
				CachedTokens: 6,
			},
		}},
	}

	var sb bytes.Buffer
	for _, c := range chunks {
		b, _ := json.Marshal(c)
		sb.WriteString("data: ")
		sb.Write(b)
		sb.WriteString("\n\n")
	}
	sb.WriteString("data: [DONE]\n\n")

	var out bytes.Buffer
	conv, err := NewStreamConverter(&out, "claude-test", nil)
	if err != nil {
		t.Fatalf("new converter: %v", err)
	}

	if err := ScanOpenAIStream(bytes.NewReader(sb.Bytes()), func(c *OpenAIStreamChunk) error {
		return conv.HandleChunk(c)
	}); err != nil && err != io.EOF {
		t.Fatalf("scan: %v", err)
	}
	if err := conv.Finalize("end_turn"); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	_ = conv.Flush()

	got := out.String()

	// Order checks.
	checks := []string{
		"event: message_start",
		"event: content_block_start", // text block
		"event: content_block_delta", // "Hello"
		"event: content_block_delta", // ", world"
		"event: content_block_stop",  // text block closes
		"event: content_block_start", // tool_use block
		"event: content_block_delta", // input_json_delta partial
		"event: content_block_stop",  // tool block closes
		"event: message_delta",
		"event: message_stop",
	}
	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output\n---OUTPUT---\n%s", want, got)
		}
	}

	// The stop reason should be tool_use.
	if !strings.Contains(got, `"stop_reason":"tool_use"`) {
		t.Errorf("expected stop_reason tool_use\n---OUTPUT---\n%s", got)
	}
	// Usage carried in message_delta.
	for _, want := range []string{
		`"input_tokens":10`,
		`"cache_creation_input_tokens":0`,
		`"cache_read_input_tokens":6`,
		`"output_tokens":20`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %s in message_delta\n---OUTPUT---\n%s", want, got)
		}
	}
}

func TestScanOpenAIStreamAcceptsEOFWithoutDoneAfterChunks(t *testing.T) {
	var seen int
	err := ScanOpenAIStream(strings.NewReader(`data: {"choices":[{"index":0,"delta":{"content":"OK"}}]}`+"\n\n"), func(c *OpenAIStreamChunk) error {
		seen++
		if len(c.Choices) != 1 || c.Choices[0].Delta.Content != "OK" {
			t.Fatalf("unexpected chunk: %+v", c)
		}
		return nil
	})
	if err != io.EOF {
		t.Fatalf("ScanOpenAIStream error = %v, want io.EOF", err)
	}
	if seen != 1 {
		t.Fatalf("seen chunks = %d, want 1", seen)
	}
}

func TestScanOpenAIStreamRejectsEOFWithoutChunks(t *testing.T) {
	err := ScanOpenAIStream(strings.NewReader("event: ping\n\n"), func(*OpenAIStreamChunk) error {
		t.Fatal("callback should not be called")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "without [DONE]") {
		t.Fatalf("ScanOpenAIStream error = %v, want missing DONE error", err)
	}
}

func TestStreamConversionRejectsUndeclaredToolCalls(t *testing.T) {
	var out bytes.Buffer
	conv, err := NewStreamConverter(&out, "test-model", nil)
	if err != nil {
		t.Fatal(err)
	}
	conv.RestrictTools(nil)

	finish := "tool_calls"
	if err := conv.HandleChunk(&OpenAIStreamChunk{Choices: []OpenAIChoice{{
		Delta: OpenAIDelta{ToolCalls: []OpenAIToolCall{{
			ID:   "call_missing",
			Type: "function",
			Function: OpenAIFunctionCall{
				Name:      "undeclared_tool",
				Arguments: `{"value":1}`,
			},
		}}},
		FinishReason: &finish,
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := conv.Finalize("end_turn"); err != nil {
		t.Fatal(err)
	}
	if err := conv.Flush(); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	if strings.Contains(got, "tool_use") || strings.Contains(got, "undeclared_tool") {
		t.Fatalf("undeclared tool leaked into Anthropic stream:\n%s", got)
	}
	if !strings.Contains(got, `"stop_reason":"end_turn"`) {
		t.Fatalf("stop reason was not normalized:\n%s", got)
	}
}

func TestOpenAIFunctionCallAcceptsJSONObjectArguments(t *testing.T) {
	var fc OpenAIFunctionCall
	if err := json.Unmarshal([]byte(`{"name":"Task","arguments":{"prompt":"inspect","description":"check"}}`), &fc); err != nil {
		t.Fatal(err)
	}
	if fc.Name != "Task" {
		t.Fatalf("name = %q, want Task", fc.Name)
	}
	if fc.Arguments != `{"description":"check","prompt":"inspect"}` {
		t.Fatalf("arguments = %q", fc.Arguments)
	}
}

func TestStreamConversionAcceptsLegacyFunctionCallAndCanonicalToolName(t *testing.T) {
	var out bytes.Buffer
	conv, err := NewStreamConverter(&out, "claude-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	conv.RestrictTools([]string{"Task"})

	if err := conv.HandleChunk(&OpenAIStreamChunk{Choices: []OpenAIChoice{{
		Delta: OpenAIDelta{FunctionCall: &OpenAIFunctionCall{
			Name:      "task",
			Arguments: `{"description":"check","prompt":"inspect"}`,
		}},
	}}}); err != nil {
		t.Fatal(err)
	}
	finish := "function_call"
	if err := conv.HandleChunk(&OpenAIStreamChunk{Choices: []OpenAIChoice{{
		FinishReason: &finish,
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := conv.Finalize("end_turn"); err != nil {
		t.Fatal(err)
	}
	if err := conv.Flush(); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	for _, want := range []string{
		`"type":"tool_use"`,
		`"name":"Task"`,
		`"partial_json":"{\"description\":\"check\",\"prompt\":\"inspect\"}"`,
		`"stop_reason":"tool_use"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q\n---OUTPUT---\n%s", want, got)
		}
	}
}

func TestStreamConversionToolCallWithStopFinishStillStopsAsToolUse(t *testing.T) {
	var out bytes.Buffer
	conv, err := NewStreamConverter(&out, "claude-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	conv.RestrictTools([]string{"web_search"})

	finish := "stop"
	if err := conv.HandleChunk(&OpenAIStreamChunk{Choices: []OpenAIChoice{{
		Delta: OpenAIDelta{ToolCalls: []OpenAIToolCall{{
			ID:   "call_web",
			Type: "function",
			Function: OpenAIFunctionCall{
				Name:      "web_search",
				Arguments: `{"query":"Claude Code web search"}`,
			},
		}}},
		FinishReason: &finish,
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := conv.Finalize("end_turn"); err != nil {
		t.Fatal(err)
	}
	if err := conv.Flush(); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	for _, want := range []string{
		`"type":"tool_use"`,
		`"name":"web_search"`,
		`"partial_json":"{\"query\":\"Claude Code web search\"}"`,
		`"stop_reason":"tool_use"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q\n---OUTPUT---\n%s", want, got)
		}
	}
}

// TestNonStreamConversion checks the non-streaming path end to end.
func TestNonStreamConversion(t *testing.T) {
	up := &OpenAIResponse{
		ID: "chatcmpl-abc",
		Choices: []OpenAIChoice{{
			Message:      &OpenAIMessage{Role: "assistant", Content: "Hi there"},
			FinishReason: strPtr("stop"),
		}},
		Usage: OpenAIUsage{PromptTokens: 5, CompletionTokens: 2},
	}
	out := ConvertResponse(up, "claude-test")
	if out.ID == "" || !strings.HasPrefix(out.ID, "msg_") {
		t.Errorf("bad id: %q", out.ID)
	}
	if len(out.Content) != 1 || out.Content[0].Type != "text" || out.Content[0].Text != "Hi there" {
		t.Errorf("bad content: %+v", out.Content)
	}
	if out.StopReason == nil || *out.StopReason != "end_turn" {
		t.Errorf("bad stop reason: %v", out.StopReason)
	}
	if out.Usage.InputTokens != 5 || out.Usage.OutputTokens != 2 {
		t.Errorf("bad usage: %+v", out.Usage)
	}
}

func TestNonStreamConversionToolCallWithStopFinishStopsAsToolUse(t *testing.T) {
	finish := "stop"
	up := &OpenAIResponse{
		ID: "chatcmpl-web",
		Choices: []OpenAIChoice{{
			Message: &OpenAIMessage{
				Role: "assistant",
				ToolCalls: []OpenAIToolCall{{
					ID:   "call_web",
					Type: "function",
					Function: OpenAIFunctionCall{
						Name:      "web_search",
						Arguments: `{"query":"Claude Code web search"}`,
					},
				}},
			},
			FinishReason: &finish,
		}},
	}

	out := ConvertResponse(up, "claude-test")
	if out.StopReason == nil || *out.StopReason != "tool_use" {
		t.Fatalf("stop_reason = %v, want tool_use", out.StopReason)
	}
	if len(out.Content) != 1 || out.Content[0].Type != "tool_use" || out.Content[0].Name != "web_search" {
		t.Fatalf("tool_use block = %+v", out.Content)
	}
}

func TestReasoningContentRoundTripsAsThinking(t *testing.T) {
	resetReasoningContentCacheForTest()
	up := &OpenAIResponse{
		ID: "chatcmpl-reasoning",
		Choices: []OpenAIChoice{{
			Message: &OpenAIMessage{
				Role:             "assistant",
				ReasoningContent: "hidden chain state",
				Content:          "visible answer",
			},
			FinishReason: strPtr("stop"),
		}},
	}
	resp := ConvertResponse(up, "claude-test")
	if len(resp.Content) != 2 {
		t.Fatalf("content blocks = %+v, want thinking + text", resp.Content)
	}
	if resp.Content[0].Type != "thinking" || resp.Content[0].Thinking != "hidden chain state" {
		t.Fatalf("thinking block was not preserved: %+v", resp.Content[0])
	}
	if resp.Content[1].Type != "text" || resp.Content[1].Text != "visible answer" {
		t.Fatalf("text block mismatch: %+v", resp.Content[1])
	}

	req := ConvertRequest(&AnthropicRequest{
		Model:     "deepseek-v4-flash",
		MaxTokens: 128,
		Messages: []AnthropicMessage{{
			Role: "assistant",
			Content: AnthropicMessageContent{Blocks: []AnthropicContent{
				{Type: "thinking", Thinking: "hidden chain state"},
				{Type: "text", Text: "visible answer"},
			}},
		}},
	}, func(model string) string { return model })
	if len(req.Messages) != 1 {
		t.Fatalf("messages = %+v", req.Messages)
	}
	if req.Messages[0].ReasoningContent != "hidden chain state" {
		t.Fatalf("reasoning_content = %q", req.Messages[0].ReasoningContent)
	}
	if req.Messages[0].Content != "visible answer" {
		t.Fatalf("content = %q", req.Messages[0].Content)
	}
}

func TestReasoningContentReplayedAfterCompaction(t *testing.T) {
	resetReasoningContentCacheForTest()
	finish := "tool_calls"
	up := &OpenAIResponse{
		ID: "chatcmpl-reasoning-tool",
		Choices: []OpenAIChoice{{
			Message: &OpenAIMessage{
				Role:             "assistant",
				ReasoningContent: "hidden tool state",
				Content:          "I will inspect the files.",
				ToolCalls: []OpenAIToolCall{{
					ID:   "call_original",
					Type: "function",
					Function: OpenAIFunctionCall{
						Name:      "shell_command",
						Arguments: `{"command":"Get-ChildItem"}`,
					},
				}},
			},
			FinishReason: &finish,
		}},
	}
	resp := ConvertResponse(up, "claude-test")
	var toolID string
	for _, block := range resp.Content {
		if block.Type == "tool_use" {
			toolID = block.ID
			break
		}
	}
	if toolID == "" {
		t.Fatalf("tool_use block missing: %+v", resp.Content)
	}

	req := ConvertRequest(&AnthropicRequest{
		Model:     "deepseek-v4-flash",
		MaxTokens: 128,
		Messages: []AnthropicMessage{{
			Role: "assistant",
			Content: AnthropicMessageContent{Blocks: []AnthropicContent{
				{Type: "text", Text: "I will inspect the files."},
				{Type: "tool_use", ID: toolID, Name: "shell_command", Input: jsonRawMessage(`{"command":"Get-ChildItem"}`)},
			}},
		}},
	}, func(model string) string { return model })
	if len(req.Messages) != 1 {
		t.Fatalf("messages = %+v", req.Messages)
	}
	if req.Messages[0].ReasoningContent != "hidden tool state" {
		t.Fatalf("reasoning_content = %q", req.Messages[0].ReasoningContent)
	}
}

func TestStreamConversionPreservesReasoningContent(t *testing.T) {
	var out bytes.Buffer
	conv, err := NewStreamConverter(&out, "claude-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := conv.HandleChunk(&OpenAIStreamChunk{Choices: []OpenAIChoice{{
		Delta: OpenAIDelta{ReasoningContent: "think first"},
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := conv.HandleChunk(&OpenAIStreamChunk{Choices: []OpenAIChoice{{
		Delta: OpenAIDelta{Content: "then answer"},
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := conv.Finalize("end_turn"); err != nil {
		t.Fatal(err)
	}
	if err := conv.Flush(); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	for _, want := range []string{
		`"content_block":{"type":"thinking","thinking":""}`,
		`"delta":{"type":"thinking_delta","thinking":"think first"}`,
		`"content_block":{"type":"text","text":""}`,
		`"delta":{"type":"text_delta","text":"then answer"}`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q\n---OUTPUT---\n%s", want, got)
		}
	}
	if strings.Index(got, `"type":"thinking"`) > strings.Index(got, `"type":"text"`) {
		t.Fatalf("thinking block should precede text block:\n%s", got)
	}
}

func TestStreamReasoningContentReplayedAfterCompaction(t *testing.T) {
	resetReasoningContentCacheForTest()
	var out bytes.Buffer
	conv, err := NewStreamConverter(&out, "claude-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := conv.HandleChunk(&OpenAIStreamChunk{Choices: []OpenAIChoice{{
		Delta: OpenAIDelta{ReasoningContent: "stream hidden state"},
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := conv.HandleChunk(&OpenAIStreamChunk{Choices: []OpenAIChoice{{
		Delta: OpenAIDelta{ToolCalls: []OpenAIToolCall{{
			ID:   "call_stream",
			Type: "function",
			Function: OpenAIFunctionCall{
				Name:      "shell_command",
				Arguments: `{"command":"pwd"}`,
			},
		}}},
		FinishReason: strPtr("tool_calls"),
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := conv.Finalize("tool_use"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	match := regexp.MustCompile(`"type":"tool_use","id":"([^"]+)"`).FindStringSubmatch(got)
	if len(match) != 2 {
		t.Fatalf("tool_use id missing in stream:\n%s", got)
	}
	req := ConvertRequest(&AnthropicRequest{
		Model:     "deepseek-v4-flash",
		MaxTokens: 128,
		Messages: []AnthropicMessage{{
			Role: "assistant",
			Content: AnthropicMessageContent{Blocks: []AnthropicContent{{
				Type: "tool_use", ID: match[1], Name: "shell_command", Input: jsonRawMessage(`{"command":"pwd"}`),
			}}},
		}},
	}, func(model string) string { return model })
	if req.Messages[0].ReasoningContent != "stream hidden state" {
		t.Fatalf("reasoning_content = %q", req.Messages[0].ReasoningContent)
	}
}

// TestRequestConversion checks tool_use/tool_result round trip in requests.
func TestRequestConversion(t *testing.T) {
	in := &AnthropicRequest{
		Model:      "claude-3-5-sonnet",
		System:     AnthropicSystem{Blocks: []AnthropicContent{{Type: "text", Text: "You are helpful"}}},
		MaxTokens:  1024,
		Tools:      []AnthropicTool{{Name: "run", InputSchema: jsonRawMessage(`{"type":"object","properties":{"cmd":{"type":"string"}}}`)}},
		ToolChoice: AnthropicToolChoice{Type: "auto"},
		Messages: []AnthropicMessage{
			{Role: "user", Content: AnthropicMessageContent{IsStr: true, Text: "do it"}},
			{Role: "assistant", Content: AnthropicMessageContent{Blocks: []AnthropicContent{
				{Type: "tool_use", ID: "t1", Name: "run", Input: jsonRawMessage(`{"cmd":"ls"}`)},
			}}},
			{Role: "user", Content: AnthropicMessageContent{Blocks: []AnthropicContent{
				{Type: "tool_result", ToolUseID: "t1", Content: &AnthropicMessageContent{IsStr: true, Text: "file1\nfile2"}},
			}}},
		},
	}
	out := ConvertRequest(in, func(s string) string { return "glm-4.6" })
	if out.Model != "glm-4.6" {
		t.Errorf("model not resolved: %q", out.Model)
	}
	if len(out.Messages) < 1 || out.Messages[0].Role != "system" {
		t.Errorf("system message missing: %+v", out.Messages)
	}
	// Expect a role:tool message for the tool_result.
	foundTool := false
	for _, m := range out.Messages {
		if m.Role == "tool" && m.ToolCallID == "t1" {
			foundTool = true
		}
	}
	if !foundTool {
		t.Errorf("tool message not produced: %+v", out.Messages)
	}
	// Tool def forwarded.
	if len(out.Tools) != 1 || out.Tools[0].Function.Name != "run" {
		t.Errorf("tool not forwarded: %+v", out.Tools)
	}
	if out.ToolChoice != "auto" {
		t.Errorf("tool_choice not mapped: %v", out.ToolChoice)
	}
}

func TestRequestConversionMapsAnthropicWebSearchServerTool(t *testing.T) {
	in := &AnthropicRequest{
		Model:     "claude-3-5-sonnet",
		MaxTokens: 1024,
		Tools: []AnthropicTool{{
			Type:    "web_search_20250305",
			Name:    "web_search",
			MaxUses: 8,
		}},
		ToolChoice: AnthropicToolChoice{Type: "tool", Name: "web_search"},
		Messages: []AnthropicMessage{{
			Role:    "user",
			Content: AnthropicMessageContent{IsStr: true, Text: "search it"},
		}},
	}
	out := ConvertRequest(in, func(model string) string { return "glm-5.2" })

	if len(out.Tools) != 1 || out.Tools[0].Function.Name != "web_search" {
		t.Fatalf("tools = %+v", out.Tools)
	}
	var schema struct {
		Type       string                     `json:"type"`
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(out.Tools[0].Function.Parameters, &schema); err != nil {
		t.Fatalf("schema was not valid JSON: %v", err)
	}
	if schema.Type != "object" || len(schema.Properties["query"]) == 0 {
		t.Fatalf("schema did not expose query: %s", out.Tools[0].Function.Parameters)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "query" {
		t.Fatalf("required = %+v, want [query]", schema.Required)
	}
	choiceJSON, _ := json.Marshal(out.ToolChoice)
	if !strings.Contains(string(choiceJSON), `"name":"web_search"`) {
		t.Fatalf("tool_choice was not mapped to web_search: %s", choiceJSON)
	}
}

func TestRequestConversionDisablesToolsWhenNoneDeclared(t *testing.T) {
	in := &AnthropicRequest{
		Model:      "deepseek-v4-flash",
		MaxTokens:  256,
		ToolChoice: AnthropicToolChoice{Type: "auto"},
		Messages: []AnthropicMessage{{
			Role:    "user",
			Content: AnthropicMessageContent{IsStr: true, Text: "hello"},
		}},
	}
	out := ConvertRequest(in, func(model string) string { return model })
	if out.ToolChoice != "none" {
		t.Fatalf("tool_choice = %#v, want none", out.ToolChoice)
	}
	if len(out.Tools) != 0 {
		t.Fatalf("unexpected tools: %+v", out.Tools)
	}
}

func TestRequestConversionCanonicalizesToolJSON(t *testing.T) {
	in := &AnthropicRequest{
		Model:     "deepseek-v4-flash",
		MaxTokens: 256,
		Tools: []AnthropicTool{{
			Name:        "run",
			InputSchema: jsonRawMessage(`{"properties":{"z":{"type":"string"},"a":{"type":"number"}},"type":"object"}`),
		}},
		Messages: []AnthropicMessage{{
			Role: "assistant",
			Content: AnthropicMessageContent{Blocks: []AnthropicContent{{
				Type:  "tool_use",
				ID:    "call_1",
				Name:  "run",
				Input: jsonRawMessage(`{"z":2,"a":1}`),
			}}},
		}},
	}

	out := ConvertRequest(in, func(model string) string { return model })
	if got := string(out.Tools[0].Function.Parameters); got != `{"properties":{"a":{"type":"number"},"z":{"type":"string"}},"type":"object"}` {
		t.Fatalf("schema was not canonicalized: %s", got)
	}
	if got := out.Messages[0].ToolCalls[0].Function.Arguments; got != `{"a":1,"z":2}` {
		t.Fatalf("tool arguments were not canonicalized: %s", got)
	}
}

func TestPromptCacheKeyUsesClaudeSessionHint(t *testing.T) {
	makeReq := func(sessionID, userText string) *OpenAIRequest {
		userID, _ := json.Marshal(map[string]string{"session_id": sessionID})
		metadata, _ := json.Marshal(map[string]string{"user_id": string(userID)})
		out := ConvertRequest(&AnthropicRequest{
			Model:     "claude-3-5-sonnet",
			MaxTokens: 128,
			System:    AnthropicSystem{Blocks: []AnthropicContent{{Type: "text", Text: "Stable system"}}},
			Metadata:  metadata,
			Messages: []AnthropicMessage{{
				Role:    "user",
				Content: AnthropicMessageContent{IsStr: true, Text: userText},
			}},
		}, func(model string) string { return "glm-5.2" })
		ApplyOpenAIPromptCache(out, PromptCacheOptions{
			Enabled:          true,
			KeyPrefix:        "test",
			NormalizePrompts: true,
		})
		return out
	}

	first := makeReq("session-a", "first turn")
	second := makeReq("session-a", "second turn")
	other := makeReq("session-b", "first turn")

	if first.PromptCacheKey == "" {
		t.Fatal("prompt_cache_key was not set")
	}
	if first.PromptCacheKey != second.PromptCacheKey {
		t.Fatalf("same session should keep the same key: %q vs %q", first.PromptCacheKey, second.PromptCacheKey)
	}
	if first.PromptCacheKey == other.PromptCacheKey {
		t.Fatalf("different sessions should not share the same key: %q", first.PromptCacheKey)
	}
}

func TestPromptCacheKeyUsesFirstMessagePrefixWithoutSessionHint(t *testing.T) {
	makeReq := func(userText string) *OpenAIRequest {
		out := &OpenAIRequest{
			Model: "glm-5.2",
			Messages: []OpenAIMessage{
				{Role: "system", Content: "Stable rules"},
				{Role: "user", Content: userText},
			},
		}
		ApplyOpenAIPromptCache(out, PromptCacheOptions{
			Enabled:          true,
			KeyPrefix:        "test",
			NormalizePrompts: true,
		})
		return out
	}

	stablePrefix := strings.Repeat("stable Claude Code context\n", promptCacheFirstMessagePrefixBytes/27+2)
	first := makeReq(stablePrefix + "turn A")
	second := makeReq(stablePrefix + "turn B")
	other := makeReq("different Claude Code context\n" + stablePrefix)

	if first.PromptCacheKey == "" {
		t.Fatal("prompt_cache_key was not set")
	}
	if first.PromptCacheKey != second.PromptCacheKey {
		t.Fatalf("same first-message prefix should keep the same key: %q vs %q", first.PromptCacheKey, second.PromptCacheKey)
	}
	if first.PromptCacheKey == other.PromptCacheKey {
		t.Fatalf("different first-message prefixes should not share the same key: %q", first.PromptCacheKey)
	}
}

func TestPromptCacheKeyUsesAnthropicCacheControlBoundary(t *testing.T) {
	cacheControl := jsonRawMessage(`{"type":"ephemeral"}`)
	makeReq := func(cachedToolResult, laterText string) *OpenAIRequest {
		out := ConvertRequest(&AnthropicRequest{
			Model:     "claude-3-5-sonnet",
			MaxTokens: 128,
			System: AnthropicSystem{Blocks: []AnthropicContent{{
				Type:         "text",
				Text:         "Stable system",
				CacheControl: cacheControl,
			}}},
			Messages: []AnthropicMessage{
				{
					Role: "user",
					Content: AnthropicMessageContent{Blocks: []AnthropicContent{{
						Type: "text",
						Text: "Stable Claude Code context",
					}}},
				},
				{
					Role: "assistant",
					Content: AnthropicMessageContent{Blocks: []AnthropicContent{{
						Type:  "tool_use",
						ID:    "toolu_read",
						Name:  "Read",
						Input: jsonRawMessage(`{"file_path":"/tmp/a.txt"}`),
					}}},
				},
				{
					Role: "user",
					Content: AnthropicMessageContent{Blocks: []AnthropicContent{
						{
							Type:         "tool_result",
							ToolUseID:    "toolu_read",
							Content:      &AnthropicMessageContent{IsStr: true, Text: cachedToolResult},
							CacheControl: cacheControl,
						},
						{Type: "text", Text: laterText},
					}},
				},
			},
		}, func(model string) string { return "glm-5.2" })
		ApplyOpenAIPromptCache(out, PromptCacheOptions{
			Enabled:          true,
			KeyPrefix:        "test",
			NormalizePrompts: true,
		})
		return out
	}

	first := makeReq("same cached file contents", "turn A")
	second := makeReq("same cached file contents", "turn B")
	other := makeReq("different cached file contents", "turn A")

	if first.PromptCacheKey == "" {
		t.Fatal("prompt_cache_key was not set")
	}
	if first.PromptCacheRetention != promptCacheRetention {
		t.Fatalf("prompt_cache_retention = %q", first.PromptCacheRetention)
	}
	if first.PromptCacheKey != second.PromptCacheKey {
		t.Fatalf("content after cache_control should not change key: %q vs %q", first.PromptCacheKey, second.PromptCacheKey)
	}
	if first.PromptCacheKey == other.PromptCacheKey {
		t.Fatalf("different cache_control prefixes should not share the same key: %q", first.PromptCacheKey)
	}
}

func strPtr(s string) *string { return &s }
