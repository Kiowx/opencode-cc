package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Kiowx/opencode-cc/internal/proxy"
)

const cacheStoreCleanupInterval = 5 * time.Minute

// tryServeFromCache checks the response cache for the given cacheKey and, if
// found, writes the cached response (in Anthropic wire format) to w. It returns
// true when the request was fully satisfied from cache.
func (s *Server) tryServeFromCache(
	w http.ResponseWriter, r *http.Request,
	cacheKey string,
	inModel, target string,
	stream bool,
	reqBody, path string,
	start time.Time,
) bool {
	if cacheKey == "" || stream {
		return false
	}
	entry, ok := s.responseCache.Get(cacheKey)
	if !ok {
		log.Printf("cache MISS key=%s model=%s path=%s", cacheKey[:min(len(cacheKey), 24)], target, path)
		return false
	}

	oresp, err := proxy.ParseOpenAIResponse(entry.Body)
	if err != nil {
		return false
	}

	log.Printf("cache HIT  key=%s model=%s path=%s tokens=%d", cacheKey[:min(len(cacheKey), 24)], target, path, entry.PromptTokens)

	if strings.HasPrefix(path, "/v1/messages") {
		aresp := proxy.ConvertResponse(oresp, inModel)
		writeJSON(w, http.StatusOK, aresp)
		stop := ""
		if aresp.StopReason != nil {
			stop = *aresp.StopReason
		}
		s.logSuccessWithCache(r.Context(), r, inModel, target, false, http.StatusOK,
			entry.PromptTokens, aresp.Usage.OutputTokens,
			entry.PromptTokens, 0,
			stop, string(reqBody), mustJSON(aresp), time.Since(start))
		return true
	}

	if strings.HasPrefix(path, "/v1/chat/completions") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(entry.Body)
		stopReason := ""
		if len(oresp.Choices) > 0 && oresp.Choices[0].FinishReason != nil {
			stopReason = *oresp.Choices[0].FinishReason
		}
		s.logSuccessWithCache(r.Context(), r, inModel, target, false, http.StatusOK,
			entry.PromptTokens, oresp.Usage.CompletionTokens,
			entry.PromptTokens, 0, stopReason,
			string(reqBody), string(entry.Body), time.Since(start))
		return true
	}

	if strings.HasPrefix(path, "/v1/responses") {
		out := proxy.ConvertResponsesResponse(oresp, inModel)
		writeJSON(w, http.StatusOK, out)
		stopReason := ""
		if len(oresp.Choices) > 0 && oresp.Choices[0].FinishReason != nil {
			stopReason = *oresp.Choices[0].FinishReason
		}
		s.logSuccessWithCache(r.Context(), r, inModel, target, false, http.StatusOK,
			entry.PromptTokens, oresp.Usage.CompletionTokens,
			entry.PromptTokens, 0, stopReason,
			string(reqBody), mustJSON(out), time.Since(start))
		return true
	}

	return false
}

// storeInCache saves an upstream OpenAI-compatible response body in the
// response cache keyed by cacheKey when it is non-empty. The entry is also
// persisted to SQLite so it survives process restarts.
func (s *Server) storeInCache(cacheKey, targetModel string, rawBody []byte) {
	if cacheKey == "" || len(rawBody) == 0 {
		return
	}
	var usage struct {
		PromptTokens int `json:"prompt_tokens"`
	}
	var env struct {
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(rawBody, &env) == nil && len(env.Usage) > 0 {
		json.Unmarshal(env.Usage, &usage)
	}
	s.responseCache.Set(cacheKey, targetModel, usage.PromptTokens, rawBody)
	s.persistEntryToStore(cacheKey, targetModel, usage.PromptTokens, rawBody)
}

// persistEntryToStore writes a cache entry to SQLite. Errors are logged but
// non-fatal — the in-memory cache is the primary fast path; the store is a
// crash-recovery backup.
func (s *Server) persistEntryToStore(key, model string, promptTokens int, body []byte) {
	if s.store == nil {
		return
	}
	now := time.Now()
	if err := s.store.SaveResponseCacheEntry(context.Background(), key, model, promptTokens, body, now, now.Add(s.responseCache.TTL())); err != nil {
		log.Printf("persist cache entry: %v", err)
	}
}

// loadCacheFromStore populates the in-memory response cache from persisted
// SQLite rows. Called once during server startup.
func (s *Server) loadCacheFromStore() {
	if s.store == nil {
		return
	}
	ctx := context.Background()
	if err := s.store.DeleteExpiredResponseCache(ctx); err != nil {
		log.Printf("purge expired persisted cache: %v", err)
	}
	maxEntries := s.responseCache.MaxEntries()
	entries, err := s.store.LoadResponseCacheEntries(ctx, maxEntries)
	if err != nil {
		log.Printf("load cache from store: %v", err)
		return
	}
	for _, e := range entries {
		s.responseCache.SetWithExpiry(e.CacheKey, e.TargetModel, e.PromptTokens, e.Body, e.CreatedAt, e.ExpiresAt)
	}
	if len(entries) > 0 {
		log.Printf("loaded %d response-cache entries from store", len(entries))
	}
}

// cleanupCacheStore periodically removes expired rows from the SQLite table.
// Runs on a background goroutine started from Server.Start().
func (s *Server) cleanupCacheStore() {
	if s.store == nil {
		return
	}
	ticker := time.NewTicker(cacheStoreCleanupInterval)
	defer ticker.Stop()
	for range ticker.C {
		if err := s.store.DeleteExpiredResponseCache(context.Background()); err != nil {
			log.Printf("cleanup persisted cache: %v", err)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func rawString(raw json.RawMessage) string {
	var v string
	if json.Unmarshal(raw, &v) == nil {
		return v
	}
	return ""
}
