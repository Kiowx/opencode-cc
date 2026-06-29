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
	"github.com/Kiowx/opencode-cc/internal/store"
)

func newOpenAITestServer(t *testing.T, upstream http.Handler) (*Server, *store.Store, *httptest.Server) {
	t.Helper()
	zen := httptest.NewServer(upstream)
	t.Cleanup(zen.Close)

	cfg := config.Default()
	cfg.UpstreamBase = zen.URL
	cfg.ZenAPIKey = "zen-test-key"
	cfg.ModelMappings = []config.ModelMapping{
		{Match: "client-model", Target: "glm-5.1"},
		{Match: "*", Target: ""},
	}
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	srv := New(cfg, st)
	httpSrv := httptest.NewServer(srv.Handler(nil, nil))
	t.Cleanup(httpSrv.Close)
	return srv, st, httpSrv
}

func TestOpenAIChatCompletionsNonStream(t *testing.T) {
	var gotPath, gotAuth, gotAccept string
	var gotBody map[string]json.RawMessage
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req_test")
		_, _ = io.WriteString(w, `{
			"id":"chatcmpl-test",
			"object":"chat.completion",
			"model":"glm-5.1",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}
		}`)
	})
	_, st, httpSrv := newOpenAITestServer(t, upstream)

	requestBody := `{
		"model":"client-model",
		"messages":[{"role":"user","content":"hi"}],
		"max_completion_tokens":128,
		"response_format":{"type":"json_object"}
	}`
	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/v1/chat/completions", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer client-value")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("upstream path = %q", gotPath)
	}
	if gotAuth != "Bearer zen-test-key" {
		t.Errorf("upstream authorization = %q", gotAuth)
	}
	if gotAccept != "application/json" {
		t.Errorf("upstream accept = %q", gotAccept)
	}
	var model string
	if err := json.Unmarshal(gotBody["model"], &model); err != nil || model != "glm-5.1" {
		t.Errorf("mapped model = %q, err=%v", model, err)
	}
	if _, ok := gotBody["max_completion_tokens"]; !ok {
		t.Error("max_completion_tokens was dropped")
	}
	if _, ok := gotBody["response_format"]; !ok {
		t.Error("response_format was dropped")
	}
	if resp.Header.Get("X-Request-Id") != "req_test" {
		t.Errorf("x-request-id was not forwarded")
	}
	if !bytes.Contains(raw, []byte(`"object":"chat.completion"`)) {
		t.Errorf("unexpected response: %s", raw)
	}

	waitForRequestLog(t, st, func(row store.RequestRow) bool {
		return row.Path == "/v1/chat/completions" &&
			row.IncomingModel == "client-model" &&
			row.TargetModel == "glm-5.1" &&
			row.InputTokens == 7 &&
			row.OutputTokens == 2
	})
}

func TestPrepareOpenAIRequestSortsTools(t *testing.T) {
	srv, _, _ := newOpenAITestServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	upBody, incoming, target, stream, _, err := srv.prepareOpenAIRequest([]byte(`{
		"model":"client-model",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[
			{"type":"function","function":{"name":"z_tool","parameters":{"type":"object"}}},
			{"type":"function","function":{"name":"a_tool","parameters":{"type":"object"}}},
			{"type":"web_search"}
		]
	}`))
	if err != nil {
		t.Fatalf("prepare request: %v", err)
	}
	if incoming != "client-model" || target != "glm-5.1" || stream {
		t.Fatalf("unexpected request metadata: incoming=%q target=%q stream=%v", incoming, target, stream)
	}
	var out struct {
		PromptCacheKey string `json:"prompt_cache_key"`
		Tools          []struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(upBody, &out); err != nil {
		t.Fatalf("decode upstream request: %v", err)
	}
	if !strings.HasPrefix(out.PromptCacheKey, "opencode-cc:") {
		t.Fatalf("prompt_cache_key = %q", out.PromptCacheKey)
	}
	if len(out.Tools) != 3 ||
		out.Tools[0].Function.Name != "a_tool" ||
		out.Tools[1].Function.Name != "z_tool" ||
		out.Tools[2].Type != "web_search" {
		t.Fatalf("tools = %+v", out.Tools)
	}
}

func TestOpenAIChatCompletionsStream(t *testing.T) {
	var expected strings.Builder
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model         string `json:"model"`
			Stream        bool   `json:"stream"`
			StreamOptions struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if body.Model != "glm-5.1" || !body.Stream || !body.StreamOptions.IncludeUsage {
			t.Errorf("unexpected upstream request: %+v", body)
		}
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("upstream accept = %q", r.Header.Get("Accept"))
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		chunks := []string{
			`{"id":"chatcmpl-stream","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
			`{"id":"chatcmpl-stream","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`,
			`{"id":"chatcmpl-stream","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`{"id":"chatcmpl-stream","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`,
		}
		for _, chunk := range chunks {
			line := fmt.Sprintf("data: %s\r\n\r\n", chunk)
			expected.WriteString(line)
			_, _ = io.WriteString(w, line)
			if flusher != nil {
				flusher.Flush()
			}
		}
		expected.WriteString("data: [DONE]\r\n\r\n")
		_, _ = io.WriteString(w, "data: [DONE]\r\n\r\n")
	})
	_, st, httpSrv := newOpenAITestServer(t, upstream)

	body := `{"model":"client-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(httpSrv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Errorf("content-type = %q", resp.Header.Get("Content-Type"))
	}
	for _, want := range []string{`"content":"hello"`, `"prompt_tokens":5`, "data: [DONE]"} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("stream missing %q:\n%s", want, raw)
		}
	}
	if string(raw) != expected.String() {
		t.Fatalf("native SSE stream was modified:\ngot  %q\nwant %q", raw, expected.String())
	}

	waitForRequestLog(t, st, func(row store.RequestRow) bool {
		return row.Path == "/v1/chat/completions" &&
			row.Stream &&
			row.InputTokens == 5 &&
			row.OutputTokens == 1 &&
			row.StopReason == "stop"
	})
}

func TestOpenAIChatCompletionsRejectsInvalidJSON(t *testing.T) {
	_, _, httpSrv := newOpenAITestServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("upstream should not be called")
	}))

	resp, err := http.Post(httpSrv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if envelope.Error.Type != "invalid_request_error" || envelope.Error.Message == "" {
		t.Errorf("unexpected error response: %s", raw)
	}
}

func TestOpenAIChatCompletionsPassesUpstreamErrorThrough(t *testing.T) {
	const upstreamBody = `{"error":{"message":"native upstream error","type":"invalid_request_error","param":"model","code":"model_not_found"}}`
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Diagnostic", "native")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, upstreamBody)
	})
	_, _, httpSrv := newOpenAITestServer(t, upstream)

	resp, err := http.Post(httpSrv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"missing","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	if string(raw) != upstreamBody {
		t.Fatalf("upstream error was changed:\ngot  %s\nwant %s", raw, upstreamBody)
	}
	if resp.Header.Get("X-Upstream-Diagnostic") != "native" {
		t.Fatalf("upstream response header was not forwarded")
	}
}

func waitForRequestLog(t *testing.T, st *store.Store, matches func(store.RequestRow) bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := st.ListRequests(ctx, 10)
		if err == nil {
			for _, row := range rows {
				if matches(row) {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	rows, _ := st.ListRequests(context.Background(), 10)
	t.Fatalf("matching request log not found: %+v", rows)
}
