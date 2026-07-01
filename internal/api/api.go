// Package api implements the web panel's backend REST API, mounted under
// /api. It exposes: auth check, config get/update, summary stats, hourly
// series, model usage, latency percentiles, and the request log + detail.
package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Kiowx/opencode-cc/internal/config"
	"github.com/Kiowx/opencode-cc/internal/metrics"
	"github.com/Kiowx/opencode-cc/internal/store"
)

// ---------------------------------------------------------------------------
// Session management (in-memory; processes restart invalidate sessions)
// ---------------------------------------------------------------------------

const sessionCookieName = "opencode_cc_session"
const sessionTTL = 24 * time.Hour

var sessions sync.Map // sessionToken(string) → time.Time expiry

// newSessionToken generates a cryptographically random 32-byte hex session token.
func newSessionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// isValidSession returns true if the token exists and has not expired.
func isValidSession(token string) bool {
	if token == "" {
		return false
	}
	v, ok := sessions.Load(token)
	if !ok {
		return false
	}
	exp, _ := v.(time.Time)
	if time.Now().After(exp) {
		sessions.Delete(token)
		return false
	}
	return true
}

func invalidateSessions() {
	sessions.Range(func(key, _ any) bool {
		sessions.Delete(key)
		return true
	})
}

// API holds dependencies shared by all panel handlers.
type API struct {
	cfg   *config.Config
	store *store.Store
}

// New constructs an API.
func New(cfg *config.Config, st *store.Store) *API {
	return &API{cfg: cfg, store: st}
}

// Mount registers the API routes on mux under /api.
func (a *API) Mount(mux *http.ServeMux) {
	mux.HandleFunc("/api/health", a.health)
	// Auth endpoints are intentionally unauthenticated.
	mux.HandleFunc("/api/auth/login", a.handleLogin)
	mux.HandleFunc("/api/auth/logout", a.handleLogout)
	mux.HandleFunc("/api/auth/check", a.handleAuthCheck)
	mux.Handle("/api/config", a.auth(http.HandlerFunc(a.configHandler)))
	mux.Handle("/api/test", a.auth(http.HandlerFunc(a.testUpstream)))
	mux.Handle("/api/stats/summary", a.auth(http.HandlerFunc(a.summary)))
	mux.Handle("/api/stats/hourly", a.auth(http.HandlerFunc(a.hourly)))
	mux.Handle("/api/stats/models", a.auth(http.HandlerFunc(a.modelUsage)))
	mux.Handle("/api/stats/latency", a.auth(http.HandlerFunc(a.latency)))
	mux.Handle("/api/logs", a.auth(http.HandlerFunc(a.logs)))
	mux.Handle("/api/logs/", a.auth(http.HandlerFunc(a.logDetail)))
	mux.Handle("/api/keys", a.auth(http.HandlerFunc(a.keysHandler)))
	mux.Handle("/api/keys/", a.auth(http.HandlerFunc(a.keysHandler)))
}

// SetInvalidateCache wires the cache-invalidation callback from the server
// package (avoids an api → server import cycle). Called after key mutations.
func SetInvalidateCache(fn func()) { invalidateCache = fn }

// auth gates the panel API behind the configured panel token. If no token is
// configured, access is open (convenient for localhost-only use).
func (a *API) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.cfg.RLock()
		token := a.cfg.PanelToken
		a.cfg.RUnlock()
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}
		// Accept Bearer header / ?token= with the raw panel token (API / scripted access).
		provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if provided == "" {
			provided = r.URL.Query().Get("token")
		}
		if provided != "" && subtleEqual(provided, token) {
			next.ServeHTTP(w, r)
			return
		}
		// Accept a valid session cookie (web login flow).
		if c, _ := r.Cookie(sessionCookieName); c != nil && isValidSession(c.Value) {
			next.ServeHTTP(w, r)
			return
		}
		// Legacy: raw token stored directly in cookie (backward compat).
		if c, _ := r.Cookie("opencode_cc_token"); c != nil && subtleEqual(c.Value, token) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
	})
}

// subtleEqual is a constant-time-ish string compare for tokens.
func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// health is an unauthenticated liveness probe.
func (a *API) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"time":    time.Now().Format(time.RFC3339),
		"version": "1.3.0",
	})
}

// ---------------------------------------------------------------------------
// Auth endpoints (unauthenticated, used by the web login flow)
// ---------------------------------------------------------------------------

// handleAuthCheck reports whether the panel requires authentication and
// whether the current request is already authenticated.
func (a *API) handleAuthCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	a.cfg.RLock()
	token := a.cfg.PanelToken
	a.cfg.RUnlock()

	if token == "" {
		writeJSON(w, http.StatusOK, map[string]any{"need_auth": false, "authenticated": true})
		return
	}
	// Check session cookie.
	if c, _ := r.Cookie(sessionCookieName); c != nil && isValidSession(c.Value) {
		writeJSON(w, http.StatusOK, map[string]any{"need_auth": true, "authenticated": true})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"need_auth": true, "authenticated": false})
}

// handleLogin verifies a password against the panel token and, on success,
// creates an in-memory session and sets an HttpOnly cookie.
func (a *API) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}

	a.cfg.RLock()
	token := a.cfg.PanelToken
	a.cfg.RUnlock()

	if token == "" || !subtleEqual(body.Password, token) {
		// Fixed delay to slow brute-force attempts.
		time.Sleep(500 * time.Millisecond)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "密码错误"})
		return
	}

	tok, err := newSessionToken()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	sessions.Store(tok, time.Now().Add(sessionTTL))

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleLogout deletes the current session and clears the session cookie.
func (a *API) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if c, _ := r.Cookie(sessionCookieName); c != nil {
		sessions.Delete(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// configHandler GET returns the current config (with sensitive fields masked);
// PUT applies only the JSON fields present in the request.
func (a *API) configHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		snap := a.cfg.Snapshot()
		writeJSON(w, http.StatusOK, publicConfig(snap))
	case http.MethodPut:
		var in config.Patch
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		oldPanelToken := a.cfg.Snapshot().PanelToken
		a.cfg.ApplyPatch(&in)
		if err := a.cfg.Save(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if in.PanelToken != nil && *in.PanelToken != oldPanelToken {
			invalidateSessions()
		}
		writeJSON(w, http.StatusOK, publicConfig(a.cfg.Snapshot()))
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// publicConfig masks the Zen API key so it is never echoed to the browser.
func publicConfig(c *config.Config) map[string]any {
	key := c.ZenAPIKey
	masked := maskKey(key)
	// Build the upstreams view (masked keys) for the panel list.
	upstreamsView := make([]map[string]any, 0, len(c.Upstreams))
	for _, u := range c.Upstreams {
		upstreamsView = append(upstreamsView, map[string]any{
			"base_url":       u.BaseURL,
			"api_key_masked": maskKey(u.APIKey),
			"api_key_set":    u.APIKey != "",
			"name":           u.Name,
			"enabled":        u.Enabled,
		})
	}
	return map[string]any{
		"listen_addr":                    c.ListenAddr,
		"upstream_base":                  c.UpstreamBase,
		"native_anthropic":               c.NativeAnthropic,
		"zen_api_key_masked":             masked,
		"zen_api_key_set":                key != "",
		"upstreams":                      upstreamsView,
		"panel_token_set":                c.PanelToken != "",
		"require_api_key":                c.RequireAPIKey,
		"default_model":                  c.DefaultModel,
		"model_mappings":                 c.ModelMappings,
		"web_search_model":               c.WebSearchModel,
		"web_search_mode":                c.WebSearchMode,
		"web_search_base_url":            c.WebSearchBaseURL,
		"web_search_api_key_masked":      maskKey(c.WebSearchAPIKey),
		"web_search_api_key_set":         c.WebSearchAPIKey != "",
		"log_requests":                   c.LogRequests,
		"max_body_log_bytes":             c.MaxBodyLogBytes,
		"request_timeout_seconds":        c.RequestTimeoutSeconds,
		"prompt_cache_enabled":           c.PromptCacheEnabled,
		"prompt_cache_key_prefix":        c.PromptCacheKeyPrefix,
		"prompt_cache_anthropic_control": c.PromptCacheAnthropicControl,
		"prompt_cache_normalize":         c.PromptCacheNormalize,
		"response_cache_enabled":              c.ResponseCacheEnabled,
		"response_cache_ttl_seconds":           c.ResponseCacheTTLSeconds,
		"response_cache_max_entries":           c.ResponseCacheMaxEntries,
		"thinking_budget_mappings":            c.ThinkingBudgetMappings,
	}
}

// maskKey renders a key for display without exposing it: short keys are fully
// starred; longer ones show first 4 + stars + last 4. Empty input yields "".
func maskKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 8 {
		return strings.Repeat("*", len(key))
	}
	return key[:4] + strings.Repeat("*", len(key)-8) + key[len(key)-4:]
}

// summary returns lifetime + last-24h aggregate numbers.
func (a *API) summary(w http.ResponseWriter, r *http.Request) {
	if a.store == nil {
		writeJSON(w, http.StatusOK, &store.StatsSummary{})
		return
	}
	s, err := a.store.Summary(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s)
}

// hourly returns the per-hour series for the requested window (?hours=24).
func (a *API) hourly(w http.ResponseWriter, r *http.Request) {
	hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
	if hours <= 0 {
		hours = 24
	}
	if a.store == nil {
		writeJSON(w, http.StatusOK, []store.HourPoint{})
		return
	}
	pts, err := a.store.HourlySeries(r.Context(), hours)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, pts)
}

// modelUsage returns per-target-model request/token counts.
func (a *API) modelUsage(w http.ResponseWriter, r *http.Request) {
	hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
	if hours <= 0 {
		hours = 24
	}
	since := time.Now().Add(-time.Duration(hours) * time.Hour).UnixMilli()
	if a.store == nil {
		writeJSON(w, http.StatusOK, []store.ModelUsagePoint{})
		return
	}
	pts, err := a.store.ModelUsage(r.Context(), since)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, pts)
}

// latency returns p50/p95/p99 latency in ms over recent requests.
func (a *API) latency(w http.ResponseWriter, r *http.Request) {
	if a.store == nil {
		writeJSON(w, http.StatusOK, map[string]int64{"p50": 0, "p95": 0, "p99": 0})
		return
	}
	vals, err := a.store.RecentLatency(r.Context(), 300)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{
		"p50": metrics.Percentile(vals, 50),
		"p95": metrics.Percentile(vals, 95),
		"p99": metrics.Percentile(vals, 99),
	})
}

// logs returns the recent request list (without full bodies).
func (a *API) logs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 100
	}
	if a.store == nil {
		writeJSON(w, http.StatusOK, []store.RequestRow{})
		return
	}
	rows, err := a.store.ListRequests(r.Context(), limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// Strip large bodies from the list view.
	type lite struct {
		store.RequestRow
		ReqBody  string `json:"req_body"`
		RespBody string `json:"resp_body"`
	}
	out := make([]lite, 0, len(rows))
	for _, row := range rows {
		row.ReqBody = ""
		row.RespBody = ""
		out = append(out, lite{RequestRow: row})
	}
	writeJSON(w, http.StatusOK, out)
}

// logDetail returns a single request with full bodies.
func (a *API) logDetail(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, "/api/logs/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad id"})
		return
	}
	if a.store == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no store"})
		return
	}
	row, err := a.store.GetRequest(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if row == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	writeJSON(w, http.StatusOK, row)
}
