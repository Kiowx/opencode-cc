package server

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
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

		upBody, incomingModel, targetModel, stream, err := s.prepareOpenAIRequest(body)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}

		cfg := s.cfg.Snapshot()
		upstream, zenKey, ok := s.cfg.NextUpstreamForKey(promptCacheKeyFromOpenAIBody(upBody))
		if !ok {
			const msg = "no upstream API key configured. Set one in the web panel (Settings → upstreams)."
			writeOpenAIError(w, http.StatusUnauthorized, "authentication_error", msg)
			s.logFailed(r.Context(), r, incomingModel, targetModel, stream,
				http.StatusUnauthorized, "no upstream api key", body, time.Since(start))
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
		upReq.Header.Set("User-Agent", "opencode-cc/1.1")
		if stream {
			upReq.Header.Set("Accept", "text/event-stream")
		} else {
			upReq.Header.Set("Accept", "application/json")
		}

		httpClient := s.upstreamClient(stream, cfg.RequestTimeoutSeconds)
		resp, err := httpClient.Do(upReq)
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "api_error", "upstream request failed: "+err.Error())
			s.logFailed(r.Context(), r, incomingModel, targetModel, stream,
				http.StatusBadGateway, err.Error(), body, time.Since(start))
			return
		}

		contentType := strings.ToLower(resp.Header.Get("Content-Type"))
		if stream && resp.StatusCode < http.StatusBadRequest &&
			(contentType == "" || strings.Contains(contentType, "text/event-stream")) {
			s.relayOpenAIStream(w, resp, r, incomingModel, targetModel, body, start)
			return
		}
		s.relayOpenAIJSON(w, resp, r, incomingModel, targetModel, stream, body, start)
	}
}

// prepareOpenAIRequest validates the JSON object and rewrites only its model
// field, preserving extensions used by different OpenAI-compatible clients.
func (s *Server) prepareOpenAIRequest(body []byte) ([]byte, string, string, bool, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, "", "", false, fmt.Errorf("request body is not valid OpenAI JSON: %w", err)
	}
	if payload == nil {
		return nil, "", "", false, fmt.Errorf("request body must be a JSON object")
	}

	var incomingModel string
	if raw := payload["model"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &incomingModel); err != nil {
			return nil, "", "", false, fmt.Errorf("model must be a string")
		}
	}
	var stream bool
	if raw := payload["stream"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &stream); err != nil {
			return nil, "", "", false, fmt.Errorf("stream must be a boolean")
		}
	}
	if stream {
		// Zen supports OpenAI's usage trailer. Request it when the client did
		// not specify stream_options so API-key quotas and dashboard stats stay
		// accurate without overriding an explicit client preference.
		if raw, ok := payload["stream_options"]; !ok || string(raw) == "null" {
			payload["stream_options"] = json.RawMessage(`{"include_usage":true}`)
		}
	}

	targetModel := s.cfg.ResolveModel(incomingModel)
	payload["model"], _ = json.Marshal(targetModel)
	proxy.ApplyRawOpenAIPromptCache(payload, promptCacheOptionsFromConfig(s.cfg.Snapshot()))
	upBody, err := json.Marshal(payload)
	if err != nil {
		return nil, "", "", false, fmt.Errorf("could not encode upstream request: %w", err)
	}
	return upBody, incomingModel, targetModel, stream, nil
}

func promptCacheKeyFromOpenAIBody(body []byte) string {
	var payload struct {
		PromptCacheKey string `json:"prompt_cache_key"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return ""
	}
	return strings.TrimSpace(payload.PromptCacheKey)
}

func (s *Server) relayOpenAIJSON(
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
		dst:      w,
		flusher:  flusher,
		log:      &responseLog,
		logLimit: s.cfg.Snapshot().MaxBodyLogBytes,
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

	s.logSuccessWithCache(r.Context(), r, incomingModel, targetModel, true, resp.StatusCode,
		relay.inputTokens, relay.outputTokens, relay.cachedInputTokens, 0, relay.stopReason,
		string(reqBody), responseLog.String(), time.Since(start))
}

// openAIStreamRelay writes upstream bytes directly to the client and flushes
// every write. It only observes complete SSE data lines for usage logging; it
// never re-encodes or otherwise changes the native OpenAI stream.
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
