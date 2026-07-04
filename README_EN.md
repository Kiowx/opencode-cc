# opencode-cc

[简体中文](README.md) | [English](README_EN.md)

> An Anthropic/OpenAI-to-OpenCode Zen multi-protocol bridge proxy with an embedded web dashboard.

`opencode-cc` is a high-performance Go proxy that exposes both the **Anthropic Messages API** and the
**OpenAI Chat Completions API**, with streaming and non-streaming support, and forwards requests to the
**[OpenCode Zen](https://opencode.ai/docs/zen/)** gateway. Zen provides 49 models across four protocols. The proxy
automatically selects the correct protocol translator based on the target model ID, allowing Claude Code to run
transparently with GLM, Kimi, DeepSeek, Qwen, Claude, GPT, Gemini, and other models.

Community: [linuxdo](https://linux.do/)

```text
┌────────────┐  POST /v1/messages   ┌──────────────┐  Route by model ↓      ┌──────────┐
│ Claude Code│ ───────────────────> │ opencode-cc  │ ─────────────────────> │ OpenCode │
│            │ <── Anthropic SSE ── │     (Go)     │ <── Protocol SSE ───── │   Zen    │
└────────────┘                      └──────────────┘                         └──────────┘
                                      Embedded React dashboard ▲ /api/*
                                           SQLite (pure Go, no CGO)
```

<img width="2540" height="1193" alt="QQ_1781519790308" src="https://github.com/user-attachments/assets/2854f45f-62a0-463a-9b22-a07a770670b4" />

## Automatic Routing Across Four Protocols

Zen is not a single OpenAI-compatible endpoint. Its models are exposed through four protocol paths. The proxy selects
the correct path automatically based on the model ID prefix:

| Protocol | Path | Models | Translation |
|----------|------|--------|-------------|
| **OpenAI** | `/v1/chat/completions` | GLM, Kimi, DeepSeek, MiniMax, MiMo, Grok, and free models | Bidirectional Anthropic ↔ OpenAI translation |
| **Anthropic** | `/v1/messages` | Claude and Qwen | Near-transparent passthrough with model ID rewriting only |
| **Responses** | `/v1/responses` | The GPT model family | Anthropic ↔ OpenAI Responses API |
| **Google** | `/v1beta/models/{id}` | Gemini | Anthropic ↔ Google Generative Language |

Each protocol implements the `TranslateRequest`, `TranslateResponse`, and `TranslateStream` methods of the
`upstream.Protocol` interface.

## Features

- **Complete tool-call support.** Anthropic `tool_use` and `tool_result` blocks are translated bidirectionally into
  OpenAI `tool_calls` and `role:"tool"` messages. Claude Code tool definitions are converted into OpenAI function
  tools. The `reasoning_content` extension used by several models is translated into Anthropic `thinking` blocks.
- **Automatic routing across four protocols.** Model IDs beginning with `claude-` or `qwen` use Anthropic,
  `gpt-` uses Responses, `gemini-` uses Google, and all other models use OpenAI. No manual protocol configuration is
  required.
- **OpenAI client compatibility.** Supports `POST /v1/chat/completions` and an OpenAI-compatible `/v1/models`
  response, allowing OpenAI SDKs and compatible desktop clients to connect directly. The OpenAI path passes through
  Zen's native `/go/v1/chat/completions` JSON/SSE response without converting it to Anthropic format.
- **Codex CLI compatibility.** Exposes `POST /v1/responses` and translates Codex Responses API requests, streaming
  text events, and function calls to Zen. OpenAI-compatible target models use `/go/v1/chat/completions`, while
  Claude/Qwen target models can use the native `/v1/messages` upstream path.
- **Smart native Anthropic routing.** `native_anthropic` is enabled by default. Only `claude-*` / `qwen*` target
  models are forwarded directly to upstream `/v1/messages`, preserving `cache_control`, native Anthropic SSE, and
  extension fields. GLM, DeepSeek, Kimi, and other target models continue using translation mode.
- **A built-in catalog of 49 models.** The Models page displays pricing, context limits, capability tags, and protocol
  badges. Models can be added to mappings directly from the Config page.
- **A single static binary.** The React SPA is embedded with `embed.FS`, so Node.js is not required at runtime.
  SQLite uses a pure-Go driver with no CGO dependency, making clean cross-compilation possible.
- **Web dashboard.** Includes Dashboard for traffic and health data, Models for browsing and filtering, Inspector for
  live request details and protocol routing, and Config for Zen settings, proxy authentication, and hot-reloaded model
  mappings.
- **Panel password protection.** When `panel_token` is set in `config.json` or via the Settings page, the web
  dashboard requires password authentication before granting access. After a successful login an HttpOnly session cookie
  (24-hour TTL) is issued to maintain the session. A logout button is available in the sidebar. When `panel_token` is
  empty, the panel remains open — suitable for local, single-user deployments.
- **Client API key management.** Create, disable, and delete client keys from the dashboard, with per-key total token
  quotas, daily token limits, allowed IPs, and automatic usage tracking.
- **Secure defaults.** Constant-time Bearer token authentication, request body size limits, per-request panic recovery,
  and graceful shutdown.

## Quick Start

### Prerequisites

- Go 1.22+
- Node.js 20+ (only required when building the UI)
- An OpenCode Zen API key. Sign in at [opencode.ai/auth](https://opencode.ai/auth), add billing information, and copy
  your API key.

### Build and Run

```bash
make            # Build the frontend and Go binary as ./opencode-cc
./opencode-cc   # Start on :8787 and create config.json and data/opencode-cc.db
```

Open `http://localhost:8787/` and go to the **Config** tab:

1. Enter your **API key** in the "Upstream (OpenCode Zen)" card.
2. Select "Test connection" to verify connectivity.
3. Adjust the Claude Code-to-Zen model mappings under "Model mappings". Several cost-effective defaults are included,
   such as `claude-sonnet-4-5` → `glm-5.1`.

Then point Claude Code at the proxy:

```bash
export ANTHROPIC_BASE_URL=http://localhost:8787
export ANTHROPIC_AUTH_TOKEN=local    # Any value works when proxy auth is disabled
claude
```

If your upstream base URL supports Anthropic Messages, for example `<base>/v1/messages`,
**smart native Anthropic routing** is used by default. You can also set it explicitly:

```bash
export OPENCODE_CC_UPSTREAM=https://opencode.ai/zen/go
export OPENCODE_CC_NATIVE_ANTHROPIC=true
```

When enabled, the proxy first resolves the target model from the mapping table. `claude-*` / `qwen*` target models are
sent directly to upstream `/v1/messages` with only the `model` field rewritten; other target models are still
translated to OpenAI Chat Completions. In the example above, Claude/Qwen target models are forwarded to
`https://opencode.ai/zen/go/v1/messages`. The native path sends both `Authorization: Bearer <key>` and
`x-api-key: <key>` to support OpenAI-style and Anthropic-style authentication.

For the OpenAI SDK or compatible clients, set the base URL to `http://localhost:8787/v1`:

> Select **OpenAI** as the client's API type. A request path of `/v1/messages` means the client is still using
> Anthropic mode, where responses are translated to the Anthropic Messages protocol instead of passed through natively.

```bash
export OPENAI_BASE_URL=http://localhost:8787/v1
export OPENAI_API_KEY=local          # Use a key created in the panel when client authentication is enabled
```

```bash
curl http://localhost:8787/v1/chat/completions \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"glm-5.1","messages":[{"role":"user","content":"Hello"}]}'
```

### Connect Codex CLI

Codex custom provider settings must be placed in the user-level configuration file:

- Linux/macOS: `~/.codex/config.toml`
- Windows: `%USERPROFILE%\.codex\config.toml`

```toml
model = "kimi-k2.7-code"
model_provider = "opencode_cc"

[model_providers.opencode_cc]
name = "opencode-cc"
base_url = "http://localhost:8787/v1"
env_key = "OPENCODE_CC_API_KEY"
wire_api = "responses"
```

Set the client key before starting Codex. Any non-empty value works when client API key authentication is disabled;
otherwise use a key created in the dashboard:

```bash
export OPENCODE_CC_API_KEY=local
codex
```

PowerShell:

```powershell
$env:OPENCODE_CC_API_KEY = "local"
codex
```

`model` may be a real Zen model ID or an alias configured in the dashboard. `kimi-k2.7-code` is the recommended
coding-oriented model. If the mapping resolves to `claude-*` / `qwen*`, Codex `/v1/responses` requests are converted
to native Anthropic Messages upstream requests; all other target models continue to use OpenAI Chat Completions
translation.

### Development Mode with HMR

```bash
make dev   # Vite on :5174 and Go on :8787, with API requests proxied by Vite
```

### Docker

Images are published to GitHub Container Registry as `ghcr.io/kiowx/opencode-cc`.

**One-command deployment** (no clone required; replace the API key and panel password):

```bash
docker run -d --name opencode-cc --restart unless-stopped \
  -p 8787:8787 \
  -v opencode-cc-data:/data \
  -e ZEN_API_KEY=sk-your-key \
  -e OPENCODE_CC_PANEL_TOKEN=your-panel-password \
  ghcr.io/kiowx/opencode-cc:latest
```

Open `http://your-server-ip:8787` and sign in with `OPENCODE_CC_PANEL_TOKEN`. The
`opencode-cc-data` named volume persists configuration and SQLite data. Setting a panel password
is strongly recommended whenever the port is exposed beyond localhost.

The included `docker-compose.yml` provides the same persistent setup:

```bash
docker compose up -d
```

To update to the latest image:

```bash
docker compose pull
docker compose up -d
docker image prune -f
```

For a deployment created with `docker run`, update it with:

```bash
docker pull ghcr.io/kiowx/opencode-cc:latest
docker rm -f opencode-cc
# Re-run the one-command deployment above
```

Replace `latest` with a version such as `1.3.1` to pin a release. To build locally instead:

```bash
docker build -t opencode-cc .
docker run -d --name opencode-cc -p 8787:8787 -v opencode-cc-data:/data opencode-cc
```

Every pushed `v*` tag publishes Linux amd64/arm64 images and refreshes the exact version,
major/minor, major, and `latest` tags. After the first publish, set the GHCR package visibility
to Public in GitHub Packages to allow anonymous pulls.

## How Translation Works

### Request Routing

When a `POST /v1/messages` request arrives, the proxy looks up `req.Model` in the mapping table to determine the target
Zen model ID. Unmapped model strings are passed through unchanged. It then calls `upstream.Router.For(modelID)` to
select the protocol. All four protocols share a unified output component, `anthropicEmitter`, which emits translated
content blocks as a standard Anthropic SSE event sequence.

### Anthropic ↔ OpenAI Translation

This is the most complex translation path:

| Anthropic | OpenAI Chat Completions |
|-----------|-------------------------|
| `tools[]{name, description, input_schema}` | `tools[]{type:"function", function:{name, description, parameters}}` |
| `content[]{type:"tool_use", id, name, input(object)}` | `assistant.tool_calls[]{id, function:{name, arguments(JSON string)}}` |
| `user.content[]{type:"tool_result", tool_use_id, content}` | `{role:"tool", tool_call_id, content}` |
| `content[]{type:"thinking"}` | `delta.reasoning_content` (DeepSeek/GLM/Kimi extension) |
| `stop_reason:"tool_use"` | `finish_reason:"tool_calls"` |
| Streaming `input_json_delta`, accumulated by index | Streaming `delta.tool_calls[].function.arguments`, accumulated by index |

During streaming, tool-call `arguments` fragments are accumulated by `index`. The completed arguments are then emitted
as a single `tool_use` block, ensuring that Claude Code receives a structurally complete tool call.

### Anthropic Passthrough for Claude and Qwen

Zen exposes a native Anthropic Messages API for Claude and Qwen models. The proxy only rewrites the model ID, while the
request body, response body, and SSE stream are forwarded unchanged. This is the simplest path and requires no tool-call
translation.

### Codex Responses ↔ OpenAI Chat / Anthropic Messages

For `/v1/responses`, the proxy chooses the upstream format by target model. Non-Anthropic-native models are converted
to OpenAI Chat Completions messages, while `claude-*` / `qwen*` target models are converted to Anthropic Messages
requests. Text deltas, streamed tool-call arguments, and usage from Zen are rebuilt as standard Responses events such as
`response.output_text.delta`, `response.function_call_arguments.delta`, and `response.completed`.

This compatibility layer runs locally in the proxy and does not require a native `/v1/responses` endpoint upstream.

### Anthropic ↔ Google for Gemini

`messages[]` becomes `contents[]` with `user` and `model` roles, while `tools[]` becomes
`tools[].functionDeclarations`. Gemini returns streaming JSON arrays whose chunks contain
`candidates[].content.parts[]`; the proxy parses these chunks and converts them into Anthropic increments.

## Configuration

`config.json` is created automatically on first launch:

```jsonc
{
  "listen_addr": ":8787",
  "upstream_base": "https://opencode.ai/zen",
  "native_anthropic": true,   // true smart-routes claude-* / qwen* target models to <upstream_base>/v1/messages
  "zen_api_key": "",           // Required Bearer token for the Zen gateway
  "panel_token": "",           // Web dashboard password; empty means open access
  "require_api_key": false,    // When true, /v1/* always requires a valid client API key
  "default_model": "glm-4.6",
  "model_mappings": [
    // Claude Code model string → actual Zen model ID
    { "match": "claude-sonnet-4-5", "target": "glm-5.1" },
    { "match": "*", "target": "" }  // Pass-through fallback
  ],
  "web_search_mode": "auto",        // auto / native / translate
  "web_search_model": "",           // web_search-specific model; empty means use the main target model
  "web_search_base_url": "",        // native web_search Anthropic upstream; empty means reuse the main upstream
  "web_search_api_key": "",         // native web_search API key; empty means reuse the main upstream key
  "thinking_budget_mappings": [
    // Map Claude Code thinking budget_tokens to model-supported OpenAI extension fields
    { "match": "glm-", "field": "thinking" },
    { "match": "kimi-", "field": "thinking_budget", "low": 1024, "medium": 4096, "high": 8192, "max": 16384 },
    { "match": "moonshot-", "field": "thinking_budget", "low": 1024, "medium": 4096, "high": 8192, "max": 16384 }
  ],
  "log_requests": true,
  "request_timeout_seconds": 0
}
```

Every field can be edited from the **Config** tab. Saving persists the configuration and hot-reloads the bridge by
rebuilding the upstream client, with no restart required. Model strings without a mapping are forwarded to Zen
unchanged.

When an OpenAI-compatible reasoning model returns `reasoning_content`, the proxy converts it into an Anthropic
`thinking` block and replays it as `reasoning_content` on the next upstream request. This satisfies thinking-mode
continuation requirements for models such as DeepSeek and GLM. `thinking_budget_mappings` only adds thinking controls
for explicitly matched models: GLM sends `thinking:{"type":"enabled","clear_thinking":false}` by default, Kimi/Moonshot
send `thinking_budget` by default, and DeepSeek does not receive `thinking_budget`, avoiding incompatible provider
parameters.

Claude Code `web_search_*` server tools support three modes. `auto` keeps the default smart routing behavior;
`native` passes web-search requests through to an Anthropic Messages upstream and can use a dedicated
`web_search_base_url` / `web_search_api_key` pair, for example DeepSeek's Anthropic API; `translate` uses the local
proxy-side search shim and summarizes results with `web_search_model`. The dedicated search API key is only returned
masked from the dashboard API.

## API Endpoints

### Model Proxy APIs

| Method | Path | Description |
|--------|------|-------------|
| POST | `/v1/messages` | Smart-routes by target model: Claude/Qwen use Anthropic, other models use OpenAI Chat translation |
| POST | `/v1/messages/count_tokens` | Best-effort token estimation |
| POST | `/v1/chat/completions` | OpenAI Chat Completions with streaming and non-streaming support |
| POST | `/v1/responses` | OpenAI Responses API for Codex CLI; converts to OpenAI Chat or native Anthropic upstreams |
| GET | `/v1/models` | Model list compatible with both OpenAI and Anthropic clients |
| GET | `/healthz` | Liveness probe |

### Dashboard API

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| GET | `/api/health` | None | Liveness probe |
| GET | `/api/auth/check` | None | Reports whether login is required and whether the current request is authenticated |
| POST | `/api/auth/login` | None | Verifies the panel password and sets an HttpOnly session cookie on success |
| POST | `/api/auth/logout` | None | Destroys the session and clears the cookie |
| GET | `/api/config` | Required | Current configuration snapshot (Zen API key masked) |
| PUT | `/api/config` | Required | Update, persist, and hot-reload the configuration |
| GET | `/api/stats/summary` | Required | Request count and token totals |
| GET | `/api/stats/hourly` | Required | Hourly time series |
| GET | `/api/stats/models` | Required | Per-model usage breakdown |
| GET | `/api/stats/latency` | Required | P50/P95/P99 latency percentiles |
| GET | `/api/logs` | Required | Request log list |
| GET | `/api/logs/{id}` | Required | Individual log entry with request and response bodies |
| GET/POST | `/api/keys` | Required | List or create API keys |
| GET/PUT/DELETE | `/api/keys/{id}` | Required | Read, update, or delete an API key |
| POST | `/api/keys/{id}/reset` | Required | Reset API key usage counters |
| GET | `/api/keys/{id}/usage` | Required | Read API key usage details |
| GET | `/api/test` | Required | Test upstream connectivity |

## Project Structure

```text
opencode-cc/
├── cmd/opencode-cc/        # Entry point
├── internal/
│   ├── config/             # JSON configuration and hot reload
│   ├── anthropic/          # Anthropic Messages API types and SSE writer
│   ├── upstream/           # Zen client and four protocol translators
│   │   ├── protocol.go     # Protocol interface and router
│   │   ├── anthropic.go    # Anthropic passthrough
│   │   ├── openai.go       # OpenAI Chat Completions translation
│   │   ├── responses.go    # OpenAI Responses translation
│   │   ├── google.go       # Google Gemini translation
│   │   ├── stream_emit.go  # Shared Anthropic SSE output
│   │   ├── models.go       # Catalog of 49 Zen models
│   │   └── client.go       # HTTP client with Bearer auth and GET /v1/models
│   ├── bridge/             # /v1/messages handler: route, translate, forward
│   ├── store/              # SQLite through modernc, with no CGO
│   └── web/                # Dashboard API and embedded SPA
├── web/                    # React, Vite, and Tailwind source
│   └── src/pages/{Dashboard,Inspector,Models,Config}.tsx
└── Dockerfile              # Multi-stage Node.js, Go, and distroless build
```

## Notes

- **Protocol-routing tradeoffs.** Translation from Anthropic to OpenAI, Responses, or Google is lossy because some
  Anthropic-specific concepts, such as `cache_control` and complete `thinking` signatures, have no equivalent in the
  target protocols. The Claude and Qwen passthrough path does not have this limitation.
- **Token usage.** The OpenAI path obtains exact counts from streaming `include_usage` chunks. Other protocols use
  upstream usage fields when available and fall back to estimates otherwise.
- **The model catalog may become outdated.** `internal/upstream/models.go` is a manually curated snapshot. Prices and
  capabilities can change. Use `GET https://opencode.ai/zen/v1/models` with an API key to retrieve the latest catalog.
- **Older SQLite databases are incompatible.** The schema changed during refactoring: `session_map` was removed and the
  `requests` fields changed. During development, delete `data/*.db`. Versioned migrations will be added for production.

## License

MIT
