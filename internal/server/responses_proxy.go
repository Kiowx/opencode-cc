package server

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Kiowx/opencode-cc/internal/config"
	"github.com/Kiowx/opencode-cc/internal/proxy"
)

// ResponsesProxy handles POST /v1/responses for Codex and other Responses API
// clients. Anthropic-native target models are translated through the upstream
// Messages API; other models use OpenAI Chat Completions translation.
func (s *Server) ResponsesProxy() http.HandlerFunc {
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
		in, err := proxy.ParseResponsesRequest(body)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		incomingModel := in.Model
		targetModel := s.cfg.ResolveModel(incomingModel)
		cfg := s.cfg.Snapshot()

		if cfg.NativeAnthropic && proxy.IsNativeAnthropicModel(targetModel) {
			stickyKey := strings.TrimSpace(in.PromptCacheKey)
			if stickyKey == "" {
				if chatReq, convErr := proxy.ConvertResponsesRequest(in, func(string) string { return targetModel }); convErr == nil {
					proxy.ApplyOpenAIPromptCache(chatReq, promptCacheOptionsFromConfig(cfg))
					stickyKey = chatReq.PromptCacheKey
				}
			}
			upstream, zenKey, ok := s.cfg.NextUpstreamForKey(stickyKey)
			if !ok {
				const msg = "no upstream API key configured. Set one in the web panel (Settings → upstreams)."
				writeOpenAIError(w, http.StatusUnauthorized, "authentication_error", msg)
				s.logFailed(r.Context(), r, incomingModel, targetModel, in.Stream,
					http.StatusUnauthorized, "no upstream api key", body, time.Since(start))
				return
			}
			s.proxyResponsesViaAnthropic(w, r, in, cfg, upstream, zenKey, incomingModel, targetModel, body, start)
			return
		}

		chatReq, err := proxy.ConvertResponsesRequest(in, func(string) string { return targetModel })
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		applyResponsesThinkingMapping(chatReq, targetModel, cfg)
		proxy.ApplyOpenAIPromptCache(chatReq, promptCacheOptionsFromConfig(cfg))
		upstream, zenKey, ok := s.cfg.NextUpstreamForKey(chatReq.PromptCacheKey)
		if !ok {
			const msg = "no upstream API key configured. Set one in the web panel (Settings → upstreams)."
			writeOpenAIError(w, http.StatusUnauthorized, "authentication_error", msg)
			s.logFailed(r.Context(), r, incomingModel, targetModel, in.Stream,
				http.StatusUnauthorized, "no upstream api key", body, time.Since(start))
			return
		}
		upBody, err := json.Marshal(chatReq)
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "api_error",
				"could not encode upstream request: "+err.Error())
			return
		}

		upURL := strings.TrimRight(upstream, "/") + "/v1/chat/completions"
		upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upURL, bytes.NewReader(upBody))
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "api_error",
				"could not build upstream request: "+err.Error())
			return
		}
		upReq.Header.Set("Authorization", "Bearer "+zenKey)
		upReq.Header.Set("Content-Type", "application/json")
		upReq.Header.Set("User-Agent", "opencode-cc/1.2")
		if in.Stream {
			upReq.Header.Set("Accept", "text/event-stream")
		} else {
			upReq.Header.Set("Accept", "application/json")
		}

		httpClient := s.upstreamClient(in.Stream, cfg.RequestTimeoutSeconds)
		resp, err := httpClient.Do(upReq)
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "api_error", "upstream request failed: "+err.Error())
			s.logFailed(r.Context(), r, incomingModel, targetModel, in.Stream,
				http.StatusBadGateway, err.Error(), body, time.Since(start))
			return
		}

		contentType := strings.ToLower(resp.Header.Get("Content-Type"))
		if in.Stream && resp.StatusCode < http.StatusBadRequest &&
			(contentType == "" || strings.Contains(contentType, "text/event-stream")) {
			s.relayResponsesStream(w, resp, r, incomingModel, targetModel, body, start)
			return
		}
		s.relayResponsesJSON(w, resp, r, incomingModel, targetModel, in.Stream, body, start)
	}
}

func (s *Server) proxyResponsesViaAnthropic(
	w http.ResponseWriter,
	r *http.Request,
	in *proxy.ResponsesRequest,
	cfg *config.Config,
	upstream, zenKey string,
	incomingModel, targetModel string,
	reqBody []byte,
	start time.Time,
) {
	anthReq, err := proxy.ConvertResponsesToAnthropicRequest(in, func(string) string { return targetModel })
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	upBody, err := json.Marshal(anthReq)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "api_error",
			"could not encode upstream request: "+err.Error())
		return
	}
	upBody, err = proxy.PrepareAnthropicPromptCacheBody(upBody, targetModel, promptCacheOptionsFromConfig(cfg))
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "api_error",
			"could not prepare upstream request: "+err.Error())
		return
	}

	upURL := strings.TrimRight(upstream, "/") + "/v1/messages"
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upURL, bytes.NewReader(upBody))
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "api_error",
			"could not build upstream request: "+err.Error())
		return
	}
	upReq.Header.Set("Authorization", "Bearer "+zenKey)
	upReq.Header.Set("x-api-key", zenKey)
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("User-Agent", "opencode-cc/1.3")
	if in.Stream {
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

	httpClient := s.upstreamClient(in.Stream, cfg.RequestTimeoutSeconds)
	resp, err := httpClient.Do(upReq)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "api_error", "upstream request failed: "+err.Error())
		s.logFailed(r.Context(), r, incomingModel, targetModel, in.Stream,
			http.StatusBadGateway, err.Error(), reqBody, time.Since(start))
		return
	}

	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if in.Stream && resp.StatusCode < http.StatusBadRequest &&
		(contentType == "" || strings.Contains(contentType, "text/event-stream")) {
		s.relayResponsesAnthropicStream(w, resp, r, incomingModel, targetModel, reqBody, start)
		return
	}
	s.relayResponsesAnthropicJSON(w, resp, r, incomingModel, targetModel, in.Stream, reqBody, start)
}

func (s *Server) relayResponsesAnthropicJSON(
	w http.ResponseWriter,
	resp *http.Response,
	r *http.Request,
	incomingModel, targetModel string,
	stream bool,
	reqBody []byte,
	start time.Time,
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
	if resp.StatusCode >= http.StatusBadRequest {
		message := extractAnthropicError(raw)
		if message == "" {
			message = extractOpenAIError(raw)
		}
		if message == "" {
			message = strings.TrimSpace(string(raw))
		}
		if message == "" {
			message = "upstream returned an error"
		}
		copyOpenAIHeaders(w.Header(), resp.Header, false)
		writeOpenAIError(w, resp.StatusCode, openAIErrorTypeForStatus(resp.StatusCode), message)
		s.logFailed(r.Context(), r, incomingModel, targetModel, stream,
			resp.StatusCode, message, reqBody, time.Since(start))
		return
	}

	var anthResp proxy.AnthropicResponse
	if err := json.Unmarshal(raw, &anthResp); err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "api_error",
			"could not parse upstream Anthropic response: "+err.Error())
		s.logFailed(r.Context(), r, incomingModel, targetModel, stream,
			http.StatusBadGateway, err.Error(), reqBody, time.Since(start))
		return
	}
	out := proxy.ConvertAnthropicResponseToResponses(&anthResp, incomingModel)
	writeJSON(w, http.StatusOK, out)

	stopReason := ""
	if anthResp.StopReason != nil {
		stopReason = *anthResp.StopReason
	}
	s.logSuccessWithCache(r.Context(), r, incomingModel, targetModel, stream, http.StatusOK,
		anthResp.Usage.InputTokens, anthResp.Usage.OutputTokens,
		anthResp.Usage.CacheReadInputTokens, anthResp.Usage.CacheCreationInputTokens,
		stopReason,
		string(reqBody), mustJSON(out), time.Since(start))
}

func (s *Server) relayResponsesJSON(
	w http.ResponseWriter,
	resp *http.Response,
	r *http.Request,
	incomingModel, targetModel string,
	stream bool,
	reqBody []byte,
	start time.Time,
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
	if resp.StatusCode >= http.StatusBadRequest {
		copyOpenAIHeaders(w.Header(), resp.Header, false)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(raw)
		message := extractOpenAIError(raw)
		if message == "" {
			message = strings.TrimSpace(string(raw))
		}
		s.logFailed(r.Context(), r, incomingModel, targetModel, stream,
			resp.StatusCode, message, reqBody, time.Since(start))
		return
	}

	chatResp, err := proxy.ParseOpenAIResponse(raw)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "api_error", err.Error())
		s.logFailed(r.Context(), r, incomingModel, targetModel, stream,
			http.StatusBadGateway, err.Error(), reqBody, time.Since(start))
		return
	}
	out := proxy.ConvertResponsesResponse(chatResp, incomingModel)
	writeJSON(w, http.StatusOK, out)

	stopReason := ""
	if len(chatResp.Choices) > 0 && chatResp.Choices[0].FinishReason != nil {
		stopReason = *chatResp.Choices[0].FinishReason
	}
	s.logSuccessWithCache(r.Context(), r, incomingModel, targetModel, stream, http.StatusOK,
		chatResp.Usage.PromptTokens, chatResp.Usage.CompletionTokens,
		chatResp.Usage.CachedPromptTokens(), 0, stopReason,
		string(reqBody), mustJSON(out), time.Since(start))
}

func (s *Server) relayResponsesStream(
	w http.ResponseWriter,
	resp *http.Response,
	r *http.Request,
	incomingModel, targetModel string,
	reqBody []byte,
	start time.Time,
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
		gzipReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "api_error",
				"could not decompress upstream stream: "+err.Error())
			return
		}
		defer gzipReader.Close()
		reader = gzipReader
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	responseLog := &limitedLogWriter{limit: s.cfg.Snapshot().MaxBodyLogBytes}
	converter, err := proxy.NewResponsesStreamConverter(io.MultiWriter(w, responseLog), incomingModel)
	if err != nil {
		s.logFailed(r.Context(), r, incomingModel, targetModel, true,
			http.StatusBadGateway, err.Error(), reqBody, time.Since(start))
		return
	}
	flusher.Flush()

	scanResult, scanErr := proxy.ScanOpenAIStreamWithStatus(reader, func(chunk *proxy.OpenAIStreamChunk) error {
		if err := converter.HandleChunk(chunk); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	})
	if scanErr != nil && !errors.Is(scanErr, io.EOF) {
		_ = converter.EmitError("upstream stream error: " + scanErr.Error())
		flusher.Flush()
		s.logFailed(r.Context(), r, incomingModel, targetModel, true,
			http.StatusBadGateway, scanErr.Error(), reqBody, time.Since(start))
		return
	}
	if !scanResult.SawDone && !scanResult.SawFinish {
		const msg = "upstream stream ended before finish_reason or [DONE]"
		_ = converter.EmitError(msg)
		flusher.Flush()
		s.logFailed(r.Context(), r, incomingModel, targetModel, true,
			http.StatusBadGateway, msg, reqBody, time.Since(start))
		return
	}
	if err := converter.Finalize(); err != nil {
		s.logFailed(r.Context(), r, incomingModel, targetModel, true,
			http.StatusBadGateway, err.Error(), reqBody, time.Since(start))
		return
	}
	flusher.Flush()

	s.logSuccessWithCache(r.Context(), r, incomingModel, targetModel, true, http.StatusOK,
		converter.InputTokens(), converter.OutputTokens(), converter.CachedInputTokens(), 0,
		converter.FinishReason(),
		string(reqBody), responseLog.String(), time.Since(start))
}

func (s *Server) relayResponsesAnthropicStream(
	w http.ResponseWriter,
	resp *http.Response,
	r *http.Request,
	incomingModel, targetModel string,
	reqBody []byte,
	start time.Time,
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
		gzipReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "api_error",
				"could not decompress upstream stream: "+err.Error())
			return
		}
		defer gzipReader.Close()
		reader = gzipReader
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	responseLog := &limitedLogWriter{limit: s.cfg.Snapshot().MaxBodyLogBytes}
	converter, err := proxy.NewResponsesStreamConverter(io.MultiWriter(w, responseLog), incomingModel)
	if err != nil {
		s.logFailed(r.Context(), r, incomingModel, targetModel, true,
			http.StatusBadGateway, err.Error(), reqBody, time.Since(start))
		return
	}
	flusher.Flush()

	var inputTokens, outputTokens int
	var cachedInputTokens, cacheCreationInputTokens int
	var sawAnthropicStop bool
	scanErr := scanAnthropicSSE(reader, func(event string, data []byte) error {
		var payload struct {
			Type    string `json:"type"`
			Message *struct {
				Usage proxy.AnthropicUsage `json:"usage"`
			} `json:"message,omitempty"`
			Index        int `json:"index,omitempty"`
			ContentBlock *struct {
				Type  string          `json:"type"`
				ID    string          `json:"id,omitempty"`
				Name  string          `json:"name,omitempty"`
				Text  string          `json:"text,omitempty"`
				Input json.RawMessage `json:"input,omitempty"`
			} `json:"content_block,omitempty"`
			Delta *struct {
				Type        string  `json:"type"`
				Text        string  `json:"text,omitempty"`
				PartialJSON string  `json:"partial_json,omitempty"`
				StopReason  *string `json:"stop_reason,omitempty"`
			} `json:"delta,omitempty"`
			Usage *struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage,omitempty"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error,omitempty"`
		}
		if err := json.Unmarshal(data, &payload); err != nil {
			return err
		}
		if payload.Type == "error" {
			message := "upstream stream error"
			if payload.Error != nil && payload.Error.Message != "" {
				message = payload.Error.Message
			}
			return errors.New(message)
		}
		switch payload.Type {
		case "message_start":
			if payload.Message != nil {
				inputTokens = payload.Message.Usage.InputTokens
				cachedInputTokens = payload.Message.Usage.CacheReadInputTokens
				cacheCreationInputTokens = payload.Message.Usage.CacheCreationInputTokens
				converter.SetUsage(inputTokens, outputTokens)
				converter.SetCachedInputTokens(cachedInputTokens)
			}
		case "content_block_start":
			if payload.ContentBlock == nil {
				return nil
			}
			switch payload.ContentBlock.Type {
			case "text":
				if payload.ContentBlock.Text != "" {
					if err := converter.HandleTextDelta(payload.ContentBlock.Text); err != nil {
						return err
					}
				}
			case "tool_use":
				if err := converter.HandleFunctionCallDelta(
					payload.Index,
					payload.ContentBlock.ID,
					payload.ContentBlock.Name,
					"",
				); err != nil {
					return err
				}
			}
		case "content_block_delta":
			if payload.Delta == nil {
				return nil
			}
			switch payload.Delta.Type {
			case "text_delta":
				if err := converter.HandleTextDelta(payload.Delta.Text); err != nil {
					return err
				}
			case "input_json_delta":
				if err := converter.HandleFunctionCallDelta(payload.Index, "", "", payload.Delta.PartialJSON); err != nil {
					return err
				}
			}
		case "message_delta":
			if payload.Usage != nil {
				outputTokens = payload.Usage.OutputTokens
				converter.SetUsage(inputTokens, outputTokens)
			}
			if payload.Delta != nil && payload.Delta.StopReason != nil {
				sawAnthropicStop = true
				converter.SetFinishReason(openAIFinishReasonFromAnthropic(*payload.Delta.StopReason))
			}
		case "message_stop":
			sawAnthropicStop = true
		}
		flusher.Flush()
		return nil
	})
	if scanErr != nil && !errors.Is(scanErr, io.EOF) {
		_ = converter.EmitError("upstream stream error: " + scanErr.Error())
		flusher.Flush()
		s.logFailed(r.Context(), r, incomingModel, targetModel, true,
			http.StatusBadGateway, scanErr.Error(), reqBody, time.Since(start))
		return
	}
	if !sawAnthropicStop {
		const msg = "upstream Anthropic stream ended before message_stop"
		_ = converter.EmitError(msg)
		flusher.Flush()
		s.logFailed(r.Context(), r, incomingModel, targetModel, true,
			http.StatusBadGateway, msg, reqBody, time.Since(start))
		return
	}
	if err := converter.Finalize(); err != nil {
		s.logFailed(r.Context(), r, incomingModel, targetModel, true,
			http.StatusBadGateway, err.Error(), reqBody, time.Since(start))
		return
	}
	flusher.Flush()

	s.logSuccessWithCache(r.Context(), r, incomingModel, targetModel, true, http.StatusOK,
		converter.InputTokens(), converter.OutputTokens(), cachedInputTokens, cacheCreationInputTokens,
		converter.FinishReason(),
		string(reqBody), responseLog.String(), time.Since(start))
}

func scanAnthropicSSE(r io.Reader, handle func(event string, data []byte) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var event string
	var dataLines []string
	flush := func() error {
		if len(dataLines) == 0 {
			event = ""
			return nil
		}
		currentEvent := event
		data := strings.Join(dataLines, "\n")
		event = ""
		dataLines = nil
		if strings.TrimSpace(data) == "[DONE]" {
			return io.EOF
		}
		return handle(currentEvent, []byte(data))
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}

func openAIFinishReasonFromAnthropic(reason string) string {
	switch reason {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	case "end_turn", "stop_sequence":
		return "stop"
	default:
		return reason
	}
}

func openAIErrorTypeForStatus(status int) string {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "authentication_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return "timeout_error"
	default:
		return "api_error"
	}
}

type limitedLogWriter struct {
	builder strings.Builder
	limit   int
}

func (w *limitedLogWriter) Write(data []byte) (int, error) {
	appendLimited(&w.builder, string(data), w.limit)
	return len(data), nil
}

func (w *limitedLogWriter) String() string {
	return w.builder.String()
}
