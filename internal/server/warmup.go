package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/Kiowx/opencode-cc/internal/config"
	"github.com/Kiowx/opencode-cc/internal/proxy"
)

// warmupCache sends the configured warmup prompts to the upstream and stores
// the responses in the local response cache under both the raw-map key
// (matches OpenAIProxy) and the struct key (matches Proxy/ResponsesProxy).
func (s *Server) warmupCache(ctx context.Context) {
	cfg := s.cfg.Snapshot()
	if !cfg.ResponseCacheWarmupEnabled || len(cfg.ResponseCacheWarmupPrompts) == 0 {
		return
	}

	log.Printf("warmup: starting with %d prompt(s) in 5s", len(cfg.ResponseCacheWarmupPrompts))
	time.Sleep(5 * time.Second)

	const maxRetries = 3
	backoffBase := []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second}

	opts := promptCacheOptionsFromConfig(cfg)

	for i, wp := range cfg.ResponseCacheWarmupPrompts {
		if wp.Model == "" || wp.UserMessage == "" {
			continue
		}
		targetModel := cfg.ResolveModel(wp.Model)

		payload := buildWarmupPayload(targetModel, &wp)
		proxy.ApplyRawOpenAIPromptCache(payload, opts)
		cacheKeyRaw := rawString(payload["prompt_cache_key"])

		body, err := json.Marshal(payload)
		if err != nil {
			continue
		}

		var cacheKeyStruct string
		var oreq proxy.OpenAIRequest
		if json.Unmarshal(body, &oreq) == nil {
			oreq.PromptCacheKey = ""
			proxy.ApplyOpenAIPromptCache(&oreq, opts)
			cacheKeyStruct = oreq.PromptCacheKey
		}

		cached := false
		for attempt := 0; attempt <= maxRetries; attempt++ {
			upstream, zenKey, ok := s.cfg.NextUpstream()
			if !ok {
				log.Printf("warmup: no upstream available, skipping")
				break
			}

			upURL := strings.TrimRight(upstream, "/") + "/v1/chat/completions"
			upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upURL, bytes.NewReader(body))
			if err != nil {
				continue
			}
			upReq.Header.Set("Authorization", "Bearer "+zenKey)
			upReq.Header.Set("Content-Type", "application/json")
			upReq.Header.Set("Accept", "application/json")

			resp, err := s.httpClient.Do(upReq)
			if err != nil {
				log.Printf("warmup: request %d attempt %d failed: %v", i, attempt, err)
				if attempt < maxRetries {
					time.Sleep(backoffBase[attempt] + time.Duration(rand.Intn(500))*time.Millisecond)
				}
				continue
			}

			if resp.StatusCode == http.StatusTooManyRequests {
				log.Printf("warmup: request %d attempt %d got 429, will retry", i, attempt)
				s.cfg.MarkUpstreamFailed()
				resp.Body.Close()
				if attempt < maxRetries {
					time.Sleep(backoffBase[attempt] + time.Duration(rand.Intn(500))*time.Millisecond)
				}
				continue
			}

			if resp.StatusCode != http.StatusOK {
				log.Printf("warmup: request %d attempt %d got status %d, giving up", i, attempt, resp.StatusCode)
				resp.Body.Close()
				break
			}

			raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
			resp.Body.Close()
			if err != nil || len(raw) > maxResponseBytes {
				log.Printf("warmup: request %d read error: %v", i, err)
				break
			}

			var usage struct {
				PromptTokens int `json:"prompt_tokens"`
			}
			var respEnvelope struct {
				Usage json.RawMessage `json:"usage"`
			}
			if json.Unmarshal(raw, &respEnvelope) == nil && len(respEnvelope.Usage) > 0 {
				json.Unmarshal(respEnvelope.Usage, &usage)
			}

			s.responseCache.Set(cacheKeyRaw, targetModel, usage.PromptTokens, raw)
			log.Printf("warmup: request %d cached raw=%s tokens=%d", i, cacheKeyRaw[:16], usage.PromptTokens)
			cached = true

			if cacheKeyStruct != "" && cacheKeyStruct != cacheKeyRaw {
				s.responseCache.Set(cacheKeyStruct, targetModel, usage.PromptTokens, raw)
				log.Printf("warmup: request %d cached struct=%s", i, cacheKeyStruct[:16])
			}
			break
		}
		if !cached {
			log.Printf("warmup: request %d failed after %d retries", i, maxRetries)
		}
		time.Sleep(3 * time.Second)
	}

	log.Printf("warmup: done")
}

func buildWarmupPayload(model string, wp *config.WarmupPrompt) map[string]json.RawMessage {
	payload := map[string]json.RawMessage{
		"model":      json.RawMessage(`"` + jsonEscape(model) + `"`),
		"max_tokens": json.RawMessage(`1`),
		"stream":     json.RawMessage(`false`),
	}
	if len(wp.Tools) > 0 {
		raw, _ := json.Marshal(wp.Tools)
		payload["tools"] = raw
	}
	messages := make([]map[string]json.RawMessage, 0, 2)
	if wp.System != "" {
		messages = append(messages, map[string]json.RawMessage{
			"role":    json.RawMessage(`"system"`),
			"content": json.RawMessage(`"` + jsonEscape(wp.System) + `"`),
		})
	}
	messages = append(messages, map[string]json.RawMessage{
		"role":    json.RawMessage(`"user"`),
		"content": json.RawMessage(`"` + jsonEscape(wp.UserMessage) + `"`),
	})
	rawMessages, _ := json.Marshal(messages)
	payload["messages"] = rawMessages
	return payload
}

func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	if len(b) >= 2 && b[0] == '"' && b[len(b)-1] == '"' {
		return string(b[1 : len(b)-1])
	}
	return s
}
