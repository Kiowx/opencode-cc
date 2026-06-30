package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
// Requests use stream=true to match real codex traffic and prime the
// upstream prompt cache with a matching request format.
func (s *Server) warmupCache(ctx context.Context) {
	cfg := s.cfg.Snapshot()
	if !cfg.ResponseCacheWarmupEnabled || len(cfg.ResponseCacheWarmupPrompts) == 0 {
		return
	}

	log.Printf("warmup: starting with %d prompt(s) in 5s", len(cfg.ResponseCacheWarmupPrompts))
	time.Sleep(5 * time.Second)

	const maxRetries = 3
	backoffBase := []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second}

	for i, wp := range cfg.ResponseCacheWarmupPrompts {
		if wp.Model == "" || wp.UserMessage == "" {
			continue
		}
		targetModel := cfg.ResolveModel(wp.Model)

		payload := buildWarmupPayload(targetModel, &wp)

		body, err := json.Marshal(payload)
		if err != nil {
			continue
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
			upReq.Header.Set("Accept", "text/event-stream")

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

			// Read the SSE stream and reconstruct a full OpenAI response.
			reconstructedBody, promptTokens, parseErr := readSSEToFullResponse(resp.Body)
			resp.Body.Close()
			if parseErr != nil {
				log.Printf("warmup: request %d parse error: %v", i, parseErr)
				if attempt < maxRetries {
					time.Sleep(backoffBase[attempt] + time.Duration(rand.Intn(500))*time.Millisecond)
				}
				continue
			}

			ck := responseCacheKey(body)
			s.responseCache.Set(ck, targetModel, promptTokens, reconstructedBody)
			s.persistEntryToStore(ck, targetModel, promptTokens, reconstructedBody)
			log.Printf("warmup: request %d cached key=%s tokens=%d", i, ck[:16], promptTokens)
			cached = true
			break
		}
		if !cached {
			log.Printf("warmup: request %d failed after %d retries", i, maxRetries)
		}
		time.Sleep(3 * time.Second)
	}

	log.Printf("warmup: done")
}

// readSSEToFullResponse reads an SSE stream from r, parsing line-by-line
// until it sees [DONE] (the upstream never closes the connection on keep-alive).
// It extracts the last data chunk with usage and reconstructs a full
// non-streaming OpenAI chat completion response for cache storage.
func readSSEToFullResponse(r io.Reader) ([]byte, int, error) {
	var (
		id, model      string
		created        int64
		finishReason   string
		contentBuilder strings.Builder
		lastUsage      *proxy.OpenAIUsage
		totalRead      int64
	)

	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			// If we already got usage and the stream ended, that's fine.
			if lastUsage != nil && err == io.EOF {
				break
			}
			return nil, 0, fmt.Errorf("read SSE line: %w", err)
		}
		totalRead += int64(len(line))
		if totalRead > maxResponseBytes {
			return nil, 0, fmt.Errorf("SSE body exceeds max response size")
		}

		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		data := bytes.TrimSpace(line[6:])
		if len(data) == 0 {
			continue
		}
		if string(data) == "[DONE]" {
			break
		}

		var rawChunk map[string]json.RawMessage
		if err := json.Unmarshal(data, &rawChunk); err != nil {
			continue
		}

		// Extract id, model, created from the first chunk that has them.
		if v, ok := rawChunk["id"]; ok && len(v) > 0 && id == "" {
			json.Unmarshal(v, &id)
		}
		if v, ok := rawChunk["model"]; ok && len(v) > 0 && model == "" {
			json.Unmarshal(v, &model)
		}
		if v, ok := rawChunk["created"]; ok && len(v) > 0 && created == 0 {
			json.Unmarshal(v, &created)
		}

		// Use the struct-based chunk for structured fields.
		var chunk proxy.OpenAIStreamChunk
		if json.Unmarshal(data, &chunk) != nil {
			continue
		}

		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				contentBuilder.WriteString(choice.Delta.Content)
			}
			if choice.FinishReason != nil {
				finishReason = *choice.FinishReason
			}
		}

		if chunk.Usage != nil {
			lastUsage = chunk.Usage
		}
	}

	if lastUsage == nil {
		return nil, 0, fmt.Errorf("no usage found in SSE stream")
	}
	if finishReason == "" {
		finishReason = "stop"
	}
	if id == "" {
		id = "chatcmpl-warmup"
	}
	if created == 0 {
		created = time.Now().Unix()
	}

	resp := proxy.OpenAIResponse{
		ID: id,
		Choices: []proxy.OpenAIChoice{
			{
				Index: 0,
				Message: &proxy.OpenAIMessage{
					Role:    "assistant",
					Content: contentBuilder.String(),
				},
				FinishReason: &finishReason,
			},
		},
		Usage: *lastUsage,
	}

	bodyMap := map[string]any{
		"id":      resp.ID,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": resp.Choices,
		"usage":   resp.Usage,
	}
	body, err := json.Marshal(bodyMap)
	if err != nil {
		return nil, 0, fmt.Errorf("marshal reconstructed response: %w", err)
	}

	return body, lastUsage.PromptTokens, nil
}

func buildWarmupPayload(model string, wp *config.WarmupPrompt) map[string]json.RawMessage {
	payload := map[string]json.RawMessage{
		"model":      json.RawMessage(`"` + jsonEscape(model) + `"`),
		"max_tokens": json.RawMessage(`1`),
		"stream":     json.RawMessage(`true`),
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
