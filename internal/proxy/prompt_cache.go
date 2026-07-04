package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// PromptCacheOptions controls request shaping that improves provider-side
// prompt-cache hit rates. It intentionally avoids changing user-visible text.
type PromptCacheOptions struct {
	Enabled               bool
	KeyPrefix             string
	AnthropicCacheControl bool
	NormalizePrompts      bool
}

const (
	promptCacheFirstMessagePrefixBytes = 8192
	promptCacheRetention               = "24h"
)

// ApplyOpenAIPromptCache normalizes an OpenAI Chat request and sets a stable
// prompt_cache_key when the caller did not provide one.
func ApplyOpenAIPromptCache(req *OpenAIRequest, opts PromptCacheOptions) {
	if req == nil || !opts.Enabled {
		return
	}
	if opts.NormalizePrompts {
		sortOpenAITools(req.Tools)
		req.Messages = normalizeOpenAIMessagePrefix(req.Messages)
	}
	if req.PromptCacheKey == "" {
		req.PromptCacheKey = buildOpenAIPromptCacheKey(req, opts.KeyPrefix)
	}
	if req.PromptCacheRetention == "" {
		req.PromptCacheRetention = promptCacheRetention
	}
}

// AnthropicPromptCacheHint returns the stable cache/session hint derived from
// Anthropic metadata or cache_control boundaries.
func AnthropicPromptCacheHint(in *AnthropicRequest) string {
	if hint := anthropicPromptCacheHint(in); hint != "" {
		return hint
	}
	return anthropicCacheControlPromptCacheHint(in)
}

// ApplyRawOpenAIPromptCache normalizes a raw OpenAI-compatible request object
// while preserving unsupported extension fields.
func ApplyRawOpenAIPromptCache(payload map[string]json.RawMessage, opts PromptCacheOptions) {
	if payload == nil || !opts.Enabled {
		return
	}
	if opts.NormalizePrompts {
		normalizeRawTools(payload)
		normalizeRawOpenAIMessages(payload)
		removeVolatileMetadata(payload)
	}
	if raw := payload["prompt_cache_key"]; len(raw) == 0 || string(raw) == "null" {
		payload["prompt_cache_key"], _ = json.Marshal(buildRawPromptCacheKey(payload, opts.KeyPrefix))
	}
	if raw := payload["prompt_cache_retention"]; len(raw) == 0 || string(raw) == "null" {
		payload["prompt_cache_retention"], _ = json.Marshal(promptCacheRetention)
	}
}

// PrepareAnthropicPromptCacheBody rewrites the target model and applies
// Anthropic cache_control markers to stable prefix blocks when appropriate.
func PrepareAnthropicPromptCacheBody(body []byte, targetModel string, opts PromptCacheOptions) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("request body is not valid Anthropic JSON: %w", err)
	}
	if payload == nil {
		return nil, fmt.Errorf("request body must be a JSON object")
	}
	payload["model"], _ = json.Marshal(targetModel)
	if opts.Enabled {
		if opts.NormalizePrompts {
			normalizeRawTools(payload)
			normalizeRawAnthropicSystem(payload, opts.AnthropicCacheControl)
			normalizeRawAnthropicMessages(payload)
			removeVolatileMetadata(payload)
		} else if opts.AnthropicCacheControl {
			addAnthropicCacheControl(payload)
		}
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("could not encode upstream request: %w", err)
	}
	return out, nil
}

func normalizeOpenAIMessagePrefix(messages []OpenAIMessage) []OpenAIMessage {
	if len(messages) < 2 {
		return messages
	}
	system := make([]OpenAIMessage, 0)
	rest := make([]OpenAIMessage, 0, len(messages))
	for _, msg := range messages {
		if msg.Role == "system" || msg.Role == "developer" {
			system = append(system, msg)
			continue
		}
		rest = append(rest, msg)
	}
	if len(system) == 0 {
		return messages
	}
	return append(system, rest...)
}

func buildOpenAIPromptCacheKey(req *OpenAIRequest, prefix string) string {
	parts := []any{
		req.Model,
	}
	if req.PromptCacheHint != "" {
		parts = append(parts, "hint", req.PromptCacheHint)
	}
	parts = append(parts, req.Tools)
	for _, msg := range req.Messages {
		if msg.Role != "system" && msg.Role != "developer" {
			break
		}
		parts = append(parts, msg.Role, msg.Content)
	}
	if req.PromptCacheHint == "" {
		if first := firstOpenAIMessagePrefixForCache(req.Messages); first != "" {
			parts = append(parts, "first_message_prefix", first)
		}
	}
	return promptCacheKey(prefix, parts)
}

func firstOpenAIMessagePrefixForCache(messages []OpenAIMessage) string {
	for _, msg := range messages {
		if msg.Role == "system" || msg.Role == "developer" {
			continue
		}
		prefix := contentPrefixForCache(msg.Content)
		if prefix == "" {
			return ""
		}
		return msg.Role + ":" + prefix
	}
	return ""
}

func contentPrefixForCache(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return truncateCachePrefix(v)
	case []OpenAIContentPart:
		raw, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return truncateCachePrefix(string(raw))
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return truncateCachePrefix(fmt.Sprint(v))
		}
		return truncateCachePrefix(string(raw))
	}
}

func truncateCachePrefix(text string) string {
	text = strings.TrimSpace(text)
	if len(text) <= promptCacheFirstMessagePrefixBytes {
		return text
	}
	for idx := range text {
		if idx >= promptCacheFirstMessagePrefixBytes {
			return text[:idx]
		}
	}
	return text
}

func anthropicPromptCacheHint(in *AnthropicRequest) string {
	if in == nil || len(in.Metadata) == 0 {
		return ""
	}
	var metadata map[string]json.RawMessage
	if json.Unmarshal(in.Metadata, &metadata) != nil {
		return ""
	}
	for _, key := range []string{"session_id", "sessionId", "conversation_id", "conversationId", "thread_id", "threadId"} {
		if hint := promptCacheHintFromRaw(metadata[key]); hint != "" {
			return hint
		}
	}
	if hint := promptCacheHintFromRaw(metadata["user_id"]); hint != "" {
		return hint
	}
	return ""
}

func promptCacheHintFromRaw(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return promptCacheHintFromString(text)
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) == nil {
		return promptCacheHintFromObject(object)
	}
	return ""
}

func promptCacheHintFromString(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if strings.HasPrefix(text, "{") {
		var object map[string]json.RawMessage
		if json.Unmarshal([]byte(text), &object) == nil {
			if hint := promptCacheHintFromObject(object); hint != "" {
				return hint
			}
		}
	}
	return text
}

func promptCacheHintFromObject(object map[string]json.RawMessage) string {
	for _, key := range []string{"session_id", "sessionId", "conversation_id", "conversationId", "thread_id", "threadId"} {
		var text string
		if json.Unmarshal(object[key], &text) == nil {
			text = strings.TrimSpace(text)
			if text != "" {
				return text
			}
		}
	}
	return ""
}

func anthropicCacheControlPromptCacheHint(in *AnthropicRequest) string {
	if in == nil {
		return ""
	}
	prefix, ok := anthropicCacheControlPrefix(in)
	if !ok {
		return ""
	}
	raw, err := json.Marshal(prefix)
	if err != nil {
		return ""
	}
	canonical, ok := canonicalJSON(raw)
	if !ok {
		canonical = raw
	}
	sum := sha256.Sum256(canonical)
	return "cache-control:" + hex.EncodeToString(sum[:])[:20]
}

type anthropicCacheControlPrefixData struct {
	Tools    []AnthropicTool    `json:"tools,omitempty"`
	System   []AnthropicContent `json:"system,omitempty"`
	Messages []AnthropicMessage `json:"messages,omitempty"`
}

func anthropicCacheControlPrefix(in *AnthropicRequest) (anthropicCacheControlPrefixData, bool) {
	var prefix anthropicCacheControlPrefixData
	found := false

	for idx, tool := range in.Tools {
		if hasCacheControl(tool.CacheControl) {
			prefix = anthropicCacheControlPrefixData{
				Tools: append([]AnthropicTool(nil), in.Tools[:idx+1]...),
			}
			found = true
		}
	}

	for idx, block := range in.System.Blocks {
		if hasCacheControl(block.CacheControl) {
			prefix = anthropicCacheControlPrefixData{
				Tools:  append([]AnthropicTool(nil), in.Tools...),
				System: append([]AnthropicContent(nil), in.System.Blocks[:idx+1]...),
			}
			found = true
		}
	}

	for msgIdx, msg := range in.Messages {
		if msg.Content.IsStr {
			continue
		}
		for blockIdx, block := range msg.Content.Blocks {
			if !hasCacheControl(block.CacheControl) {
				continue
			}
			messages := append([]AnthropicMessage(nil), in.Messages[:msgIdx]...)
			truncated := msg
			truncated.Content = AnthropicMessageContent{
				Blocks: append([]AnthropicContent(nil), msg.Content.Blocks[:blockIdx+1]...),
			}
			messages = append(messages, truncated)
			prefix = anthropicCacheControlPrefixData{
				Tools:    append([]AnthropicTool(nil), in.Tools...),
				System:   append([]AnthropicContent(nil), in.System.Blocks...),
				Messages: messages,
			}
			found = true
		}
	}

	return prefix, found
}

func hasCacheControl(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	text := strings.TrimSpace(string(raw))
	return text != "" && text != "null"
}

func buildRawPromptCacheKey(payload map[string]json.RawMessage, prefix string) string {
	parts := make([]any, 0, 5)
	for _, key := range []string{"model", "tools", "response_format"} {
		if raw := payload[key]; len(raw) > 0 {
			parts = append(parts, key, string(raw))
		}
	}
	if raw := payload["messages"]; len(raw) > 0 {
		var messages []map[string]json.RawMessage
		if json.Unmarshal(raw, &messages) == nil {
			for _, msg := range messages {
				role := rawString(msg["role"])
				if role != "system" && role != "developer" {
					break
				}
				parts = append(parts, role, string(msg["content"]))
			}
			if first := firstRawOpenAIMessagePrefixForCache(messages); first != "" {
				parts = append(parts, "first_message_prefix", first)
			}
		}
	}
	return promptCacheKey(prefix, parts)
}

func firstRawOpenAIMessagePrefixForCache(messages []map[string]json.RawMessage) string {
	for _, msg := range messages {
		role := rawString(msg["role"])
		if role == "system" || role == "developer" {
			continue
		}
		prefix := rawContentPrefixForCache(msg["content"])
		if prefix == "" {
			return ""
		}
		return role + ":" + prefix
	}
	return ""
}

func rawContentPrefixForCache(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return truncateCachePrefix(text)
	}
	return truncateCachePrefix(string(raw))
}

func promptCacheKey(prefix string, parts []any) string {
	prefix = sanitizeCacheKeyPrefix(prefix)
	if prefix == "" {
		prefix = "opencode-cc"
	}
	body, _ := json.Marshal(parts)
	sum := sha256.Sum256(body)
	return prefix + ":" + hex.EncodeToString(sum[:])[:20]
}

func sanitizeCacheKeyPrefix(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	var out strings.Builder
	for _, r := range prefix {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			out.WriteRune(r)
		case r == '-', r == '_', r == ':', r == '.':
			out.WriteRune(r)
		}
		if out.Len() >= 48 {
			break
		}
	}
	return out.String()
}

func normalizeRawOpenAIMessages(payload map[string]json.RawMessage) {
	raw := payload["messages"]
	if len(raw) == 0 {
		return
	}
	var messages []map[string]json.RawMessage
	if json.Unmarshal(raw, &messages) != nil || len(messages) == 0 {
		return
	}
	system := make([]map[string]json.RawMessage, 0)
	rest := make([]map[string]json.RawMessage, 0, len(messages))
	for _, msg := range messages {
		normalizeRawContentField(msg)
		role := rawString(msg["role"])
		if role == "system" || role == "developer" {
			system = append(system, msg)
			continue
		}
		rest = append(rest, msg)
	}
	messages = append(system, rest...)
	if out, err := json.Marshal(messages); err == nil {
		payload["messages"] = out
	}
}

func normalizeRawAnthropicSystem(payload map[string]json.RawMessage, addCacheControl bool) {
	raw := payload["system"]
	if len(raw) == 0 || string(raw) == "null" {
		if addCacheControl {
			addAnthropicCacheControl(payload)
		}
		return
	}
	var systemText string
	if json.Unmarshal(raw, &systemText) == nil {
		if addCacheControl && !rawPayloadHasCacheControl(payload) {
			payload["system"], _ = json.Marshal([]map[string]any{{
				"type":          "text",
				"text":          systemText,
				"cache_control": map[string]string{"type": "ephemeral"},
			}})
		}
		return
	}
	payload["system"] = normalizeContentBlocks(raw)
	if addCacheControl {
		addAnthropicCacheControl(payload)
	}
}

func normalizeRawAnthropicMessages(payload map[string]json.RawMessage) {
	raw := payload["messages"]
	if len(raw) == 0 {
		return
	}
	var messages []map[string]json.RawMessage
	if json.Unmarshal(raw, &messages) != nil {
		return
	}
	for _, msg := range messages {
		normalizeRawContentField(msg)
	}
	if out, err := json.Marshal(messages); err == nil {
		payload["messages"] = out
	}
}

func normalizeRawContentField(obj map[string]json.RawMessage) {
	raw := obj["content"]
	if len(raw) == 0 {
		return
	}
	obj["content"] = normalizeContentBlocks(raw)
}

func normalizeContentBlocks(raw json.RawMessage) json.RawMessage {
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil || len(blocks) < 2 {
		return raw
	}
	for start := 0; start < len(blocks); {
		if !isContextBlock(blocks[start]) {
			start++
			continue
		}
		end := start + 1
		for end < len(blocks) && isContextBlock(blocks[end]) {
			end++
		}
		sort.SliceStable(blocks[start:end], func(i, j int) bool {
			return contextBlockKey(blocks[start+i]) < contextBlockKey(blocks[start+j])
		})
		start = end
	}
	out, err := json.Marshal(blocks)
	if err != nil {
		return raw
	}
	return out
}

func normalizeRawTools(payload map[string]json.RawMessage) {
	raw := payload["tools"]
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	var tools []json.RawMessage
	if json.Unmarshal(raw, &tools) != nil || len(tools) < 2 {
		return
	}
	sort.SliceStable(tools, func(i, j int) bool {
		return rawToolSortKey(tools[i]) < rawToolSortKey(tools[j])
	})
	if out, err := json.Marshal(tools); err == nil {
		payload["tools"] = out
	}
}

func rawToolSortKey(raw json.RawMessage) string {
	var tool struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &tool) != nil {
		return ""
	}
	if tool.Function.Name != "" {
		return "function:" + tool.Function.Name
	}
	if tool.Name != "" {
		return tool.Type + ":" + tool.Name
	}
	return tool.Type
}

func addAnthropicCacheControl(payload map[string]json.RawMessage) {
	if rawPayloadHasCacheControl(payload) {
		return
	}
	if addCacheControlToArrayField(payload, "system") {
		return
	}
	_ = addCacheControlToArrayField(payload, "tools")
}

func addCacheControlToArrayField(payload map[string]json.RawMessage, field string) bool {
	raw := payload[field]
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(raw, &items) != nil || len(items) == 0 {
		return false
	}
	for i := len(items) - 1; i >= 0; i-- {
		if len(items[i]) == 0 {
			continue
		}
		items[i]["cache_control"], _ = json.Marshal(map[string]string{"type": "ephemeral"})
		if out, err := json.Marshal(items); err == nil {
			payload[field] = out
			return true
		}
	}
	return false
}

func rawPayloadHasCacheControl(payload map[string]json.RawMessage) bool {
	for _, raw := range payload {
		if strings.Contains(string(raw), `"cache_control"`) {
			return true
		}
	}
	return false
}

func isContextBlock(raw json.RawMessage) bool {
	var block map[string]json.RawMessage
	if json.Unmarshal(raw, &block) != nil {
		return false
	}
	typ := rawString(block["type"])
	switch typ {
	case "document", "input_document", "file", "input_file", "file_search_result", "search_result", "context", "attachment":
		return true
	case "text", "input_text", "output_text", "image", "input_image", "image_url", "tool_use", "tool_result":
		return false
	}
	for _, key := range []string{"file_id", "filename", "file_name", "path", "url", "uri"} {
		if _, ok := block[key]; ok {
			return true
		}
	}
	return false
}

func contextBlockKey(raw json.RawMessage) string {
	var block map[string]json.RawMessage
	if json.Unmarshal(raw, &block) != nil {
		return string(raw)
	}
	typ := rawString(block["type"])
	for _, key := range []string{"path", "filename", "file_name", "file_id", "url", "uri", "title", "name", "id"} {
		if value := rawString(block[key]); value != "" {
			return typ + ":" + key + ":" + value
		}
	}
	if sourceRaw := block["source"]; len(sourceRaw) > 0 {
		var source map[string]json.RawMessage
		if json.Unmarshal(sourceRaw, &source) == nil {
			for _, key := range []string{"path", "filename", "file_name", "file_id", "url", "uri", "title", "name", "id"} {
				if value := rawString(source[key]); value != "" {
					return typ + ":source." + key + ":" + value
				}
			}
		}
	}
	sum := sha256.Sum256(raw)
	return typ + ":hash:" + hex.EncodeToString(sum[:])[:16]
}

func removeVolatileMetadata(payload map[string]json.RawMessage) {
	for _, key := range []string{"request_id", "requestId", "trace_id", "span_id", "timestamp", "created_at", "updated_at", "nonce"} {
		delete(payload, key)
	}
	raw := payload["metadata"]
	if len(raw) == 0 {
		return
	}
	var metadata map[string]json.RawMessage
	if json.Unmarshal(raw, &metadata) != nil {
		return
	}
	for _, key := range []string{"request_id", "requestId", "trace_id", "span_id", "timestamp", "created_at", "updated_at", "nonce"} {
		delete(metadata, key)
	}
	if len(metadata) == 0 {
		delete(payload, "metadata")
		return
	}
	if out, err := json.Marshal(metadata); err == nil {
		payload["metadata"] = out
	}
}

func rawString(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	return ""
}
