package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	cfgpkg "github.com/Kiowx/opencode-cc/internal/config"
	"github.com/Kiowx/opencode-cc/internal/proxy"
	"github.com/Kiowx/opencode-cc/internal/store"
)

// Proxy handles POST /v1/messages. Native Anthropic-capable target models go
// straight to the upstream Messages API; all other models are translated to
// OpenAI Chat Completions and converted back.
func (s *Server) Proxy() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ctx := r.Context()

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
		if err != nil {
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "could not read request body: "+err.Error())
			return
		}

		var areq proxy.AnthropicRequest
		if err := json.Unmarshal(body, &areq); err != nil {
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "request body is not valid Anthropic JSON: "+err.Error())
			return
		}

		// Resolve target model under a short read lock.
		cfg := s.cfg.Snapshot()
		nativeAnthropic := cfg.NativeAnthropic
		timeoutSeconds := cfg.RequestTimeoutSeconds
		targetModel := s.cfg.ResolveModel(areq.Model)

		// Pick the current upstream (sticky-primary).
		upstream, zenKey, ok := s.cfg.NextUpstream()
		if !ok {
			writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "no upstream API key configured. Set one in the web panel (Settings → upstreams).")
			s.logFailed(ctx, r, areq.Model, targetModel, areq.Stream, http.StatusUnauthorized, "no upstream api key", body, time.Since(start))
			return
		}

		hasWebSearch := shouldUseWebSearchShim(&areq)
		webSearchMode := cfg.ResolveWebSearchMode()

		// Convert to OpenAI struct early — needed for cache key regardless of path.
		oreq := proxy.ConvertRequest(&areq, func(string) string { return targetModel })
		applyThinkingBudgetMapping(oreq, &areq, targetModel, cfg)
		proxy.ApplyOpenAIPromptCache(oreq, promptCacheOptionsFromConfig(cfg))

		// Attempt to serve from local response cache (non-streaming only).
		if !areq.Stream && s.tryServeFromCache(w, r, oreq.PromptCacheKey, areq.Model,
			targetModel, false, string(body), r.URL.Path, start) {
			return
		}

		if hasWebSearch && webSearchMode == cfgpkg.WebSearchModeNative {
			searchUpstream, searchKey := cfg.ResolveWebSearchUpstream(upstream, zenKey)
			searchModel := cfg.ResolveWebSearchModel(targetModel)
			s.proxyNativeAnthropic(w, r, body, &areq, searchUpstream, searchKey, searchModel, timeoutSeconds, start, oreq.PromptCacheKey)
			return
		}

		if nativeAnthropic && proxy.IsNativeAnthropicModel(targetModel) &&
			!(hasWebSearch && webSearchMode == cfgpkg.WebSearchModeTranslate) {
			s.proxyNativeAnthropic(w, r, body, &areq, upstream, zenKey, targetModel, timeoutSeconds, start, oreq.PromptCacheKey)
			return
		}

		if s.handleWebSearchShim(w, r, body, &areq, oreq, upstream, zenKey, targetModel, cfg, timeoutSeconds, start) {
			return
		}

		// Marshal the upstream request.
		upBody, err := json.Marshal(oreq)
		if err != nil {
			writeAnthropicError(w, http.StatusInternalServerError, "api_error", "could not encode upstream request: "+err.Error())
			return
		}

		// Retry loop: on 429 immediately fail over to the next upstream API key.
		const maxRetries = 3
		var resp *http.Response
		for attempt := 0; attempt <= maxRetries; attempt++ {
			curUpstream, curZenKey := upstream, zenKey
			if attempt > 0 {
				var ok bool
				curUpstream, curZenKey, ok = s.cfg.NextUpstream()
				if !ok {
					writeAnthropicError(w, http.StatusBadGateway, "api_error", "no upstream available")
					s.logFailed(ctx, r, areq.Model, targetModel, areq.Stream, http.StatusBadGateway, "no upstream available", body, time.Since(start))
					return
				}
			}

			upURL := strings.TrimRight(curUpstream, "/") + "/v1/chat/completions"
			upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upURL, bytes.NewReader(upBody))
			if err != nil {
				writeAnthropicError(w, http.StatusInternalServerError, "api_error", "could not build upstream request: "+err.Error())
				return
			}
			upReq.Header.Set("Content-Type", "application/json")
			upReq.Header.Set("Authorization", "Bearer "+curZenKey)
			if areq.Stream {
				upReq.Header.Set("Accept", "text/event-stream")
			} else {
				upReq.Header.Set("Accept", "application/json")
			}
			upReq.Header.Set("User-Agent", "opencode-cc/1.0")
			if v := r.Header.Get("anthropic-version"); v != "" {
				upReq.Header.Set("anthropic-version", v)
			}

			httpClient := s.upstreamClient(areq.Stream, timeoutSeconds)
			resp, err = httpClient.Do(upReq)
			if err != nil {
				writeAnthropicError(w, http.StatusBadGateway, "api_error", "upstream request failed: "+err.Error())
				s.logFailed(ctx, r, areq.Model, targetModel, areq.Stream, http.StatusBadGateway, err.Error(), body, time.Since(start))
				return
			}

			if resp.StatusCode == http.StatusTooManyRequests && attempt < maxRetries {
				s.cfg.MarkUpstreamFailed()
				resp.Body.Close()
				log.Printf("upstream 429 on %s attempt %d, failover to next key", areq.Model, attempt)
				continue
			}
			break
		}
		if resp == nil {
			writeAnthropicError(w, http.StatusTooManyRequests, "rate_limit_error", "all upstream keys returned 429")
			s.logFailed(ctx, r, areq.Model, targetModel, areq.Stream, http.StatusTooManyRequests, "all upstream keys returned 429", body, time.Since(start))
			return
		}

		// Non-2xx: failover on non-recoverable errors, then pass upstream error back.
		if resp.StatusCode >= 400 {
			if shouldFailover(resp.StatusCode) {
				s.cfg.MarkUpstreamFailed()
			}
			s.passUpstreamError(w, resp, r, areq.Model, targetModel, body, start)
			return
		}

		if areq.Stream {
			s.handleStreamResponse(w, resp, r, areq.Model, targetModel, body, start)
		} else {
			s.handleNonStreamResponse(w, resp, r, areq.Model, targetModel, body, start, oreq.PromptCacheKey)
		}
	}
}

func (s *Server) proxyNativeAnthropic(
	w http.ResponseWriter,
	r *http.Request,
	body []byte,
	areq *proxy.AnthropicRequest,
	upstream, zenKey, targetModel string,
	timeoutSeconds int,
	start time.Time,
	cacheKey string,
) {
	upBody, err := proxy.PrepareAnthropicPromptCacheBody(body, targetModel, promptCacheOptionsFromConfig(s.cfg.Snapshot()))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
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
				writeAnthropicError(w, http.StatusBadGateway, "api_error", "no upstream available")
				s.logFailed(r.Context(), r, areq.Model, targetModel, areq.Stream, http.StatusBadGateway, "no upstream available", body, time.Since(start))
				return
			}
		}

		upURL := strings.TrimRight(curUpstream, "/") + "/v1/messages"
		upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upURL, bytes.NewReader(upBody))
		if err != nil {
			writeAnthropicError(w, http.StatusInternalServerError, "api_error", "could not build upstream request: "+err.Error())
			return
		}
		upReq.Header.Set("Content-Type", "application/json")
		upReq.Header.Set("Authorization", "Bearer "+curZenKey)
		upReq.Header.Set("x-api-key", curZenKey)
		upReq.Header.Set("User-Agent", "opencode-cc/1.3")
		if areq.Stream {
			upReq.Header.Set("Accept", "text/event-stream")
		} else {
			upReq.Header.Set("Accept", "application/json")
		}
		if version := r.Header.Get("anthropic-version"); version != "" {
			upReq.Header.Set("anthropic-version", version)
		} else {
			upReq.Header.Set("anthropic-version", "2023-06-01")
		}
		if beta := r.Header.Get("anthropic-beta"); beta != "" {
			upReq.Header.Set("anthropic-beta", beta)
		}

		httpClient := s.upstreamClient(areq.Stream, timeoutSeconds)
		resp, err = httpClient.Do(upReq)
		if err != nil {
			writeAnthropicError(w, http.StatusBadGateway, "api_error", "upstream request failed: "+err.Error())
			s.logFailed(r.Context(), r, areq.Model, targetModel, areq.Stream, http.StatusBadGateway, err.Error(), body, time.Since(start))
			return
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt < maxRetries {
			s.cfg.MarkUpstreamFailed()
			resp.Body.Close()
			log.Printf("upstream 429 on %s attempt %d, failover to next key", areq.Model, attempt)
			continue
		}
		break
	}
	if resp == nil {
		writeAnthropicError(w, http.StatusTooManyRequests, "rate_limit_error", "all upstream keys returned 429")
		s.logFailed(r.Context(), r, areq.Model, targetModel, areq.Stream, http.StatusTooManyRequests, "all upstream keys returned 429", body, time.Since(start))
		return
	}

	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if areq.Stream && resp.StatusCode < http.StatusBadRequest &&
		(contentType == "" || strings.Contains(contentType, "text/event-stream")) {
		s.relayNativeAnthropicStream(w, resp, r, areq.Model, targetModel, body, start)
		return
	}
	s.relayNativeAnthropicJSON(w, resp, r, areq.Model, targetModel, areq.Stream, body, start, cacheKey)
}

func (s *Server) relayNativeAnthropicJSON(
	w http.ResponseWriter,
	resp *http.Response,
	r *http.Request,
	inModel, target string,
	stream bool,
	reqBody []byte,
	start time.Time,
	cacheKey string,
) {
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "could not read upstream body: "+err.Error())
		s.logFailed(r.Context(), r, inModel, target, stream, http.StatusBadGateway, err.Error(), reqBody, time.Since(start))
		return
	}
	if len(raw) > maxResponseBytes {
		const msg = "upstream response exceeded the maximum allowed size"
		writeAnthropicError(w, http.StatusBadGateway, "api_error", msg)
		s.logFailed(r.Context(), r, inModel, target, stream, http.StatusBadGateway, msg, reqBody, time.Since(start))
		return
	}

	// Store in response cache before writing the response.
	if cacheKey != "" && !stream && resp.StatusCode == http.StatusOK {
		if openAIBody, err := proxy.AnthropicToOpenAIJSON(raw); err == nil {
			s.storeInCache(cacheKey, target, openAIBody)
		}
	}

	copyOpenAIHeaders(w.Header(), resp.Header, false)
	status := resp.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(raw)

	if status >= http.StatusBadRequest {
		msg := extractAnthropicError(raw)
		if msg == "" {
			msg = extractOpenAIError(raw)
		}
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		s.logFailed(r.Context(), r, inModel, target, stream, status, msg, reqBody, time.Since(start))
		return
	}

	var out struct {
		StopReason *string              `json:"stop_reason"`
		Usage      proxy.AnthropicUsage `json:"usage"`
	}
	_ = json.Unmarshal(raw, &out)
	s.logSuccessWithCache(r.Context(), r, inModel, target, stream, status,
		out.Usage.InputTokens, out.Usage.OutputTokens,
		out.Usage.CacheReadInputTokens, out.Usage.CacheCreationInputTokens,
		stopReasonStr(out.StopReason),
		string(reqBody), string(raw), time.Since(start))
}

func (s *Server) relayNativeAnthropicStream(
	w http.ResponseWriter,
	resp *http.Response,
	r *http.Request,
	inModel, target string,
	reqBody []byte,
	start time.Time,
) {
	defer resp.Body.Close()
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "streaming not supported by this server")
		return
	}

	reader := io.Reader(resp.Body)
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			writeAnthropicError(w, http.StatusBadGateway, "api_error", "could not decompress upstream stream: "+err.Error())
			return
		}
		defer gz.Close()
		reader = gz
	}

	copyOpenAIHeaders(w.Header(), resp.Header, true)
	w.WriteHeader(resp.StatusCode)
	flusher.Flush()

	var responseLog strings.Builder
	relay := &nativeAnthropicStreamRelay{
		dst:      w,
		flusher:  flusher,
		log:      &responseLog,
		logLimit: s.cfg.Snapshot().MaxBodyLogBytes,
	}
	if _, err := io.Copy(relay, reader); err != nil {
		errPayload, _ := json.Marshal(map[string]any{
			"type":  "error",
			"error": map[string]string{"type": "api_error", "message": "upstream stream error: " + err.Error()},
		})
		_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", errPayload)
		flusher.Flush()
		s.logFailed(r.Context(), r, inModel, target, true, http.StatusBadGateway, err.Error(), reqBody, time.Since(start))
		return
	}

	s.logSuccessWithCache(r.Context(), r, inModel, target, true, resp.StatusCode,
		relay.inputTokens, relay.outputTokens,
		relay.cachedInputTokens, relay.cacheCreationInputTokens,
		relay.stopReason,
		string(reqBody), responseLog.String(), time.Since(start))
}

type nativeAnthropicStreamRelay struct {
	dst      io.Writer
	flusher  http.Flusher
	log      *strings.Builder
	logLimit int

	pending                  []byte
	inputTokens              int
	outputTokens             int
	cachedInputTokens        int
	cacheCreationInputTokens int
	stopReason               string
}

func (r *nativeAnthropicStreamRelay) Write(p []byte) (int, error) {
	n, err := r.dst.Write(p)
	if n > 0 {
		appendLimited(r.log, string(p[:n]), r.logLimit)
		r.observe(p[:n])
		r.flusher.Flush()
	}
	return n, err
}

func (r *nativeAnthropicStreamRelay) observe(p []byte) {
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
		var event struct {
			Message struct {
				Usage proxy.AnthropicUsage `json:"usage"`
			} `json:"message"`
			Delta struct {
				StopReason *string `json:"stop_reason"`
			} `json:"delta"`
			Usage *struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &event) != nil {
			continue
		}
		if event.Message.Usage.InputTokens > 0 ||
			event.Message.Usage.CacheReadInputTokens > 0 ||
			event.Message.Usage.CacheCreationInputTokens > 0 {
			r.inputTokens = event.Message.Usage.InputTokens
			r.cachedInputTokens = event.Message.Usage.CacheReadInputTokens
			r.cacheCreationInputTokens = event.Message.Usage.CacheCreationInputTokens
		}
		if event.Usage != nil {
			r.outputTokens = event.Usage.OutputTokens
		}
		if event.Delta.StopReason != nil {
			r.stopReason = *event.Delta.StopReason
		}
	}
}

// passUpstreamError reads the upstream error body and returns an Anthropic-
// shaped error to the client, while logging the failed request.
func (s *Server) passUpstreamError(w http.ResponseWriter, resp *http.Response, r *http.Request, inModel, target string, reqBody []byte, start time.Time) {
	defer resp.Body.Close()
	eb, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	msg := strings.TrimSpace(string(eb))
	// Try to extract OpenAI error message.
	if em := extractOpenAIError(eb); em != "" {
		msg = em
	}
	if msg == "" {
		msg = fmt.Sprintf("upstream returned status %d", resp.StatusCode)
	}
	errType := "api_error"
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		errType = "authentication_error"
	case http.StatusTooManyRequests:
		errType = "rate_limit_error"
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		errType = "timeout_error"
	}
	writeAnthropicError(w, resp.StatusCode, errType, msg)
	s.logFailed(r.Context(), r, inModel, target, false, resp.StatusCode, msg, reqBody, time.Since(start))
}

// handleNonStreamResponse decodes the upstream JSON, converts it, and writes
// the Anthropic response.
func (s *Server) handleNonStreamResponse(w http.ResponseWriter, resp *http.Response, r *http.Request, inModel, target string, reqBody []byte, start time.Time, cacheKey string) {
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "could not read upstream body: "+err.Error())
		s.logFailed(r.Context(), r, inModel, target, false, http.StatusBadGateway, err.Error(), reqBody, time.Since(start))
		return
	}
	// Store in response cache before parsing (so we cache the raw upstream body).
	if resp.StatusCode == http.StatusOK {
		s.storeInCache(cacheKey, target, raw)
	}
	oresp, err := proxy.ParseOpenAIResponse(raw)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "could not parse upstream response: "+err.Error())
		s.logFailed(r.Context(), r, inModel, target, false, http.StatusBadGateway, err.Error(), reqBody, time.Since(start))
		return
	}
	aresp := proxy.ConvertResponse(oresp, inModel)
	filterUndeclaredToolUses(aresp, areqToolsFromBody(reqBody))
	writeJSON(w, http.StatusOK, aresp)

	// Log a success row.
	s.logSuccessWithCache(r.Context(), r, inModel, target, false, http.StatusOK,
		aresp.Usage.InputTokens, aresp.Usage.OutputTokens,
		aresp.Usage.CacheReadInputTokens, aresp.Usage.CacheCreationInputTokens,
		stopReasonStr(aresp.StopReason),
		string(reqBody), mustJSON(aresp), time.Since(start))
}

// handleStreamResponse proxies the SSE stream, converting each OpenAI chunk to
// Anthropic events. It flushes continuously so the client sees real-time data.
func (s *Server) handleStreamResponse(w http.ResponseWriter, resp *http.Response, r *http.Request, inModel, target string, reqBody []byte, start time.Time) {
	defer resp.Body.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "streaming not supported by this server")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	var stopSeq *string
	stopReason := ""
	var inputTok, outputTok int
	var cachedInputTok int

	conv, err := proxy.NewStreamConverter(w, inModel, stopSeq)
	if err != nil {
		return
	}
	conv.RestrictTools(anthropicToolNamesFromBody(reqBody))

	// Read the upstream body, transparently decompressing gzip if needed.
	bodyReader := io.Reader(resp.Body)
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gz, gerr := gzip.NewReader(resp.Body)
		if gerr == nil {
			defer gz.Close()
			bodyReader = gz
		}
	}

	streamErr := proxy.ScanOpenAIStream(bodyReader, func(chunk *proxy.OpenAIStreamChunk) error {
		if chunk.Usage != nil {
			inputTok = chunk.Usage.PromptTokens
			outputTok = chunk.Usage.CompletionTokens
			cachedInputTok = chunk.Usage.CachedPromptTokens()
		}
		return conv.HandleChunk(chunk)
	})

	// Finalize the stream: emit content_block_stop (if open) + message_delta
	// (carrying usage) + message_stop. If the upstream errored mid-stream,
	// surface it as an error event first.
	if streamErr != nil && streamErr.Error() != "EOF" {
		_ = conv.EmitError("api_error", "upstream stream error: "+streamErr.Error())
		stopReason = "stream_error"
		_ = conv.Finalize(stopReason)
	} else {
		_ = conv.Finalize("end_turn")
		stopReason = "end_turn"
	}
	_ = conv.Flush()
	flusher.Flush()

	// Log.
	s.logSuccessWithCache(r.Context(), r, inModel, target, true, http.StatusOK,
		inputTok, outputTok, cachedInputTok, 0, stopReason, string(reqBody), "[streamed]", time.Since(start))
}

// CountTokens handles POST /v1/messages/count_tokens with a rough estimate
// (~4 chars/token), which is all Claude Code uses it for.
func (s *Server) CountTokens() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes))
		var areq proxy.AnthropicRequest
		_ = json.Unmarshal(body, &areq)
		tokens := estimateTokens(&areq)
		writeJSON(w, http.StatusOK, proxy.CountTokensResponse{InputTokens: tokens})
	}
}

// estimateTokens returns a rough token count for the request (~4 chars/tok).
func estimateTokens(areq *proxy.AnthropicRequest) int {
	total := 0
	for _, b := range areq.System.Blocks {
		total += len(b.Text)
	}
	for _, m := range areq.Messages {
		if m.Content.IsStr {
			total += len(m.Content.Text)
			continue
		}
		for _, b := range m.Content.Blocks {
			total += len(b.Text)
		}
	}
	if total == 0 {
		return 1
	}
	return total/4 + 1
}

// ---- logging helpers ----

func (s *Server) logSuccess(ctx context.Context, r *http.Request, inModel, target string, stream bool, status, inputTok, outputTok int, stopReason, reqBody, respBody string, dur time.Duration) {
	s.logSuccessWithCache(ctx, r, inModel, target, stream, status, inputTok, outputTok, 0, 0, stopReason, reqBody, respBody, dur)
}

func (s *Server) logSuccessWithCache(ctx context.Context, r *http.Request, inModel, target string, stream bool, status, inputTok, outputTok, cachedInputTok, cacheCreationInputTok int, stopReason, reqBody, respBody string, dur time.Duration) {
	apiKey := APIKeyFromContext(ctx)
	if !s.shouldLog() {
		// Still record usage for quota even if request logging is off.
		s.recordUsage(apiKey, inputTok+outputTok, 1)
		return
	}
	if s.cfg.Snapshot().MaxBodyLogBytes > 0 {
		reqBody = truncate(reqBody, s.cfg.Snapshot().MaxBodyLogBytes)
		respBody = truncate(respBody, s.cfg.Snapshot().MaxBodyLogBytes)
	}
	var keyID int64
	if apiKey != nil {
		keyID = apiKey.ID
	}
	go func() {
		bg := context.Background()
		_ = s.store.InsertRequest(bg, &store.RequestRow{
			Ts:                       time.Now(),
			Method:                   r.Method,
			Path:                     r.URL.Path,
			IncomingModel:            inModel,
			TargetModel:              target,
			Stream:                   stream,
			Status:                   status,
			DurationMs:               dur.Milliseconds(),
			InputTokens:              inputTok,
			OutputTokens:             outputTok,
			CachedInputTokens:        cachedInputTok,
			CacheCreationInputTokens: cacheCreationInputTok,
			StopReason:               stopReason,
			ReqBody:                  reqBody,
			RespBody:                 respBody,
			APIKeyID:                 keyID,
		})
	}()
	s.recordUsage(apiKey, inputTok+outputTok, 1)
}

// recordUsage bumps the key's quota counters (lifetime + daily). No-op if no
// authenticated key is present.
func (s *Server) recordUsage(apiKey *store.APIKey, tokens, requests int) {
	if apiKey == nil {
		return
	}
	id := apiKey.ID
	go func() { _ = s.store.RecordUsage(context.Background(), id, tokens, requests) }()
}

func (s *Server) logFailed(ctx context.Context, r *http.Request, inModel, target string, stream bool, status int, errMsg string, reqBody []byte, dur time.Duration) {
	apiKey := APIKeyFromContext(ctx)
	// A failed request still counts as one request against quota (but we don't
	// charge tokens for requests that never produced a model response).
	s.recordUsage(apiKey, 0, 1)
	if !s.shouldLog() {
		return
	}
	mb := s.cfg.Snapshot().MaxBodyLogBytes
	reqStr := string(reqBody)
	if mb > 0 {
		reqStr = truncate(reqStr, mb)
	}
	var keyID int64
	if apiKey != nil {
		keyID = apiKey.ID
	}
	go func() {
		_ = s.store.InsertRequest(context.Background(), &store.RequestRow{
			Ts:            time.Now(),
			Method:        r.Method,
			Path:          r.URL.Path,
			IncomingModel: inModel,
			TargetModel:   target,
			Stream:        stream,
			Status:        status,
			DurationMs:    dur.Milliseconds(),
			StopReason:    "error",
			Error:         truncate(errMsg, 2048),
			ReqBody:       reqStr,
			APIKeyID:      keyID,
		})
	}()
}

func (s *Server) shouldLog() bool {
	s.cfg.RLock()
	defer s.cfg.RUnlock()
	return s.cfg.LogRequests
}

func stopReasonStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func areqToolsFromBody(body []byte) []proxy.AnthropicTool {
	var req proxy.AnthropicRequest
	if json.Unmarshal(body, &req) != nil {
		return nil
	}
	return req.Tools
}

func anthropicToolNamesFromBody(body []byte) []string {
	tools := areqToolsFromBody(body)
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return names
}

func filterUndeclaredToolUses(resp *proxy.AnthropicResponse, tools []proxy.AnthropicTool) {
	if resp == nil {
		return
	}
	allowed := make(map[string]string, len(tools))
	for _, tool := range tools {
		allowed[tool.Name] = tool.Name
	}
	filtered := resp.Content[:0]
	removed := false
	for _, block := range resp.Content {
		if block.Type == "tool_use" {
			name, ok := canonicalDeclaredToolName(block.Name, allowed)
			if !ok {
				removed = true
				continue
			}
			block.Name = name
		}
		filtered = append(filtered, block)
	}
	resp.Content = filtered
	if removed && resp.StopReason != nil && *resp.StopReason == "tool_use" {
		stop := "end_turn"
		resp.StopReason = &stop
	}
}

func canonicalDeclaredToolName(name string, allowed map[string]string) (string, bool) {
	if canonical, ok := allowed[name]; ok {
		return canonical, true
	}
	for allowedName, canonical := range allowed {
		if strings.EqualFold(allowedName, name) {
			return canonical, true
		}
	}
	return "", false
}

// extractOpenAIError pulls the human message out of an OpenAI error envelope.
func extractOpenAIError(b []byte) string {
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return ""
	}
	if env.Error.Message != "" {
		return env.Error.Message
	}
	return env.Message
}

func extractAnthropicError(b []byte) string {
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return ""
	}
	return env.Error.Message
}

// shouldFailover returns true when the upstream HTTP status indicates the API
// key is exhausted or invalid, triggering a failover to the next upstream.
// 401/403 = bad key, 429 = rate-limited/quota exhausted, 507 = insufficient quota.
func shouldFailover(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden,
		http.StatusTooManyRequests, httpStatusInsufficientStorage:
		return true
	default:
		return status >= 500
	}
}

const (
	httpStatusInsufficientStorage = 507
	maxRequestBytes              = 32 << 20 // 32 MiB
	maxResponseBytes             = 32 << 20
)
