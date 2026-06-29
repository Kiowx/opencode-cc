// Tiny fetch wrapper for the panel API. All endpoints live under /api.

export interface Summary {
  total_requests: number;
  total_errors: number;
  total_input_tokens: number;
  total_output_tokens: number;
  total_cached_input_tokens: number;
  total_cache_creation_input_tokens: number;
  requests_last_24h: number;
  errors_last_24h: number;
}

export interface HourPoint {
  hour: number; // epoch seconds
  requests: number;
  errors: number;
  input_tokens: number;
  output_tokens: number;
  cached_input_tokens: number;
  cache_creation_input_tokens: number;
}

export interface ModelUsagePoint {
  model: string;
  requests: number;
  tokens: number;
  cached_input_tokens: number;
}

export interface Latency {
  p50: number;
  p95: number;
  p99: number;
}

export interface LogRow {
  id: number;
  ts: string;
  method: string;
  path: string;
  incoming_model: string;
  target_model: string;
  stream: boolean;
  status: number;
  duration_ms: number;
  input_tokens: number;
  output_tokens: number;
  cached_input_tokens: number;
  cache_creation_input_tokens: number;
  stop_reason: string;
  error: string;
  req_body?: string;
  resp_body?: string;
}

export interface UpstreamView {
  base_url: string;
  api_key_masked: string;
  api_key_set: boolean;
  name: string;
  enabled: boolean;
}

export interface PanelConfig {
  listen_addr: string;
  upstream_base: string;
  native_anthropic: boolean;
  zen_api_key_masked: string;
  zen_api_key_set: boolean;
  upstreams: UpstreamView[];
  panel_token_set: boolean;
  require_api_key: boolean;
  default_model: string;
  model_mappings: { match: string; target: string }[];
  web_search_model: string;
  web_search_mode: string;
  web_search_base_url: string;
  web_search_api_key_masked: string;
  web_search_api_key_set: boolean;
  log_requests: boolean;
  max_body_log_bytes: number;
  request_timeout_seconds: number;
  prompt_cache_enabled: boolean;
  prompt_cache_key_prefix: string;
  prompt_cache_anthropic_control: boolean;
  prompt_cache_normalize: boolean;
  response_cache_enabled: boolean;
  response_cache_ttl_seconds: number;
  response_cache_max_entries: number;
  response_cache_warmup_enabled: boolean;
  response_cache_warmup_prompts: { model: string; system?: string; user_message: string; tools?: any[] }[];
  thinking_budget_mappings: {
    match: string;
    field: string;
    low?: number;
    medium?: number;
    high?: number;
    max?: number;
  }[];
}

export interface TestResult {
  ok: boolean;
  model: string;
  elapsed_ms?: number;
  prompt_tokens?: number;
  completion_tokens?: number;
  preview?: string;
  error?: string;
  upstreams?: TestResult[];
}

export interface APIKey {
  id: number;
  key_prefix: string;
  name: string;
  enabled: boolean;
  token_quota: number;
  request_quota: number;
  daily_token_limit: number;
  daily_request_limit: number;
  allowed_ips: string;
  used_tokens: number;
  used_requests: number;
  daily_used_tokens: number;
  daily_used_requests: number;
  daily_reset_ts: number;
  created_at: number;
  expires_at: number;
}

export interface APIKeyCreated extends APIKey {
  plain_key: string;
}

export interface KeyUsagePoint {
  day: number;
  requests: number;
  input_tokens: number;
  output_tokens: number;
}

export interface KeyBody {
  name: string;
  enabled?: boolean;
  token_quota: number;
  request_quota: number;
  daily_token_limit: number;
  daily_request_limit: number;
  allowed_ips: string;
  expires_at: number;
}

export interface AuthCheckResult {
  need_auth: boolean;
  authenticated: boolean;
}

async function asJson<T>(res: Response): Promise<T> {
  if (!res.ok) {
    const t = await res.text().catch(() => res.statusText);
    throw new Error(t || `HTTP ${res.status}`);
  }
  return (await res.json()) as T;
}

async function asArray<T>(res: Response): Promise<T[]> {
  const value = await asJson<unknown>(res);
  return Array.isArray(value) ? (value as T[]) : [];
}

export const api = {
  health: () => fetch("/api/health").then((r) => asJson<{ ok: boolean }>(r)),
  summary: () => fetch("/api/stats/summary").then((r) => asJson<Summary>(r)),
  hourly: (hours = 24) =>
    fetch(`/api/stats/hourly?hours=${hours}`).then((r) => asArray<HourPoint>(r)),
  models: (hours = 24) =>
    fetch(`/api/stats/models?hours=${hours}`).then((r) => asArray<ModelUsagePoint>(r)),
  latency: () => fetch("/api/stats/latency").then((r) => asJson<Latency>(r)),
  logs: (limit = 100) =>
    fetch(`/api/logs?limit=${limit}`).then((r) => asArray<LogRow>(r)),
  log: (id: number) => fetch(`/api/logs/${id}`).then((r) => asJson<LogRow>(r)),
  getConfig: () => fetch("/api/config").then((r) => asJson<PanelConfig>(r)),
  putConfig: (body: unknown) =>
    fetch("/api/config", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    }).then((r) => asJson<PanelConfig>(r)),
  test: (model?: string) =>
    fetch(`/api/test${model ? `?model=${encodeURIComponent(model)}` : ""}`).then((r) =>
      asJson<TestResult>(r)
    ),
  keys: () => fetch("/api/keys").then((r) => asArray<APIKey>(r)),
  createKey: (body: KeyBody) =>
    fetch("/api/keys", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    }).then((r) => asJson<APIKeyCreated>(r)),
  updateKey: (id: number, body: KeyBody) =>
    fetch(`/api/keys/${id}`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    }).then((r) => asJson<APIKey>(r)),
  deleteKey: (id: number) =>
    fetch(`/api/keys/${id}`, { method: "DELETE" }).then((r) => asJson<{ ok: boolean }>(r)),
  resetKey: (id: number) =>
    fetch(`/api/keys/${id}/reset`, { method: "POST" }).then((r) => asJson<{ ok: boolean }>(r)),
  keyUsage: (id: number, days = 14) =>
    fetch(`/api/keys/${id}/usage?days=${days}`).then((r) => asArray<KeyUsagePoint>(r)),
  checkAuth: () =>
    fetch("/api/auth/check").then((r) => asJson<AuthCheckResult>(r)),
  login: (password: string) =>
    fetch("/api/auth/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ password }),
    }).then((r) => asJson<{ ok: boolean }>(r)),
  logout: () =>
    fetch("/api/auth/logout", { method: "POST" }).then((r) => asJson<{ ok: boolean }>(r)),
};
