package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Kiowx/opencode-cc/internal/config"
)

// TestRoundRobinUpstreamsAcrossRequests verifies that requests without a
// prompt-cache sticky key still cycle through the configured upstream pool.
func TestRoundRobinUpstreamsAcrossRequests(t *testing.T) {
	// Two mock upstreams that record which key hit them.
	var mu sync.Mutex
	hitsA, hitsB := []string{}, []string{}
	zenA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hitsA = append(hitsA, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "a", "choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	}))
	zenB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hitsB = append(hitsB, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "b", "choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	}))
	defer zenA.Close()
	defer zenB.Close()

	cfg := config.Default()
	cfg.PromptCacheEnabled = false
	cfg.Upstreams = []config.Upstream{
		{BaseURL: zenA.URL, APIKey: "key-A", Enabled: true},
		{BaseURL: zenB.URL, APIKey: "key-B", Enabled: true},
	}
	cfg.ModelMappings = []config.ModelMapping{{Match: "*", Target: "glm-5.1"}}
	srv, _ := newTestServerWithCfg(t, cfg)

	body, _ := json.Marshal(map[string]any{
		"model": "claude-x", "max_tokens": 8,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})

	// Fire 6 requests; expect alternating A/B by key.
	for i := 0; i < 6; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body)))
		rr := httptest.NewRecorder()
		srv.Proxy()(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("req %d: status %d: %s", i, rr.Code, rr.Body.String())
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(hitsA) != 3 || len(hitsB) != 3 {
		t.Fatalf("expected 3 hits each, got A=%d B=%d", len(hitsA), len(hitsB))
	}
	// Every A hit must carry key-A, every B hit key-B.
	for _, h := range hitsA {
		if h != "Bearer key-A" {
			t.Errorf("upstream A got wrong auth: %q", h)
		}
	}
	for _, h := range hitsB {
		if h != "Bearer key-B" {
			t.Errorf("upstream B got wrong auth: %q", h)
		}
	}
}

func TestModelsEndpointUsesPairedRoundRobinUpstream(t *testing.T) {
	zenA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key-A" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"model-a","created":1}]}`)
	}))
	defer zenA.Close()
	zenB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key-B" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"model-b","created":2}]}`)
	}))
	defer zenB.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{
		{BaseURL: zenA.URL, APIKey: "key-A", Enabled: true},
		{BaseURL: zenB.URL, APIKey: "key-B", Enabled: true},
	}
	srv, _ := newTestServerWithCfg(t, cfg)
	httpSrv := httptest.NewServer(srv.Handler(nil, nil))
	defer httpSrv.Close()

	resp, err := http.Get(httpSrv.URL + "/v1/models")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), `"id":"model-a"`) &&
		!strings.Contains(string(raw), `"id":"model-b"`) {
		t.Fatalf("models endpoint did not use paired upstream credentials: %s", raw)
	}
}
