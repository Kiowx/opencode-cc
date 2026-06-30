package server

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Kiowx/opencode-cc/internal/proxy"
)

// OpenAIProxy handles POST /v1/chat/completions. Unlike Proxy, this endpoint
// accepts and returns OpenAI wire format, so OpenAI-compatible SDKs can point
// their base URL at opencode-cc directly.
func (s *Server) OpenAIProxy() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method must be POST")
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error",
				"could not read request body: "+err.Error())
			return
		}

		upBody, incomingModel, targetModel, stream, _, err := s.prepareOpenAIRequest(body)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}

		rcKey := responseCacheKey(upBody)

		// Attempt to serve from local response cache.
		if s.tryServeFromCache(w, r, rcKey, incomingModel,
			targetModel, stream, string(body), r.URL.Path, start) {
			return
		}

		cfg := s.cfg.Snapshot()
		upstream, zenKey, ok := s.cfg.NextUpstream()
		if !ok {
			const msg = "no upstream API key configured. Set one in the web panel (Settings → upstreams)."
			writeOpenAIError(w, http.StatusUnauthorized, "authentication_error", msg)
			s.logFailed(r.Context(), r, incomingModel, targetModel, stream,
				http.StatusUnauthorized, "no upstream api key", body, time.Since(start))
			return
		}

		const maxRetries = 3
		var resp *http.Response
		for attempt := 0; attempt <= maxRetries; attempt++ {
			curUpstream, curZenKey := upstream, zenKey
			if attempt > 0 {
				var ok bool
				curUpstream, curZenKey, ok = s.cfg.NextUpstream()
				if !ok {
					const msg = "no upstream API key configured. Set one in the web panel (Settings → upstreams)."
					writeOpenAIError(w, http.StatusUnauthorized, "authentication_error", msg)
					s.logFailed(r.Context(), r, incomingModel, targetModel, stream,
						http.StatusUnauthorized, "no upstream api key", body, time.Since(start))
					return
				}
			}

			upURL := strings.TrimRight(curUpstream, "/") + "/v1/chat/completions"
			upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upURL, bytes.NewReader(upBody))
			if err != nil {
				writeOpenAIError(w, http.StatusInternalServerError, "api_error",
					"could not build upstream request: "+err.Error())
				return
			}
			upReq.Header.Set("Authorization", "Bearer "+curZenKey)
			upReq.Header.Set("Content-Type", "application/json")
			upReq.Header.Set("User-Agent", "opencode-cc/1.1")
			if stream {
				upReq.Header.Set("Accept", "text/event-stream")
			} else {
				upReq.Header.Set("Accept", "application/json")
			}

			httpClient := s.upstreamClient(stream, cfg.RequestTimeoutSeconds)
			resp, err = httpClient.Do(upReq)
			if err != nil {
				writeOpenAIError(w, http.StatusBadGateway, "api_error", "upstream request failed: "+err.Error())
				s.logFailed(r.Context(), r, incomingModel, targetModel, stream,
					http.StatusBadGateway, err.Error(), body, time.Since(start))
				return
			}
			if resp.StatusCode == http.StatusTooManyRequests && attempt < maxRetries {
				s.cfg.MarkUpstreamFailed()
				resp.Body.Close()
				log.Printf("upstream 429 on %s attempt %d, failover to next key", incomingModel, attempt)
				continue
			}
			break
		}
		if resp == nil {
			writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_error",
				"all upstream keys returned 429")
			s.logFailed(r.Context(), r, incomingModel, targetModel, stream,
				http.StatusTooManyRequests, "all upstream keys returned 429", body, time.Since(start))
			return
		}

		if resp.StatusCode >= http.StatusBadRequest && shouldFailover(resp.StatusCode) {
			s.cfg.MarkUpstreamFailed()
		}

		contentType := strings.ToLower(resp.Header.Get("Content-Type"))
		if stream && resp.StatusCode < http.StatusBadRequest &&
			(contentType == "" || strings.Contains(contentType, "text/event-stream")) {
			s.relayOpenAIStream(w, resp, r, incomingModel, targetModel, body, start, rcKey)
			return
		}
		s.relayOpenAIJSON(w, resp, r, incomingModel, targetModel, stream, body, start, rcKey)
	}
}

// prepareOpenAIRequest validates the JSON object and rewrites only its model
// field, preserving extensions used by different OpenAI-compatible clients.
// It returns the upstream body, incoming model, target model, stream flag, and
// the prompt cache key (for response caching).
func (s *Server) prepareOpenAIRequest(body []byte) ([]byte, string, string, bool, string, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, "", "", false, "", fmt.Errorf("request body is not valid OpenAI JSON: %w", err)
	}
	if payload == nil {
		return nil, "", "", false, "", fmt.Errorf("request body must be a JSON object")
	}

	var incomingModel string
	if raw := payload["model"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &incomingModel); err != nil {
			return nil, "", "", false, "", fmt.Errorf("model must be a string")
		}
	}
	var stream bool
	if raw := payload["stream"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &stream); err != nil {
			return nil, "", "", false, "", fmt.Errorf("stream must be a boolean")
		}
	}
	if stream {
		if raw, ok := payload["stream_options"]; !ok || string(raw) == "null" {
			payload["stream_options"] = json.RawMessage(`{"include_usage":true}`)
		}
	}

	targetModel := s.cfg.ResolveModel(incomingModel)
	payload["model"], _ = json.Marshal(targetModel)
	proxy.ApplyRawOpenAIPromptCache(payload, promptCacheOptionsFromConfig(s.cfg.Snapshot()))

	cacheKey := rawString(payload["prompt_cache_key"])

	upBody, err := json.Marshal(payload)
	if err != nil {
		return nil, "", "", false, "", fmt.Errorf("could not encode upstream request: %w", err)
	}
	return upBody, incomingModel, targetModel, stream, cacheKey, nil
}

func (s *Server) relayOpenAIJSON(
	w http.ResponseWriter,
	resp *http.Response,
	r *http.Request,
	incomingModel, targetModel string,
	stream bool,
	reqBody []byte,
	start time.Time,
	cacheKey string,
) {
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "api_error", "could not read upstream body: "+err.Error())
		s.logFailed(r.Context(), r, incomingModel, targetModel, stream,
			http.StatusBadGateway, err.Error(), reqBody, time.Since(start))
		return
	}
	if len(raw) > maxResponseBytes {
		const msg = "upstream response exceeded the maximum allowed size"
		writeOpenAIError(w, http.StatusBadGateway, "api_error", msg)
		s.logFailed(r.Context(), r, incomingModel, targetModel, stream,
			http.StatusBadGateway, msg, reqBody, time.Since(start))
		return
	}

	// Store successful non-streaming responses in cache.
	if !stream && resp.StatusCode == http.StatusOK {
		s.storeInCache(cacheKey, targetModel, raw)
	}

	copyOpenAIHeaders(w.Header(), resp.Header, false)
	status := resp.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(raw)

	if status >= http.StatusBadRequest {
		msg := strings.TrimSpace(string(raw))
		if extracted := extractOpenAIError(raw); extracted != "" {
			msg = extracted
		}
		s.logFailed(r.Context(), r, incomingModel, targetModel, stream,
			status, msg, reqBody, time.Since(start))
		return
	}

	var out struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage proxy.OpenAIUsage `json:"usage"`
	}
	_ = json.Unmarshal(raw, &out)
	stopReason := ""
	if len(out.Choices) > 0 {
		stopReason = out.Choices[0].FinishReason
	}
	s.logSuccessWithCache(r.Context(), r, incomingModel, targetModel, stream, status,
		out.Usage.PromptTokens, out.Usage.CompletionTokens, out.Usage.CachedPromptTokens(), 0, stopReason,
		string(reqBody), string(raw), time.Since(start))
}

func (s *Server) relayOpenAIStream(
	w http.ResponseWriter,
	resp *http.Response,
	r *http.Request,
	incomingModel, targetModel string,
	reqBody []byte,
	start time.Time,
	cacheKey string,
) {
	defer resp.Body.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "api_error",
			"streaming not supported by this server")
		return
	}

	reader := io.Reader(resp.Body)
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "api_error",
				"could not decompress upstream stream: "+err.Error())
			return
		}
		defer gz.Close()
		reader = gz
	}

	copyOpenAIHeaders(w.Header(), resp.Header, true)
	w.WriteHeader(resp.StatusCode)
	flusher.Flush()

	var responseLog strings.Builder
	relay := &openAIStreamRelay{
		dst:         w,
		flusher:     flusher,
		log:         &responseLog,
		logLimit:    s.cfg.Snapshot().MaxBodyLogBytes,
		cacheKey:    cacheKey,
		targetModel: targetModel,
	}
	if _, err := io.Copy(relay, reader); err != nil {
		errPayload, _ := json.Marshal(map[string]any{
			"error": map[string]any{
				"message": "upstream stream error: " + err.Error(),
				"type":    "api_error",
			},
		})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", errPayload)
		flusher.Flush()
		s.logFailed(r.Context(), r, incomingModel, targetModel, true,
			http.StatusBadGateway, err.Error(), reqBody, time.Since(start))
		return
	}

	// After stream completes, reconstruct a full response from buffered SSE
	// chunks and store in cache so subsequent identical streaming requests
	// are served instantly from cache.
	if resp.StatusCode == http.StatusOK && cacheKey != "" {
		relay.cacheResponse(s)
	}

	s.logSuccessWithCache(r.Context(), r, incomingModel, targetModel, true, resp.StatusCode,
		relay.inputTokens, relay.outputTokens, relay.cachedInputTokens, 0, relay.stopReason,
		string(reqBody), responseLog.String(), time.Since(start))
}

// openAIStreamRelay writes upstream bytes directly to the client and flushes
// every write. It observes complete SSE data lines for usage logging and
// buffers them for cache reconstruction after the stream ends.
type openAIStreamRelay struct {
	dst      io.Writer
	flusher  http.Flusher
	log      *strings.Builder
	logLimit int

	pending           []byte
	inputTokens       int
	outputTokens      int
	cachedInputTokens int
	stopReason        string

	// Cache fields: set by relayOpenAIStream before streaming.
	cacheKey    string
	targetModel string
	// dataBuf accumulates the JSON payload of each SSE data: line (without
	// the "data: " prefix or trailing newline), one per line.
	dataBuf bytes.Buffer
}

// cacheResponse reconstructs a full non-streaming OpenAI response from the
// buffered SSE data chunks and stores it in the server's response cache.
func (r *openAIStreamRelay) cacheResponse(s *Server) {
	if r.cacheKey == "" || r.dataBuf.Len() == 0 {
		return
	}
	body := r.reconstructFullResponse()
	if body == nil {
		return
	}
	s.storeInCache(r.cacheKey, r.targetModel, body)
}

// reconstructFullResponse parses all buffered SSE data payloads and builds a
// single non-streaming OpenAI Response JSON, suitable for cache storage.
func (r *openAIStreamRelay) reconstructFullResponse() []byte {
	var (
		id, model string
		created   int64
		finishReason string
		contentBuilder strings.Builder
		lastUsage *proxy.OpenAIUsage
	)

	scanner := bufio.NewScanner(&r.dataBuf)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var rawChunk map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &rawChunk); err != nil {
			continue
		}
		if v, ok := rawChunk["id"]; ok && len(v) > 0 && id == "" {
			json.Unmarshal(v, &id)
		}
		if v, ok := rawChunk["model"]; ok && len(v) > 0 && model == "" {
			json.Unmarshal(v, &model)
		}
		if v, ok := rawChunk["created"]; ok && len(v) > 0 && created == 0 {
			json.Unmarshal(v, &created)
		}

		var chunk proxy.OpenAIStreamChunk
		if json.Unmarshal([]byte(line), &chunk) != nil {
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
		return nil
	}
	if finishReason == "" {
		finishReason = "stop"
	}
	if id == "" {
		id = "chatcmpl-stream"
	}
	if created == 0 {
		created = time.Now().Unix()
	}

	respMap := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": contentBuilder.String(),
			},
			"finish_reason": finishReason,
			"logprobs":      nil,
		}},
		"usage": lastUsage,
	}
	body, _ := json.Marshal(respMap)
	return body
}

func (r *openAIStreamRelay) Write(p []byte) (int, error) {
	n, err := r.dst.Write(p)
	if n > 0 {
		appendLimited(r.log, string(p[:n]), r.logLimit)
		r.observe(p[:n])
		r.flusher.Flush()
	}
	return n, err
}

func (r *openAIStreamRelay) observe(p []byte) {
	r.pending = append(r.pending, p...)
	for {
		i := bytes.IndexByte(r.pending, '\n')
		if i < 0 {
			return
		}
		line := strings.TrimSuffix(string(r.pending[:i]), "\r")
		r.pending = r.pending[i+1:]
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk proxy.OpenAIStreamChunk
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if chunk.Usage != nil {
			r.inputTokens = chunk.Usage.PromptTokens
			r.outputTokens = chunk.Usage.CompletionTokens
			r.cachedInputTokens = chunk.Usage.CachedPromptTokens()
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason != nil {
				r.stopReason = *choice.FinishReason
			}
		}
		// Buffer the data line for cache reconstruction.
		if r.cacheKey != "" {
			r.dataBuf.WriteString(data)
			r.dataBuf.WriteByte('\n')
		}
	}
}

func copyOpenAIHeaders(dst, src http.Header, streaming bool) {
	for key, values := range src {
		lower := strings.ToLower(key)
		if isHopByHopHeader(lower) || lower == "content-length" || lower == "content-encoding" {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
	if dst.Get("Content-Type") == "" {
		if streaming {
			dst.Set("Content-Type", "text/event-stream")
		} else {
			dst.Set("Content-Type", "application/json")
		}
	}
	if streaming {
		dst.Set("Cache-Control", "no-cache")
		dst.Set("X-Accel-Buffering", "no")
	}
}

func isHopByHopHeader(lower string) bool {
	switch lower {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func appendLimited(dst *strings.Builder, value string, limit int) {
	if limit <= 0 {
		_, _ = dst.WriteString(value)
		return
	}
	if dst.Len() >= limit {
		return
	}
	remaining := limit - dst.Len()
	if len(value) > remaining {
		value = value[:remaining]
	}
	_, _ = dst.WriteString(value)
}
