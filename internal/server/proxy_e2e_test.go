package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Kiowx/opencode-cc/internal/config"
	"github.com/Kiowx/opencode-cc/internal/proxy"
	"github.com/Kiowx/opencode-cc/internal/store"
)

// mockZen returns a test server that pretends to be Zen's /v1/chat/completions.
// It supports both non-streaming and streaming, and echoes what it received so
// the test can assert the converted OpenAI payload.
func mockZen(t *testing.T, stream bool, chunks []string) *httptest.Server {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		t.Logf("mock zen received: %s", string(gotBody))

		if !stream {
			// Non-streaming response with a tool call.
			resp := map[string]any{
				"id": "chatcmpl-test",
				"choices": []map[string]any{{
					"index": 0,
					"message": map[string]any{
						"role":    "assistant",
						"content": "Hello from Zen",
					},
					"finish_reason": "stop",
				}},
				"usage": map[string]any{
					"prompt_tokens":     12,
					"completion_tokens": 4,
					"total_tokens":      16,
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		// Streaming: write the provided chunks as SSE.
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, ch := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", ch)
			if flusher != nil {
				flusher.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	// Expose the captured body via a closure for the test.
	t.Cleanup(func() {
		_ = gotBody
		srv.Close()
	})
	return srv
}

func newTestServer(t *testing.T, upstream string) (*Server, *store.Store) {
	cfg := config.Default()
	cfg.UpstreamBase = strings.TrimRight(upstream, "/")
	cfg.ZenAPIKey = "test-key"
	cfg.NativeAnthropic = false
	cfg.ModelMappings = []config.ModelMapping{{Match: "*", Target: "glm-4.6"}}
	return newTestServerWithCfg(t, cfg)
}

// newTestServerWithCfg builds a Server from an explicit, fully-configured cfg.
// Shared by tests that need non-default upstream setups (e.g. multi-upstream
// round-robin).
func newTestServerWithCfg(t *testing.T, cfg *config.Config) (*Server, *store.Store) {
	tmp := t.TempDir()
	st, err := store.Open(tmp + "/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return New(cfg, st), st
}

func TestProxySticksSessionToSameUpstream(t *testing.T) {
	counts := map[string]int{"a": 0, "b": 0}
	newBackend := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			counts[name]++
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{
				"id":"chatcmpl-sticky",
				"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}
			}`)
		}))
	}
	zenA := newBackend("a")
	defer zenA.Close()
	zenB := newBackend("b")
	defer zenB.Close()

	cfg := config.Default()
	cfg.UpstreamBase = ""
	cfg.ZenAPIKey = ""
	cfg.Upstreams = []config.Upstream{
		{BaseURL: zenA.URL, APIKey: "key-a", Enabled: true},
		{BaseURL: zenB.URL, APIKey: "key-b", Enabled: true},
	}
	cfg.NativeAnthropic = false
	cfg.ModelMappings = []config.ModelMapping{{Match: "*", Target: "glm-4.6"}}
	srv, _ := newTestServerWithCfg(t, cfg)

	body := []byte(`{
		"model":"client-model",
		"max_tokens":64,
		"metadata":{"session_id":"sticky-session"},
		"messages":[{"role":"user","content":"hi"}]
	}`)
	for i := 0; i < 4; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		srv.Proxy().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d status %d: %s", i, rec.Code, rec.Body.String())
		}
	}
	if counts["a"] != 4 && counts["b"] != 4 {
		t.Fatalf("session was not sticky across upstreams: %+v", counts)
	}
}

func TestProxyNonStreamEndToEnd(t *testing.T) {
	zen := mockZen(t, false, nil)
	srv, _ := newTestServer(t, zen.URL)

	// An Anthropic request with system + user + a tool.
	anthReq := map[string]any{
		"model":      "claude-3-5-sonnet-20241022",
		"max_tokens": 256,
		"system":     "You are concise.",
		"messages": []map[string]any{
			{"role": "user", "content": "hi"},
		},
	}
	body, _ := json.Marshal(anthReq)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	srv.Proxy()(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, rr.Body.String())
	}
	if resp["type"] != "message" {
		t.Errorf("expected type=message, got %v", resp["type"])
	}
	content, _ := resp["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("expected content blocks, got %v", resp["content"])
	}
	first, _ := content[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "Hello from Zen" {
		t.Errorf("unexpected first block: %v", first)
	}
	usage, _ := resp["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 12 || usage["output_tokens"].(float64) != 4 {
		t.Errorf("unexpected usage: %v", usage)
	}
}

func TestProxyReplaysReasoningContentOnFollowup(t *testing.T) {
	var calls int
	zen := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var upstreamReq struct {
			Messages []struct {
				Role             string `json:"role"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&upstreamReq); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if calls == 2 {
			found := false
			for _, msg := range upstreamReq.Messages {
				if msg.Role == "assistant" && msg.ReasoningContent == "deepseek hidden state" {
					found = true
				}
			}
			if !found {
				t.Fatalf("follow-up request did not replay reasoning_content: %+v", upstreamReq.Messages)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			_, _ = io.WriteString(w, `{
				"id":"chatcmpl-reasoning",
				"choices":[{
					"index":0,
					"message":{
						"role":"assistant",
						"reasoning_content":"deepseek hidden state",
						"content":"I will inspect the workspace.",
						"tool_calls":[{
							"id":"toolu_reasoning_cache",
							"type":"function",
							"function":{"name":"shell_command","arguments":"{\"command\":\"Get-ChildItem\"}"}
						}]
					},
					"finish_reason":"tool_calls"
				}],
				"usage":{"prompt_tokens":6,"completion_tokens":3,"total_tokens":9}
			}`)
			return
		}
		_, _ = io.WriteString(w, `{
			"id":"chatcmpl-reasoning",
			"choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":6,"completion_tokens":3,"total_tokens":9}
		}`)
	}))
	defer zen.Close()
	srv, _ := newTestServer(t, zen.URL)
	srv.cfg.ModelMappings = []config.ModelMapping{{Match: "*", Target: ""}}

	firstBody := []byte(`{
		"model":"deepseek-v4-flash",
		"max_tokens":256,
		"tools":[{"name":"shell_command","description":"run shell command","input_schema":{"type":"object","properties":{"command":{"type":"string"}}}}],
		"messages":[{"role":"user","content":"hi"}]
	}`)
	firstReq := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(firstBody))
	firstRec := httptest.NewRecorder()
	srv.Proxy().ServeHTTP(firstRec, firstReq)
	if firstRec.Code != http.StatusOK {
		t.Fatalf("first status %d: %s", firstRec.Code, firstRec.Body.String())
	}
	var firstResp proxy.AnthropicResponse
	if err := json.Unmarshal(firstRec.Body.Bytes(), &firstResp); err != nil {
		t.Fatalf("decode first response: %v", err)
	}
	if len(firstResp.Content) < 2 ||
		firstResp.Content[0].Type != "thinking" ||
		firstResp.Content[0].Thinking != "deepseek hidden state" {
		t.Fatalf("first response did not expose thinking block for replay: %+v", firstResp.Content)
	}
	var toolBlock proxy.AnthropicContent
	for _, block := range firstResp.Content {
		if block.Type == "tool_use" {
			toolBlock = block
			break
		}
	}
	if toolBlock.ID == "" {
		t.Fatalf("first response did not include tool_use: %+v", firstResp.Content)
	}

	secondBody, _ := json.Marshal(map[string]any{
		"model":      "deepseek-v4-flash",
		"max_tokens": 256,
		"tools": []map[string]any{{
			"name":        "shell_command",
			"description": "run shell command",
			"input_schema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"command": map[string]any{"type": "string"}},
			},
		}},
		"messages": []map[string]any{
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": []map[string]any{
				{"type": "text", "text": "I will inspect the workspace."},
				{"type": "tool_use", "id": toolBlock.ID, "name": toolBlock.Name, "input": map[string]any{"command": "Get-ChildItem"}},
			}},
			{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": toolBlock.ID, "content": "README.md"},
			}},
			{"role": "user", "content": "continue"},
		},
	})
	secondReq := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(secondBody))
	secondRec := httptest.NewRecorder()
	srv.Proxy().ServeHTTP(secondRec, secondReq)
	if secondRec.Code != http.StatusOK {
		t.Fatalf("second status %d: %s", secondRec.Code, secondRec.Body.String())
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
}

func TestProxyAppliesThinkingBudgetMappingForKimi(t *testing.T) {
	var got struct {
		Model           string `json:"model"`
		ThinkingBudget  *int   `json:"thinking_budget"`
		ReasoningEffort string `json:"reasoning_effort"`
	}
	zen := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"chatcmpl-kimi",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}
		}`)
	}))
	defer zen.Close()

	cfg := config.Default()
	cfg.UpstreamBase = zen.URL
	cfg.ZenAPIKey = "test-key"
	cfg.NativeAnthropic = false
	cfg.ModelMappings = []config.ModelMapping{{Match: "*", Target: ""}}
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	srv := New(cfg, st)

	body := []byte(`{
		"model":"anthropic/kimi-k2.7-code",
		"max_tokens":256,
		"thinking":{"type":"enabled","budget_tokens":20000},
		"messages":[{"role":"user","content":"hi"}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Proxy().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got.Model != "kimi-k2.7-code" {
		t.Fatalf("model = %q, want kimi-k2.7-code", got.Model)
	}
	if got.ThinkingBudget == nil || *got.ThinkingBudget != 16384 {
		t.Fatalf("thinking_budget = %v, want 16384", got.ThinkingBudget)
	}
	if got.ReasoningEffort != "" {
		t.Fatalf("reasoning_effort should not be set for Kimi mapping: %q", got.ReasoningEffort)
	}
}

func TestProxyAppliesThinkingObjectForGLM(t *testing.T) {
	var got struct {
		Model    string `json:"model"`
		Thinking *struct {
			Type          string `json:"type"`
			ClearThinking *bool  `json:"clear_thinking"`
			BudgetTokens  *int   `json:"budget_tokens"`
		} `json:"thinking"`
	}
	zen := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"chatcmpl-glm",
			"choices":[{"index":0,"message":{"role":"assistant","reasoning_content":"plan","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":2,"completion_tokens":2,"total_tokens":4}
		}`)
	}))
	defer zen.Close()

	cfg := config.Default()
	cfg.UpstreamBase = zen.URL
	cfg.ZenAPIKey = "test-key"
	cfg.NativeAnthropic = false
	cfg.ModelMappings = []config.ModelMapping{{Match: "*", Target: ""}}
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	srv := New(cfg, st)

	body := []byte(`{
		"model":"glm-5.2",
		"max_tokens":256,
		"messages":[{"role":"user","content":"hi"}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Proxy().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got.Model != "glm-5.2" {
		t.Fatalf("model = %q, want glm-5.2", got.Model)
	}
	if got.Thinking == nil ||
		got.Thinking.Type != "enabled" ||
		got.Thinking.ClearThinking != nil ||
		got.Thinking.BudgetTokens != nil {
		t.Fatalf("thinking object was not applied for GLM: %+v", got.Thinking)
	}
	if !strings.Contains(rec.Body.String(), `"type":"thinking"`) {
		t.Fatalf("reasoning_content was not returned as Anthropic thinking: %s", rec.Body.String())
	}
}

func TestProxyMapsGLMThinkingBudgetWithoutClearThinking(t *testing.T) {
	var got struct {
		Model    string `json:"model"`
		Thinking *struct {
			Type          string `json:"type"`
			BudgetTokens  *int   `json:"budget_tokens"`
			ClearThinking *bool  `json:"clear_thinking"`
		} `json:"thinking"`
	}
	zen := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"chatcmpl-glm-budget",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}
		}`)
	}))
	defer zen.Close()

	cfg := config.Default()
	cfg.UpstreamBase = zen.URL
	cfg.ZenAPIKey = "test-key"
	cfg.NativeAnthropic = false
	cfg.ModelMappings = []config.ModelMapping{{Match: "*", Target: ""}}
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	srv := New(cfg, st)

	body := []byte(`{
		"model":"glm-5.2",
		"max_tokens":256,
		"thinking":{"type":"enabled","budget_tokens":4096},
		"messages":[{"role":"user","content":"hi"}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Proxy().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got.Model != "glm-5.2" || got.Thinking == nil || got.Thinking.Type != "enabled" {
		t.Fatalf("thinking object was not applied for GLM: %+v", got)
	}
	if got.Thinking.BudgetTokens == nil || *got.Thinking.BudgetTokens != 4096 {
		t.Fatalf("budget_tokens = %v, want 4096", got.Thinking.BudgetTokens)
	}
	if got.Thinking.ClearThinking != nil {
		t.Fatalf("clear_thinking should be omitted, got %+v", got.Thinking.ClearThinking)
	}
}

func TestProxyStreamEndToEnd(t *testing.T) {
	chunks := []string{
		// role
		`{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		// text deltas
		`{"choices":[{"index":0,"delta":{"content":"Hello"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":", streamed"}}]}`,
		// finish
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		// usage
		`{"choices":[],"usage":{"prompt_tokens":8,"completion_tokens":3,"total_tokens":11,"prompt_tokens_details":{"cached_tokens":5}}}`,
	}
	zen := mockZen(t, true, chunks)
	srv, st := newTestServer(t, zen.URL)

	anthReq := map[string]any{
		"model":      "claude-3-5-sonnet-20241022",
		"max_tokens": 256,
		"stream":     true,
		"messages": []map[string]any{
			{"role": "user", "content": "hi"},
		},
	}
	body, _ := json.Marshal(anthReq)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	srv.Proxy()(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("expected event-stream content type, got %q", ct)
	}

	out := rr.Body.String()
	// Must contain the full event sequence and the usage in message_delta.
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		`"text_delta","text":"Hello"`,
		`"text_delta","text":", streamed"`,
		"event: content_block_stop",
		`"stop_reason":"end_turn"`,
		`"input_tokens":8`,
		`"cache_creation_input_tokens":0`,
		`"cache_read_input_tokens":5`,
		`"output_tokens":3`,
		"event: message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n---OUTPUT---\n%s", want, out)
		}
	}

	// The request should have been logged to the store (give the async write
	// a moment, then verify).
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rows, _ := st.ListRequests(ctx, 10)
		if len(rows) > 0 {
			if rows[0].TargetModel != "glm-4.6" {
				t.Errorf("logged wrong target model: %s", rows[0].TargetModel)
			}
			if rows[0].Stream != true {
				t.Errorf("stream flag not logged")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("request was not logged to the store")
}

func TestProxyMissingKey(t *testing.T) {
	zen := mockZen(t, false, nil)
	srv, _ := newTestServer(t, zen.URL)
	srv.cfg.SetZenAPIKey("") // simulate unconfigured key

	body, _ := json.Marshal(map[string]any{
		"model": "claude-x", "max_tokens": 8,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	srv.Proxy()(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "no upstream API key") {
		t.Errorf("unexpected error body: %s", rr.Body.String())
	}
}

func TestProxyNonStreamFiltersUndeclaredToolCall(t *testing.T) {
	zen := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var upstreamReq map[string]any
		if err := json.NewDecoder(r.Body).Decode(&upstreamReq); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if upstreamReq["tool_choice"] != "none" {
			t.Errorf("tool_choice = %v, want none", upstreamReq["tool_choice"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-rogue",
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"id":   "call_missing",
						"type": "function",
						"function": map[string]any{
							"name":      "undeclared_tool",
							"arguments": `{}`,
						},
					}},
				},
				"finish_reason": "tool_calls",
			}},
			"usage": map[string]any{},
		})
	}))
	defer zen.Close()
	srv, _ := newTestServer(t, zen.URL)

	body := []byte(`{
		"model":"deepseek-v4-flash",
		"max_tokens":256,
		"messages":[{"role":"user","content":"hello"}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Proxy().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp proxy.AnthropicResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	for _, block := range resp.Content {
		if block.Type == "tool_use" {
			t.Fatalf("undeclared tool call leaked: %+v", block)
		}
	}
	if resp.StopReason == nil || *resp.StopReason != "end_turn" {
		t.Fatalf("stop_reason = %v, want end_turn", resp.StopReason)
	}
}

func TestProxyNonStreamCanonicalizesDeclaredToolName(t *testing.T) {
	zen := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-task",
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"id":   "call_task",
						"type": "function",
						"function": map[string]any{
							"name":      "task",
							"arguments": map[string]any{"description": "check", "prompt": "inspect"},
						},
					}},
				},
				"finish_reason": "tool_calls",
			}},
			"usage": map[string]any{},
		})
	}))
	defer zen.Close()
	srv, _ := newTestServer(t, zen.URL)

	body := []byte(`{
		"model":"glm-5.2",
		"max_tokens":256,
		"tools":[{"name":"Task","input_schema":{"type":"object","properties":{"description":{"type":"string"},"prompt":{"type":"string"}}}}],
		"messages":[{"role":"user","content":"use subagent"}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Proxy().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp proxy.AnthropicResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != "tool_use" {
		t.Fatalf("content = %+v, want one tool_use", resp.Content)
	}
	if resp.Content[0].Name != "Task" {
		t.Fatalf("tool name = %q, want Task", resp.Content[0].Name)
	}
	if resp.StopReason == nil || *resp.StopReason != "tool_use" {
		t.Fatalf("stop_reason = %v, want tool_use", resp.StopReason)
	}
	if string(resp.Content[0].Input) != `{"description":"check","prompt":"inspect"}` {
		t.Fatalf("tool input = %s", resp.Content[0].Input)
	}
}

func TestProxyAuthenticatedUsageIsRecorded(t *testing.T) {
	zen := mockZen(t, false, nil)
	srv, st := newTestServer(t, zen.URL)
	srv.cfg.RequireAPIKey = true
	plain, err := st.CreateKey(context.Background(), store.KeyOpts{Name: "usage-test", Enabled: true})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	key, err := st.LookupKeyByHash(context.Background(), store.HashKey(plain))
	if err != nil || key == nil {
		t.Fatalf("lookup key: key=%+v err=%v", key, err)
	}

	body, _ := json.Marshal(map[string]any{
		"model":      "claude-test",
		"max_tokens": 32,
		"messages": []map[string]any{
			{"role": "user", "content": "hi"},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+plain)
	rr := httptest.NewRecorder()
	srv.clientAuth(srv.Proxy()).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, lookupErr := st.GetKey(context.Background(), key.ID)
		rows, rowsErr := st.ListRequests(context.Background(), 1)
		if lookupErr == nil && rowsErr == nil && got != nil &&
			got.UsedRequests == 1 && got.UsedTokens == 16 &&
			len(rows) == 1 && rows[0].APIKeyID == key.ID {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	got, _ := st.GetKey(context.Background(), key.ID)
	rows, _ := st.ListRequests(context.Background(), 1)
	t.Fatalf("usage was not recorded: key=%+v rows=%+v", got, rows)
}

func TestProxySmartNativeAnthropicUsesChatForOpenAIModels(t *testing.T) {
	var gotPath string
	var gotModel string
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		gotModel = body.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"chatcmpl-smart",
			"choices":[{"index":0,"message":{"role":"assistant","content":"chat path"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}
		}`)
	})
	_, _, httpSrv := newOpenAITestServer(t, upstream)

	body := `{"model":"client-model","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(httpSrv.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q, want /v1/chat/completions", gotPath)
	}
	if gotModel != "glm-5.1" {
		t.Fatalf("mapped model = %q, want glm-5.1", gotModel)
	}
	if !bytes.Contains(raw, []byte("chat path")) {
		t.Fatalf("converted response missing text: %s", raw)
	}
}

func TestProxyNativeAnthropicNonStream(t *testing.T) {
	var gotPath, gotAuth, gotAPIKey, gotAccept, gotVersion string
	var gotBody map[string]json.RawMessage
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("x-api-key")
		gotAccept = r.Header.Get("Accept")
		gotVersion = r.Header.Get("anthropic-version")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"msg_native",
			"type":"message",
			"role":"assistant",
			"model":"claude-sonnet-4-5",
			"content":[{"type":"text","text":"native hello"}],
			"stop_reason":"end_turn",
			"usage":{"input_tokens":9,"output_tokens":2}
		}`)
	})
	srv, st, httpSrv := newOpenAITestServer(t, upstream)
	srv.cfg.NativeAnthropic = true
	srv.cfg.ModelMappings = []config.ModelMapping{
		{Match: "client-model", Target: "claude-sonnet-4-5"},
		{Match: "*", Target: ""},
	}

	body := `{
		"model":"client-model",
		"max_tokens":64,
		"system":[{"type":"text","text":"stable","cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":"hi"}],
		"metadata":{"trace":"keep-me"}
	}`
	resp, err := http.Post(httpSrv.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("upstream path = %q, want /v1/messages", gotPath)
	}
	if gotAuth != "Bearer zen-test-key" || gotAPIKey != "zen-test-key" ||
		gotAccept != "application/json" || gotVersion != "2023-06-01" {
		t.Fatalf("upstream headers auth=%q x-api-key=%q accept=%q version=%q", gotAuth, gotAPIKey, gotAccept, gotVersion)
	}
	var model string
	if err := json.Unmarshal(gotBody["model"], &model); err != nil || model != "claude-sonnet-4-5" {
		t.Fatalf("mapped model = %q, err=%v", model, err)
	}
	if !bytes.Contains(gotBody["system"], []byte("cache_control")) {
		t.Fatalf("cache_control was not preserved: %s", gotBody["system"])
	}
	if !bytes.Contains(gotBody["metadata"], []byte("keep-me")) {
		t.Fatalf("unknown metadata was not preserved: %s", gotBody["metadata"])
	}
	if !bytes.Contains(raw, []byte("native hello")) {
		t.Fatalf("native response was not relayed: %s", raw)
	}

	waitForRequestLog(t, st, func(row store.RequestRow) bool {
		return row.Path == "/v1/messages" &&
			row.IncomingModel == "client-model" &&
			row.TargetModel == "claude-sonnet-4-5" &&
			row.InputTokens == 9 &&
			row.OutputTokens == 2 &&
			row.StopReason == "end_turn"
	})
}

func TestProxyNativeAnthropicStream(t *testing.T) {
	var expected strings.Builder
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if body.Model != "claude-sonnet-4-5" || !body.Stream {
			t.Fatalf("unexpected upstream request: %+v", body)
		}
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Fatalf("Accept = %q", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		events := []string{
			`event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[],"stop_reason":null,"usage":{"input_tokens":7,"output_tokens":0}}}

`,
			`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"native stream"}}

`,
			`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":3}}

`,
			`event: message_stop
data: {"type":"message_stop"}

`,
		}
		for _, event := range events {
			expected.WriteString(event)
			_, _ = io.WriteString(w, event)
			if flusher != nil {
				flusher.Flush()
			}
		}
	})
	srv, st, httpSrv := newOpenAITestServer(t, upstream)
	srv.cfg.NativeAnthropic = true
	srv.cfg.ModelMappings = []config.ModelMapping{
		{Match: "client-model", Target: "claude-sonnet-4-5"},
		{Match: "*", Target: ""},
	}

	body := `{"model":"client-model","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(httpSrv.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	if string(raw) != expected.String() {
		t.Fatalf("native SSE was modified:\ngot  %q\nwant %q", raw, expected.String())
	}

	waitForRequestLog(t, st, func(row store.RequestRow) bool {
		return row.Path == "/v1/messages" &&
			row.Stream &&
			row.InputTokens == 7 &&
			row.OutputTokens == 3 &&
			row.StopReason == "end_turn"
	})
}
