package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Kiowx/opencode-cc/internal/config"
	"github.com/Kiowx/opencode-cc/internal/proxy"
)

type webSearchCall struct {
	ToolCall       proxy.OpenAIToolCall
	Query          string
	AllowedDomains []string
	BlockedDomains []string
}

func (s *Server) handleWebSearchShim(
	w http.ResponseWriter,
	r *http.Request,
	reqBody []byte,
	areq *proxy.AnthropicRequest,
	oreq *proxy.OpenAIRequest,
	upstream, zenKey, targetModel string,
	cfg *config.Config,
	timeoutSeconds int,
	start time.Time,
) bool {
	if !shouldUseWebSearchShim(areq) {
		return false
	}
	if cfg != nil && cfg.ResolveWebSearchMode() == config.WebSearchModeNative {
		return false
	}
	searchModel := targetModel
	if cfg != nil {
		searchModel = cfg.ResolveWebSearchModel(targetModel)
	}

	var queryResp *proxy.OpenAIResponse
	var call webSearchCall
	var ok bool
	if requiresWebSearch(areq) {
		call, ok = fallbackWebSearchCall(areq)
		if !ok {
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "web_search requires a non-empty query")
			s.logFailed(r.Context(), r, areq.Model, targetModel, areq.Stream, http.StatusBadRequest, "web_search requires a non-empty query", reqBody, time.Since(start))
			return true
		}
	} else {
		queryReq := cloneOpenAIRequest(oreq)
		queryReq.Stream = false
		queryReq.StreamOptions = nil
		prepareWebSearchModelRequest(&queryReq, areq, searchModel, cfg, "query")

		var err error
		queryResp, err = s.doOpenAIChat(r.Context(), upstream, zenKey, &queryReq, false, timeoutSeconds)
		if err != nil {
			writeAnthropicError(w, http.StatusBadGateway, "api_error", err.Error())
			s.logFailed(r.Context(), r, areq.Model, targetModel, areq.Stream, http.StatusBadGateway, err.Error(), reqBody, time.Since(start))
			return true
		}

		call, ok = extractWebSearchCall(queryResp)
		if !ok {
			aresp := proxy.ConvertResponse(queryResp, areq.Model)
			filterUndeclaredToolUses(aresp, areq.Tools)
			if areq.Stream {
				s.writeSyntheticAnthropicStream(w, r, areq.Model, targetModel, reqBody, start, aresp)
			} else {
				writeJSON(w, http.StatusOK, aresp)
				s.logSuccessWithCache(r.Context(), r, areq.Model, targetModel, false, http.StatusOK,
					aresp.Usage.InputTokens, aresp.Usage.OutputTokens,
					aresp.Usage.CacheReadInputTokens, aresp.Usage.CacheCreationInputTokens,
					stopReasonStr(aresp.StopReason),
					string(reqBody), mustJSON(aresp), time.Since(start))
			}
			return true
		}
	}
	if call.Query == "" {
		call.Query = lastAnthropicUserText(areq)
	}

	results, searchErr := webSearchProvider(r.Context(), call.Query, call.AllowedDomains, call.BlockedDomains, 5)
	serverToolID := "srvtoolu_" + proxyRandHex(24)
	searchBlocks := webSearchServerBlocks(serverToolID, call, results, searchErr)

	answerResp := &proxy.AnthropicResponse{
		ID:    "msg_" + proxyRandHex(24),
		Type:  "message",
		Role:  "assistant",
		Model: areq.Model,
		Usage: proxy.AnthropicUsage{
			ServerToolUse: &proxy.AnthropicServerToolUse{WebSearchRequests: 1},
		},
	}
	if queryResp != nil {
		answerResp.Usage.InputTokens = queryResp.Usage.PromptTokens
		answerResp.Usage.OutputTokens = queryResp.Usage.CompletionTokens
		answerResp.Usage.CacheReadInputTokens = queryResp.Usage.CachedPromptTokens()
	}
	endTurn := "end_turn"
	answerResp.StopReason = &endTurn

	if searchErr != nil || len(results) == 0 {
		answerResp.Content = append(answerResp.Content, searchBlocks...)
		answerResp.Content = append(answerResp.Content, proxy.AnthropicContent{
			Type: "text",
			Text: "Web search did not return usable results.",
		})
	} else {
		answerReq := buildWebSearchAnswerRequest(oreq, call, results, areq, searchModel, cfg)
		finalOpenAIResp, err := s.doOpenAIChat(r.Context(), upstream, zenKey, answerReq, false, timeoutSeconds)
		if err != nil {
			answerResp.Content = append(answerResp.Content, searchBlocks...)
			answerResp.Content = append(answerResp.Content, fallbackSearchSummary(results))
		} else {
			finalAnthResp := proxy.ConvertResponse(finalOpenAIResp, areq.Model)
			answerResp.Content = append(answerResp.Content, searchBlocks...)
			answerResp.Content = append(answerResp.Content, finalTextLikeContent(finalAnthResp.Content)...)
			if len(answerResp.Content) == len(searchBlocks) {
				answerResp.Content = append(answerResp.Content, fallbackSearchSummary(results))
			}
			answerResp.Usage.InputTokens += finalOpenAIResp.Usage.PromptTokens
			answerResp.Usage.OutputTokens += finalOpenAIResp.Usage.CompletionTokens
			answerResp.Usage.CacheReadInputTokens += finalOpenAIResp.Usage.CachedPromptTokens()
			answerResp.Usage.CacheCreationInputTokens += finalAnthResp.Usage.CacheCreationInputTokens
			if finalAnthResp.StopReason != nil && *finalAnthResp.StopReason == "max_tokens" {
				maxTokens := "max_tokens"
				answerResp.StopReason = &maxTokens
			}
		}
	}

	if areq.Stream {
		s.writeSyntheticAnthropicStream(w, r, areq.Model, targetModel, reqBody, start, answerResp)
		return true
	}
	writeJSON(w, http.StatusOK, answerResp)
	s.logSuccessWithCache(r.Context(), r, areq.Model, targetModel, false, http.StatusOK,
		answerResp.Usage.InputTokens, answerResp.Usage.OutputTokens,
		answerResp.Usage.CacheReadInputTokens, answerResp.Usage.CacheCreationInputTokens,
		stopReasonStr(answerResp.StopReason),
		string(reqBody), mustJSON(answerResp), time.Since(start))
	return true
}

func (s *Server) writeSyntheticAnthropicStream(
	w http.ResponseWriter,
	r *http.Request,
	inModel, targetModel string,
	reqBody []byte,
	start time.Time,
	resp *proxy.AnthropicResponse,
) {
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

	responseLog := &strings.Builder{}
	write := func(event string, payload any) {
		b, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		appendLimited(responseLog, fmt.Sprintf("event: %s\ndata: %s\n\n", event, b), s.cfg.Snapshot().MaxBodyLogBytes)
		flusher.Flush()
	}

	write("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            resp.ID,
			"type":          "message",
			"role":          "assistant",
			"model":         inModel,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         resp.Usage,
		},
	})
	write("ping", map[string]string{"type": "ping"})

	for i, block := range resp.Content {
		switch block.Type {
		case "text":
			startBlock := proxy.AnthropicContent{Type: "text", Text: ""}
			write("content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         i,
				"content_block": startBlock,
			})
			if block.Text != "" {
				write("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": i,
					"delta": map[string]string{
						"type": "text_delta",
						"text": block.Text,
					},
				})
			}
			write("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
		case "thinking":
			startBlock := proxy.AnthropicContent{Type: "thinking", Thinking: ""}
			write("content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         i,
				"content_block": startBlock,
			})
			if block.Thinking != "" {
				write("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": i,
					"delta": map[string]string{
						"type":     "thinking_delta",
						"thinking": block.Thinking,
					},
				})
			}
			write("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
		case "server_tool_use":
			startBlock := block
			startBlock.Input = json.RawMessage(`{}`)
			write("content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         i,
				"content_block": startBlock,
			})
			input := "{}"
			if len(block.Input) > 0 {
				input = string(block.Input)
			}
			write("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": i,
				"delta": map[string]string{
					"type":         "input_json_delta",
					"partial_json": input,
				},
			})
			write("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
		default:
			write("content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         i,
				"content_block": block,
			})
			write("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
		}
	}

	stopReason := "end_turn"
	if resp.StopReason != nil && *resp.StopReason != "" {
		stopReason = *resp.StopReason
	}
	write("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]int{"output_tokens": resp.Usage.OutputTokens},
	})
	write("message_stop", map[string]string{"type": "message_stop"})

	s.logSuccessWithCache(r.Context(), r, inModel, targetModel, true, http.StatusOK,
		resp.Usage.InputTokens, resp.Usage.OutputTokens,
		resp.Usage.CacheReadInputTokens, resp.Usage.CacheCreationInputTokens,
		stopReason, string(reqBody), responseLog.String(), time.Since(start))
}

func shouldUseWebSearchShim(areq *proxy.AnthropicRequest) bool {
	if areq == nil {
		return false
	}
	for _, tool := range areq.Tools {
		if proxy.IsAnthropicWebSearchTool(tool) {
			return true
		}
	}
	return false
}

func cloneOpenAIRequest(req *proxy.OpenAIRequest) proxy.OpenAIRequest {
	out := *req
	out.Messages = append([]proxy.OpenAIMessage(nil), req.Messages...)
	out.Tools = append([]proxy.OpenAITool(nil), req.Tools...)
	return out
}

func (s *Server) doOpenAIChat(
	ctx context.Context,
	upstream, zenKey string,
	req *proxy.OpenAIRequest,
	stream bool,
	timeoutSeconds int,
) (*proxy.OpenAIResponse, error) {
	upBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("could not encode upstream request: %w", err)
	}
	upURL := strings.TrimRight(upstream, "/") + "/v1/chat/completions"
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upURL, bytes.NewReader(upBody))
	if err != nil {
		return nil, fmt.Errorf("could not build upstream request: %w", err)
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Authorization", "Bearer "+zenKey)
	upReq.Header.Set("Accept", "application/json")
	upReq.Header.Set("User-Agent", "opencode-cc/1.3")

	resp, err := s.upstreamClient(stream, timeoutSeconds).Do(upReq)
	if err != nil {
		return nil, fmt.Errorf("upstream request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("could not read upstream body: %w", err)
	}
	if len(raw) > maxResponseBytes {
		return nil, fmt.Errorf("upstream response exceeded the maximum allowed size")
	}
	if resp.StatusCode >= http.StatusBadRequest {
		if shouldFailover(resp.StatusCode) {
			s.cfg.MarkUpstreamFailed()
		}
		if msg := extractOpenAIError(raw); msg != "" {
			return nil, fmt.Errorf("upstream returned status %d: %s", resp.StatusCode, msg)
		}
		return nil, fmt.Errorf("upstream returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	out, err := proxy.ParseOpenAIResponse(raw)
	if err != nil {
		return nil, fmt.Errorf("could not parse upstream response: %w", err)
	}
	return out, nil
}

func extractWebSearchCall(resp *proxy.OpenAIResponse) (webSearchCall, bool) {
	if resp == nil || len(resp.Choices) == 0 || resp.Choices[0].Message == nil {
		return webSearchCall{}, false
	}
	msg := resp.Choices[0].Message
	for _, call := range msg.ToolCalls {
		if strings.EqualFold(call.Function.Name, "web_search") {
			return parseWebSearchCall(call)
		}
	}
	if msg.FunctionCall != nil && strings.EqualFold(msg.FunctionCall.Name, "web_search") {
		return parseWebSearchCall(proxy.OpenAIToolCall{
			Type:     "function",
			Function: *msg.FunctionCall,
		})
	}
	return webSearchCall{}, false
}

func parseWebSearchCall(call proxy.OpenAIToolCall) (webSearchCall, bool) {
	if call.ID == "" {
		call.ID = "call_" + proxyRandHex(24)
	}
	out := webSearchCall{ToolCall: call}
	var args map[string]any
	if json.Unmarshal([]byte(call.Function.Arguments), &args) == nil {
		if query, _ := args["query"].(string); strings.TrimSpace(query) != "" {
			out.Query = strings.TrimSpace(query)
		}
		out.AllowedDomains = stringList(args["allowed_domains"])
		out.BlockedDomains = stringList(args["blocked_domains"])
	}
	return out, true
}

func stringList(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
			out = append(out, strings.TrimSpace(text))
		}
	}
	return out
}

func requiresWebSearch(areq *proxy.AnthropicRequest) bool {
	if areq == nil {
		return false
	}
	switch strings.ToLower(areq.ToolChoice.Type) {
	case "tool":
		return strings.EqualFold(areq.ToolChoice.Name, "web_search")
	case "any":
		webSearchTools := 0
		for _, tool := range areq.Tools {
			if proxy.IsAnthropicWebSearchTool(tool) {
				webSearchTools++
			}
		}
		return webSearchTools == len(areq.Tools) && webSearchTools > 0
	default:
		return false
	}
}

func fallbackWebSearchCall(areq *proxy.AnthropicRequest) (webSearchCall, bool) {
	query := fallbackWebSearchQuery(lastAnthropicUserText(areq))
	if query == "" {
		return webSearchCall{}, false
	}
	args, _ := json.Marshal(map[string]string{"query": query})
	call := proxy.OpenAIToolCall{
		ID:   "call_" + proxyRandHex(24),
		Type: "function",
		Function: proxy.OpenAIFunctionCall{
			Name:      "web_search",
			Arguments: string(args),
		},
	}
	return parseWebSearchCall(call)
}

func fallbackWebSearchQuery(text string) string {
	text = strings.TrimSpace(systemReminderRe.ReplaceAllString(text, " "))
	for _, prefix := range []string{
		"perform a web search for the query:",
		"perform a web search for:",
		"web search query:",
		"search query:",
		"search for:",
	} {
		if len(text) >= len(prefix) && strings.EqualFold(text[:len(prefix)], prefix) {
			text = strings.TrimSpace(text[len(prefix):])
			break
		}
	}
	text = strings.Join(strings.Fields(text), " ")
	if len([]rune(text)) > 240 {
		text = string([]rune(text)[:240])
	}
	return strings.TrimSpace(text)
}

func buildWebSearchAnswerRequest(
	base *proxy.OpenAIRequest,
	call webSearchCall,
	results []proxy.AnthropicWebSearchResult,
	areq *proxy.AnthropicRequest,
	searchModel string,
	cfg *config.Config,
) *proxy.OpenAIRequest {
	answerReq := cloneOpenAIRequest(base)
	answerReq.Stream = false
	answerReq.StreamOptions = nil
	answerReq.Tools = nil
	answerReq.ToolChoice = nil
	answerReq.ParallelToolCalls = nil
	prepareWebSearchModelRequest(&answerReq, areq, searchModel, cfg, "answer")
	answerReq.Messages = append(answerReq.Messages, proxy.OpenAIMessage{
		Role: "user",
		Content: "Use the following web search results to answer the user's request. " +
			"Keep the answer concise and include URLs when they are useful.\n\n" +
			webSearchResultsText(call.Query, results),
	})
	return &answerReq
}

func prepareWebSearchModelRequest(
	req *proxy.OpenAIRequest,
	areq *proxy.AnthropicRequest,
	searchModel string,
	cfg *config.Config,
	stage string,
) {
	if searchModel != "" {
		req.Model = searchModel
	}
	req.ReasoningEffort = ""
	req.ThinkingBudget = nil
	req.Thinking = nil
	if stage == "query" && req.ToolChoice == "auto" {
		req.ToolChoice = nil
	}
	req.PromptCacheKey = webSearchPromptCacheKey(req.PromptCacheKey, req.Model, stage)
}

func webSearchPromptCacheKey(base, model, stage string) string {
	if base == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(model + "\x00" + stage))
	return base + ":web:" + hex.EncodeToString(sum[:])[:8]
}

func webSearchServerBlocks(
	serverToolID string,
	call webSearchCall,
	results []proxy.AnthropicWebSearchResult,
	searchErr error,
) []proxy.AnthropicContent {
	args, _ := json.Marshal(map[string]string{"query": call.Query})
	out := []proxy.AnthropicContent{{
		Type:  "server_tool_use",
		ID:    serverToolID,
		Name:  "web_search",
		Input: args,
	}}
	resultBlock := proxy.AnthropicContent{
		Type:      "web_search_tool_result",
		ToolUseID: serverToolID,
	}
	if searchErr != nil || len(results) == 0 {
		resultBlock.WebSearchError = &proxy.AnthropicWebSearchError{
			Type:      "web_search_tool_result_error",
			ErrorCode: "unavailable",
		}
	} else {
		resultBlock.WebSearchResults = results
	}
	return append(out, resultBlock)
}

func finalTextLikeContent(blocks []proxy.AnthropicContent) []proxy.AnthropicContent {
	out := make([]proxy.AnthropicContent, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case "text", "thinking":
			out = append(out, block)
		}
	}
	return out
}

func fallbackSearchSummary(results []proxy.AnthropicWebSearchResult) proxy.AnthropicContent {
	return proxy.AnthropicContent{
		Type: "text",
		Text: webSearchResultsText("", results),
	}
}

func webSearchResultsText(query string, results []proxy.AnthropicWebSearchResult) string {
	var b strings.Builder
	if query != "" {
		fmt.Fprintf(&b, "Web search results for %q:\n", query)
	} else {
		b.WriteString("Web search results:\n")
	}
	for i, result := range results {
		fmt.Fprintf(&b, "%d. %s\nURL: %s\n", i+1, result.Title, result.URL)
		if result.EncryptedContent != "" {
			fmt.Fprintf(&b, "Snippet: %s\n", result.EncryptedContent)
		}
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

func lastAnthropicUserText(areq *proxy.AnthropicRequest) string {
	for i := len(areq.Messages) - 1; i >= 0; i-- {
		msg := areq.Messages[i]
		if msg.Role != "user" {
			continue
		}
		if msg.Content.IsStr {
			return msg.Content.Text
		}
		var b strings.Builder
		for _, block := range msg.Content.Blocks {
			if block.Type == "text" && block.Text != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(block.Text)
			}
		}
		return b.String()
	}
	return ""
}

func duckDuckGoSearch(
	ctx context.Context,
	query string,
	allowedDomains, blockedDomains []string,
	limit int,
) ([]proxy.AnthropicWebSearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("empty search query")
	}
	reqURL := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; opencode-cc-web-search/1.0)")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("search provider returned status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	results := parseDuckDuckGoHTML(string(raw), allowedDomains, blockedDomains, limit)
	if len(results) == 0 {
		return nil, fmt.Errorf("search provider returned no results")
	}
	return results, nil
}

var webSearchProvider = duckDuckGoSearch

var (
	systemReminderRe = regexp.MustCompile(`(?is)<system-reminder>.*?</system-reminder>`)
	hrefRe           = regexp.MustCompile(`(?is)href="([^"]+)"`)
	snippetRe        = regexp.MustCompile(`(?is)class="result__snippet"[^>]*>(.*?)</(?:a|div)>`)
	tagRe            = regexp.MustCompile(`(?is)<[^>]+>`)
)

func parseDuckDuckGoHTML(page string, allowedDomains, blockedDomains []string, limit int) []proxy.AnthropicWebSearchResult {
	if limit <= 0 {
		limit = 5
	}
	var out []proxy.AnthropicWebSearchResult
	needle := `class="result__a"`
	for start := 0; start < len(page) && len(out) < limit; {
		idx := strings.Index(page[start:], needle)
		if idx < 0 {
			break
		}
		idx += start
		next := strings.Index(page[idx+len(needle):], needle)
		end := len(page)
		if next >= 0 {
			end = idx + len(needle) + next
		}
		segment := page[idx:end]
		start = end

		openEnd := strings.Index(segment, ">")
		closeIdx := strings.Index(strings.ToLower(segment), "</a>")
		if openEnd < 0 || closeIdx < 0 || closeIdx <= openEnd {
			continue
		}
		hrefMatch := hrefRe.FindStringSubmatch(segment[:openEnd])
		if len(hrefMatch) != 2 {
			continue
		}
		resultURL := normalizeDuckDuckGoURL(hrefMatch[1])
		if resultURL == "" || !domainAllowed(resultURL, allowedDomains, blockedDomains) {
			continue
		}
		title := cleanHTMLText(segment[openEnd+1 : closeIdx])
		if title == "" {
			continue
		}
		snippet := ""
		if match := snippetRe.FindStringSubmatch(segment); len(match) == 2 {
			snippet = cleanHTMLText(match[1])
		}
		out = append(out, proxy.AnthropicWebSearchResult{
			Type:             "web_search_result",
			Title:            title,
			URL:              resultURL,
			URI:              resultURL,
			EncryptedContent: snippet,
		})
	}
	return out
}

func normalizeDuckDuckGoURL(raw string) string {
	raw = html.UnescapeString(raw)
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if strings.Contains(parsed.Host, "duckduckgo.com") {
		if target := parsed.Query().Get("uddg"); target != "" {
			if decoded, err := url.QueryUnescape(target); err == nil {
				return decoded
			}
			return target
		}
		return ""
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	return parsed.String()
}

func cleanHTMLText(text string) string {
	text = tagRe.ReplaceAllString(text, " ")
	text = html.UnescapeString(text)
	return strings.Join(strings.Fields(text), " ")
}

func domainAllowed(raw string, allowedDomains, blockedDomains []string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return false
	}
	for _, blocked := range blockedDomains {
		if domainMatches(host, blocked) {
			return false
		}
	}
	if len(allowedDomains) == 0 {
		return true
	}
	for _, allowed := range allowedDomains {
		if domainMatches(host, allowed) {
			return true
		}
	}
	return false
}

func domainMatches(host, domain string) bool {
	domain = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(domain)), ".")
	return host == domain || strings.HasSuffix(host, "."+domain)
}

func proxyRandHex(n int) string {
	if n <= 0 {
		return ""
	}
	buf := make([]byte, (n+1)/2)
	if _, err := rand.Read(buf); err != nil {
		for i := range buf {
			buf[i] = byte(i * 17)
		}
	}
	return hex.EncodeToString(buf)[:n]
}
