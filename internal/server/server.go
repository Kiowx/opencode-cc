package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/Kiowx/opencode-cc/internal/api"
	"github.com/Kiowx/opencode-cc/internal/config"
	"github.com/Kiowx/opencode-cc/internal/proxy"
	"github.com/Kiowx/opencode-cc/internal/store"
)

// Server is the top-level HTTP server. It owns the config, store, response
// cache, and the underlying *http.Server so it can be shut down cleanly.
type Server struct {
	cfg           *config.Config
	store         *store.Store
	httpClient    *http.Client
	srv           *http.Server
	responseCache *proxy.ResponseCache
}

// New constructs a Server. The store may be nil if request logging is disabled.
func New(cfg *config.Config, st *store.Store) *Server {
	ttl := time.Duration(cfg.ResponseCacheTTLSeconds) * time.Second
	maxEntries := cfg.ResponseCacheMaxEntries
	if maxEntries <= 0 {
		maxEntries = proxy.ResponseCacheDefaultMaxEntries
	}
	rc := proxy.NewResponseCache(ttl, maxEntries)
	rc.SetEnabled(cfg.ResponseCacheEnabled)
	rc.Start()
	s := &Server{
		cfg:           cfg,
		store:         st,
		httpClient: &http.Client{
			Timeout: 0,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 16,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		responseCache: rc,
	}
	s.loadCacheFromStore()
	return s
}

// Handler returns the root http.Handler wiring all routes. panelAssets serves
// the embedded SPA (nil = no embedded assets, e.g. dev mode).
func (s *Server) Handler(panelAssets http.FileSystem, panelMux http.Handler) http.Handler {
	mux := http.NewServeMux()

	// ---- Anthropic-compatible endpoints (gated by client API key) ----
	mux.Handle("/v1/messages", s.clientAuth(s.Proxy()))
	mux.Handle("/v1/messages/count_tokens", s.clientAuth(s.CountTokens()))
	// OpenAI-compatible clients can use the same server directly. Requests are
	// forwarded to Zen after applying the configured model mapping.
	mux.Handle("/v1/chat/completions", s.clientAuth(s.OpenAIProxy()))
	// Codex uses the Responses API. Zen /go only exposes Chat Completions, so
	// this route performs a bidirectional Responses <-> Chat translation.
	mux.Handle("/v1/responses", s.clientAuth(s.ResponsesProxy()))
	mux.Handle("/v1/models", s.clientAuth(proxy.ModelsHandlerWithUpstream(
		s.httpClient,
		func() (string, string) {
			base, key, _ := s.cfg.NextUpstream()
			return base, key
		},
	)))

	// ---- Panel API (mounted under /api) ----
	s.mountPanelAPI(mux)

	// ---- Panel SPA (everything else) ----
	if panelMux != nil {
		mux.Handle("/", panelMux)
	} else if panelAssets != nil {
		mux.Handle("/", http.FileServer(panelAssets))
	} else {
		mux.HandleFunc("/", s.panelPlaceholder)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Recover from panics so one bad request can't take the process down.
		// Logging middleware records basic request info for non-/api paths.
		s.withLogging(mux).ServeHTTP(w, r)
	})
}

// panelPlaceholder is shown when no embedded assets are present (dev mode).
func (s *Server) panelPlaceholder(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(devPanelPlaceholder))
}

// Start begins serving. It blocks until Shutdown is called.
func (s *Server) Start(handler http.Handler) error {
	s.srv = &http.Server{
		Addr:              s.cfg.Snapshot().ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// WriteTimeout left at 0 so long streams are not killed.
	}
	go s.cleanupCacheStore()
	return s.srv.ListenAndServe()
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown() error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(nil)
}

// mountPanelAPI wires the panel's REST API onto mux via the api package.
func (s *Server) mountPanelAPI(mux *http.ServeMux) {
	api.SetInvalidateCache(InvalidateKeyCache)
	api.New(s.cfg, s.store).Mount(mux)
}

const devPanelPlaceholder = `<!doctype html><meta charset="utf-8">
<title>opencode-cc</title>
<body style="font-family:system-ui;background:#0b0f17;color:#e5e7eb;padding:2rem">
<h1>opencode-cc proxy is running ✅</h1>
<p>The web panel assets are not embedded in this build (dev mode).</p>
<p>Run <code>cd web &amp;&amp; npm run dev</code> and open the Vite dev server,
or build the panel with <code>npm run build</code> and rebuild the Go binary.</p>
<p>Set <code>ANTHROPIC_BASE_URL=http://` + "`HOST:8787`" + `</code> to start using it.</p>
</body>`
