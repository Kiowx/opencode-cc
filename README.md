# opencode-cc

[简体中文](README.md) | [English](README_EN.md)

> Anthropic / OpenAI → OpenCode Zen 多协议桥接代理，附带嵌入式 Web 控制面板。

`opencode-cc` 是一个高性能 Go 代理，对外暴露 **Anthropic Messages API** 和
**OpenAI Chat Completions API**（均支持流式与非流式），并将请求转发到 **[OpenCode Zen](https://opencode.ai/docs/zen/)**
网关的原生协议。Zen 上有 49 个模型分属 4 种协议——本代理按目标 model id 自动路由到正确的协议翻译器，让 Claude Code 透明地跑在 GLM / Kimi / DeepSeek / Qwen / Claude / GPT / Gemini 等模型上。

友情链接：[linuxdo](https://linux.do/)

```
┌────────────┐  POST /v1/messages   ┌──────────────┐  按 model 路由 ↓       ┌──────────┐
│ Claude Code│ ───────────────────> │ opencode-cc  │ ──────────────────────> │ OpenCode │
│            │ <── Anthropic SSE ── │   (Go)       │ <── 各协议 SSE ─────── │   Zen    │
└────────────┘                      └──────────────┘                         └──────────┘
                                       嵌入式 React 控制面板 ▲ /api/*
                                          SQLite（纯 Go，无 CGO）
```

<img width="2540" height="1193" alt="QQ_1781519790308" src="https://github.com/user-attachments/assets/2854f45f-62a0-463a-9b22-a07a770670b4" />

## 4 种协议自动路由

Zen 不是单一 OpenAI 兼容端点——按模型来源分成 4 条路径。代理根据 model id 前缀自动选择：

| 协议 | 路径 | 适用模型 | 翻译方式 |
|------|------|----------|----------|
| **OpenAI** | `/v1/chat/completions` | GLM、Kimi、DeepSeek、MiniMax、MiMo、Grok、免费模型 | Anthropic ↔ OpenAI 双向翻译 |
| **Anthropic** | `/v1/messages` | Claude、Qwen | 近乎透传（仅改写 model id） |
| **Responses** | `/v1/responses` | GPT 全家桶 | Anthropic ↔ OpenAI Responses API |
| **Google** | `/v1beta/models/{id}` | Gemini | Anthropic ↔ Google Generative Language |

每种协议实现 `upstream.Protocol` 接口的 `TranslateRequest` / `TranslateResponse` / `TranslateStream` 三个方法。

## 特性

- **完整工具调用支持。** Anthropic `tool_use` / `tool_result` 块在 OpenAI 协议下双向翻译为 `tool_calls` / `role:"tool"` 消息；Claude Code 的工具定义转成 OpenAI function tools。国产模型的 `reasoning_content` 扩展字段翻译为 Anthropic `thinking` 块。
- **4 协议自动路由。** 根据 model id 前缀（`claude-`/`qwen` → Anthropic、`gpt-` → Responses、`gemini-` → Google、其余 → OpenAI），无需手动配置。
- **OpenAI 客户端兼容。** 支持 `POST /v1/chat/completions` 和 OpenAI 格式的 `/v1/models`，可直接接入 OpenAI SDK、Cherry Studio、ChatBox 等兼容客户端。
  OpenAI 路径直接透传 Zen `/go/v1/chat/completions` 的原生 JSON/SSE 响应，不转换成 Anthropic 格式。
- **Codex CLI 兼容。** 提供 `POST /v1/responses`，将 Codex 使用的 Responses API 请求、流式文本事件和函数调用转换到 Zen；OpenAI 兼容模型走 `/go/v1/chat/completions`，Claude/Qwen 目标模型可走原生 `/v1/messages`。
- **Anthropic 智能原生路由。** 默认开启 `native_anthropic`，仅当目标模型是 `claude-*` / `qwen*` 时直连上游 `/v1/messages` 并保留 `cache_control`、Anthropic 原生 SSE 和其它扩展字段；GLM、DeepSeek、Kimi 等目标模型继续走转换模式。
- **49 个预置模型目录。** Models 页展示所有模型的价格、context、能力标签、协议徽章；Config 页可从目录里一键选模型加入映射。
- **单一静态二进制。** React SPA 通过 `embed.FS` 内嵌，运行时无需 Node；SQLite 用纯 Go 驱动，无 CGO，可干净交叉编译。
- **Web 控制面板。** Dashboard（流量图、健康状态、模型分布）、Models（模型库浏览 + 筛选）、Inspector（实时请求列表，显示走的哪个协议）、Config（Zen 配置、代理鉴权、模型映射——热更新）。
- **面板密码保护。** 在 `config.json` 或 Settings 页面设置 `panel_token` 后，访问 Web 控制面板需要先通过登录页验证密码。登录后以 HttpOnly Session Cookie（24 小时有效期）维持会话；支持从侧边栏一键退出登录。未设置 `panel_token` 时面板保持开放（适合本地单机使用）。
- **客户端 API Key 管理。** 可在控制面板创建、停用和删除客户端密钥，并为每个密钥设置总 token 配额、每日 token 限额和允许访问的 IP；用量会自动统计。
- **默认安全。** 恒定时间 Bearer token 鉴权、请求体大小限制、单请求 panic 恢复、优雅关闭。

## 快速开始

### 前置条件

- Go 1.22+
- Node 20+（仅构建 UI 时需要；运行时不需要）
- 一个 OpenCode Zen API key——到 [opencode.ai/auth](https://opencode.ai/auth) 登录、添加账单信息、复制 API key

### 构建与运行

```bash
make            # 构建前端 + Go 二进制 -> ./opencode-cc
./opencode-cc   # 启动于 :8787，自动创建 config.json 与 data/opencode-cc.db
```

打开控制面板 `http://localhost:8787/`，进入 **Config** 标签页：
1. 在 "Upstream (OpenCode Zen)" 卡片填入你的 **API key**
2. 点 "Test connection" 确认能连通
3. 在 "Model mappings" 里调整 Claude Code 模型名到 Zen 模型的映射（默认已预置几条省钱组合，例如 `claude-sonnet-4-5` → `glm-5.1`）

然后指向 Claude Code：

```bash
export ANTHROPIC_BASE_URL=http://localhost:8787
export ANTHROPIC_AUTH_TOKEN=local    # 未设 token 时任意值都行
claude
```

如果你的上游 Base URL 支持 Anthropic Messages（例如 `<base>/v1/messages`），默认会使用 **Anthropic 智能原生路由**。也可以显式设置：

```bash
export OPENCODE_CC_UPSTREAM=https://opencode.ai/zen/go
export OPENCODE_CC_NATIVE_ANTHROPIC=true
```

开启后，代理会先按映射表解析目标模型：`claude-*` / `qwen*` 目标模型直连上游 `/v1/messages`，只改写 `model`；其它目标模型仍转换到 OpenAI Chat Completions。例如上面配置中，Claude/Qwen 目标模型会转发到 `https://opencode.ai/zen/go/v1/messages`。原生路径会同时发送 `Authorization: Bearer <key>` 和 `x-api-key: <key>`，兼容 OpenAI/Anthropic 两种鉴权风格。

使用 OpenAI SDK 或兼容客户端时，将 Base URL 设置为 `http://localhost:8787/v1`：

> 客户端的接口类型必须选择 **OpenAI**。如果请求路径是 `/v1/messages`，说明客户端仍处于 Anthropic 模式，
> 此时响应会按 Anthropic Messages 协议转换，而不是 OpenAI 原生透传。

```bash
export OPENAI_BASE_URL=http://localhost:8787/v1
export OPENAI_API_KEY=local          # 开启客户端鉴权后改为控制面板创建的 API Key
```

```bash
curl http://localhost:8787/v1/chat/completions \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"glm-5.1","messages":[{"role":"user","content":"你好"}]}'
```

### 接入 Codex CLI

Codex 的自定义模型提供商配置必须写在用户级配置文件：

- Linux/macOS：`~/.codex/config.toml`
- Windows：`%USERPROFILE%\.codex\config.toml`

```toml
model = "kimi-k2.7-code"
model_provider = "opencode_cc"

[model_providers.opencode_cc]
name = "opencode-cc"
base_url = "http://localhost:8787/v1"
env_key = "OPENCODE_CC_API_KEY"
wire_api = "responses"
```

启动 Codex 前设置客户端密钥。未开启客户端 API Key 鉴权时可使用任意非空值；开启后改为控制面板创建的密钥：

```bash
export OPENCODE_CC_API_KEY=local
codex
```

PowerShell：

```powershell
$env:OPENCODE_CC_API_KEY = "local"
codex
```

`model` 可以直接填写 Zen 模型 ID，也可以填写控制面板中配置的映射别名。推荐编码模型 `kimi-k2.7-code`。如果模型映射到 `claude-*` / `qwen*`，Codex 的 `/v1/responses` 请求会被转换为原生 Anthropic Messages 上游请求；其它模型继续转换为 OpenAI Chat Completions。

### 开发模式（HMR 热更新）

```bash
make dev   # Vite 跑在 :5174 + Go 跑在 :8787，API 通过 Vite 代理
```

### Docker

镜像发布到 GitHub Container Registry：`ghcr.io/kiowx/opencode-cc`。

**一键运行**（无需 clone 仓库；请替换 API key 和面板密码）：

```bash
docker run -d --name opencode-cc --restart unless-stopped \
  -p 8787:8787 \
  -v opencode-cc-data:/data \
  -e ZEN_API_KEY=sk-你的key \
  -e OPENCODE_CC_PANEL_TOKEN=你的面板密码 \
  ghcr.io/kiowx/opencode-cc:latest
```

启动后访问 `http://服务器IP:8787`，使用 `OPENCODE_CC_PANEL_TOKEN` 登录。命名卷
`opencode-cc-data` 会持久化配置和 SQLite 数据。对外开放端口时强烈建议设置面板密码，
否则任何能访问该地址的人都可以打开控制面板。

仓库中的 `docker-compose.yml` 提供相同的持久化配置：

```bash
docker compose up -d
```

更新到最新版本：

```bash
docker compose pull
docker compose up -d
docker image prune -f
```

使用 `docker run` 部署时，更新命令为：

```bash
docker pull ghcr.io/kiowx/opencode-cc:latest
docker rm -f opencode-cc
# 再执行上面的一键运行命令
```

也可以把镜像标签从 `latest` 改为例如 `1.3.2` 来固定版本。如需本地构建：

```bash
docker build -t opencode-cc .
docker run -d --name opencode-cc -p 8787:8787 -v opencode-cc-data:/data opencode-cc
```

每次推送 `v*` 标签时，GitHub Actions 会自动发布 Linux amd64/arm64 镜像，并更新
版本号、主次版本号和 `latest` 标签。GHCR 包首次发布后需在 GitHub Packages 设置中
将可见性改为 Public，才能匿名拉取。

## 翻译原理

### 请求路由

`POST /v1/messages` 进来后，代理根据 `req.Model` 在映射表里找到目标 Zen model id（找不到则原样透传），再用 `upstream.Router.For(modelID)` 选协议。所有 4 种协议共享一个统一的输出侧（`anthropicEmitter`），把翻译后的内容块以 Anthropic SSE 标准事件序列发出。

### Anthropic ↔ OpenAI 翻译（最复杂）

| Anthropic | OpenAI Chat Completions |
|-----------|-------------------------|
| `tools[]{name, description, input_schema}` | `tools[]{type:"function", function:{name, description, parameters}}` |
| `content[]{type:"tool_use", id, name, input(object)}` | `assistant.tool_calls[]{id, function:{name, arguments(JSON 字符串)}}` |
| `user.content[]{type:"tool_result", tool_use_id, content}` | `{role:"tool", tool_call_id, content}` |
| `content[]{type:"thinking"}` | `delta.reasoning_content`（DeepSeek/GLM/Kimi 扩展字段） |
| `stop_reason:"tool_use"` | `finish_reason:"tool_calls"` |
| 流式 `input_json_delta`（按 index 累积） | 流式 `delta.tool_calls[].function.arguments`（按 index 累积） |

流式输出时，工具调用的 `arguments` 分片按 `index` 累积，完整后再作为单个 `tool_use` 块发出，保证 Claude Code 收到结构完整的工具调用。

### Anthropic 透传（Claude/Qwen）

Zen 对 Claude 和 Qwen 模型直接提供原生 Anthropic Messages API。代理只改写 model id，请求体、响应体、SSE 流全部原样转发。这是最简单的一条路径，连工具调用都无需翻译。

### Codex Responses ↔ OpenAI Chat / Anthropic Messages

Codex 请求 `/v1/responses` 时，代理会按目标模型智能选择上游：非 Anthropic 原生模型转成 OpenAI Chat Completions 消息；`claude-*` / `qwen*` 目标模型转成 Anthropic Messages 请求。Zen 返回的文本增量、工具调用参数分片和 usage 会重新组装成 `response.output_text.delta`、`response.function_call_arguments.delta`、`response.completed` 等标准 Responses SSE 事件。

此兼容层由代理本地完成，不要求 Zen 上游原生提供 `/v1/responses`。

### Anthropic ↔ Google（Gemini）

`messages[]` → `contents[]`（role: user/model），`tools[]` → `tools[].functionDeclarations`。Gemini 流式返回 JSON 数组（每个 chunk 含 `candidates[].content.parts[]`），代理解析后翻译为 Anthropic 增量。

## 配置

`config.json`（首次运行自动创建）：

```jsonc
{
  "listen_addr": ":8787",
  "upstream_base": "https://opencode.ai/zen",
  "native_anthropic": true,   // true 时 claude-* / qwen* 目标模型智能直连 <upstream_base>/v1/messages
  "zen_api_key": "",           // Bearer token，必填
  "panel_token": "",           // 控制面板登录密码；"" = 开放（本地用）
  "require_api_key": false,    // true 时 /v1/* 必须携带有效 API Key
  "default_model": "glm-4.6",
  "model_mappings": [
    // Claude Code 发来的 model 字符串 → Zen 真实 model id
    { "match": "claude-sonnet-4-5", "target": "glm-5.1" },
    { "match": "*", "target": "" }  // 透传兜底
  ],
  "web_search_mode": "auto",        // auto / native / translate
  "web_search_model": "",           // web_search 专用模型，"" = 沿用主模型
  "web_search_base_url": "",        // native web_search 专用 Anthropic 上游，"" = 沿用主上游
  "web_search_api_key": "",         // native web_search 专用 API key，"" = 沿用主上游 key
  "thinking_budget_mappings": [
    // 将 Claude Code 的 thinking budget_tokens 映射为模型支持的 OpenAI 扩展字段
    { "match": "glm-", "field": "thinking" },
    { "match": "kimi-", "field": "thinking_budget", "low": 1024, "medium": 4096, "high": 8192, "max": 16384 },
    { "match": "moonshot-", "field": "thinking_budget", "low": 1024, "medium": 4096, "high": 8192, "max": 16384 }
  ],
  "log_requests": true,
  "request_timeout_seconds": 0
}
```

所有字段都可在 **Config** 标签页编辑，保存后桥接器热更新（重建上游客户端），无需重启。未在映射表里的 model 字符串原样转发给 Zen。

OpenAI 兼容的推理模型如果返回 `reasoning_content`，代理会把它转换成 Anthropic `thinking` 块，并在下一轮请求中作为 `reasoning_content` 回传给上游，满足 DeepSeek / GLM 等模型的 thinking mode 连续对话要求。`thinking_budget_mappings` 只给明确匹配的模型追加思考控制字段：GLM 默认发送 `thinking:{"type":"enabled","clear_thinking":false}`，Kimi/Moonshot 默认发送 `thinking_budget`，DeepSeek 默认不发送 `thinking_budget`，避免触发不兼容参数。

Claude Code 的 `web_search_*` server tool 支持三种模式：`auto` 保持默认智能路由；`native` 将带 web_search 的请求原样发送到 Anthropic Messages 上游，可配合 `web_search_base_url` / `web_search_api_key` 单独接入 DeepSeek Anthropic API；`translate` 使用代理本地搜索 shim，并用 `web_search_model` 整理结果。搜索专用 API key 在面板和 `/api/config` 响应中只返回脱敏状态。

## API 接口

### 模型代理 API

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/v1/messages` | 按目标模型智能路由：Claude/Qwen 直连 Anthropic，其它模型转 OpenAI Chat |
| POST | `/v1/messages/count_tokens` | 尽力而为的 token 估算 |
| POST | `/v1/chat/completions` | OpenAI Chat Completions，支持流式与非流式 |
| POST | `/v1/responses` | OpenAI Responses API，兼容 Codex CLI，可转 OpenAI Chat 或原生 Anthropic 上游 |
| GET | `/v1/models` | 同时兼容 OpenAI 与 Anthropic 的模型列表 |
| GET | `/healthz` | 存活探针 |

### 控制面板（给 UI 用）

| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| GET | `/api/health` | 无 | 存活探针 |
| GET | `/api/auth/check` | 无 | 返回是否需要登录及当前是否已认证 |
| POST | `/api/auth/login` | 无 | 密码验证，成功后写入 Session Cookie |
| POST | `/api/auth/logout` | 无 | 销毁 Session，清除 Cookie |
| GET | `/api/config` | 需要 | 当前配置快照（ZenAPIKey 脱敏） |
| PUT | `/api/config` | 需要 | 更新 + 持久化 + 热更新 |
| GET | `/api/stats/summary` | 需要 | 请求数、token 汇总 |
| GET | `/api/stats/hourly` | 需要 | 小时级时序 |
| GET | `/api/stats/models` | 需要 | 分模型用量 |
| GET | `/api/stats/latency` | 需要 | P50/P95/P99 延迟 |
| GET | `/api/logs` | 需要 | 请求日志列表 |
| GET | `/api/logs/{id}` | 需要 | 单条日志详情（含请求/响应体） |
| GET/POST | `/api/keys` | 需要 | 列出 / 创建 API Key |
| GET/PUT/DELETE | `/api/keys/{id}` | 需要 | 查询 / 更新 / 删除 API Key |
| POST | `/api/keys/{id}/reset` | 需要 | 重置 API Key 用量 |
| GET | `/api/keys/{id}/usage` | 需要 | 查询 API Key 用量明细 |
| GET | `/api/test` | 需要 | 测试上游连通性 |

## 项目结构

```
opencode-cc/
├── cmd/opencode-cc/        # 入口
├── internal/
│   ├── config/             # JSON 配置 + 热更新
│   ├── anthropic/          # Anthropic Messages API 线路类型 + SSE writer
│   ├── upstream/           # Zen 网关客户端 + 4 个协议翻译器
│   │   ├── protocol.go     # Protocol 接口 + 路由器
│   │   ├── anthropic.go    # Anthropic 透传
│   │   ├── openai.go       # OpenAI Chat Completions 翻译（国产模型）
│   │   ├── responses.go    # OpenAI Responses 翻译（GPT）
│   │   ├── google.go       # Google Gemini 翻译
│   │   ├── stream_emit.go  # 共享的 Anthropic SSE 输出侧
│   │   ├── models.go       # 49 个 Zen 模型目录
│   │   └── client.go       # HTTP 客户端（Bearer + GET /v1/models）
│   ├── bridge/             # /v1/messages handler：路由 → 翻译 → 转发
│   ├── store/              # SQLite（modernc，纯 Go）
│   └── web/                # 控制面板 API + 嵌入式 SPA
├── web/                    # React + Vite + Tailwind 源码
│   └── src/pages/{Dashboard,Inspector,Models,Config}.tsx
└── Dockerfile              # 多阶段：node → go → distroless
```

## 注意事项

- **协议路由的代价。** Anthropic → OpenAI / Responses / Google 是有损翻译——某些 Anthropic 特有概念（如 `cache_control`、`thinking` 的 signature 完整性）在后端协议里没有对应物。Claude/Qwen 透传路径无此问题。
- **token 用量。** OpenAI 协议下用流式的 `include_usage` chunk 拿真实计数；其他协议按上游返回的 usage 字段读取，缺失时回退到估算。
- **模型目录可能过时。** `internal/upstream/models.go` 是手工整理的快照，价格和能力会变。可通过 `GET https://opencode.ai/zen/v1/models`（需 API key）拉取最新清单校准。
- **旧 SQLite 库不兼容。** 重构后 schema 变了（删了 session_map 表，requests 改字段）。开发期直接删 `data/*.db`；正式版会加版本化迁移。

## 许可证

MIT
