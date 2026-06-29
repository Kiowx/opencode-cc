// Package store is a tiny SQLite-backed persistence layer for request logs
// and aggregate statistics shown in the web panel. It uses modernc.org/sqlite
// (pure Go, no CGO) so it compiles cleanly on Windows without a C toolchain.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store wraps a SQLite database connection.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the database at path and initialises the schema.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite serializes all access through a single connection. Allowing more
	// than one open connection leads to "database is locked" / transient
	// "SQL logic error" under concurrent writes (we insert asynchronously from
	// the proxy hot path). database/sql serializes operations on a 1-conn pool.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) migrate() error {
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	// Additive migrations: each ALTER is idempotent — if the column already
	// exists SQLite errors, which we ignore. This lets us evolve the requests
	// table across versions without a full migration framework.
	for _, stmt := range []string{
		`ALTER TABLE requests ADD COLUMN api_key_id INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE requests ADD COLUMN cached_input_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE requests ADD COLUMN cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE stats_hourly ADD COLUMN cached_input_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE stats_hourly ADD COLUMN cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := s.db.Exec(stmt); err != nil && !isDupColumnErr(err) {
			return err
		}
	}
	// Indexes referencing migrated columns must be created AFTER the ALTERs
	// (CREATE INDEX inside `schema` runs before them and would fail on an
	// existing DB where the column was just added).
	for _, stmt := range []string{
		`CREATE INDEX IF NOT EXISTS idx_requests_apikey ON requests(api_key_id)`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// isDupColumnErr reports whether err is the "duplicate column name" error that
// ALTER TABLE ADD COLUMN raises when the column already exists.
func isDupColumnErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate column")
}

const schema = `
CREATE TABLE IF NOT EXISTS requests (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    ts              INTEGER NOT NULL,
    method          TEXT    NOT NULL,
    path            TEXT    NOT NULL,
    incoming_model  TEXT    NOT NULL,
    target_model    TEXT    NOT NULL,
    stream          INTEGER NOT NULL DEFAULT 0,
    status          INTEGER NOT NULL DEFAULT 0,
    duration_ms     INTEGER NOT NULL DEFAULT 0,
    input_tokens    INTEGER NOT NULL DEFAULT 0,
    output_tokens   INTEGER NOT NULL DEFAULT 0,
    cached_input_tokens INTEGER NOT NULL DEFAULT 0,
    cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
    stop_reason     TEXT    NOT NULL DEFAULT '',
    error           TEXT    NOT NULL DEFAULT '',
    req_body        TEXT    NOT NULL DEFAULT '',
    resp_body       TEXT    NOT NULL DEFAULT '',
    api_key_id      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_requests_ts ON requests(ts DESC);

CREATE TABLE IF NOT EXISTS stats_hourly (
    hour            INTEGER PRIMARY KEY,
    requests        INTEGER NOT NULL DEFAULT 0,
    errors          INTEGER NOT NULL DEFAULT 0,
    input_tokens    INTEGER NOT NULL DEFAULT 0,
    output_tokens   INTEGER NOT NULL DEFAULT 0,
    cached_input_tokens INTEGER NOT NULL DEFAULT 0,
    cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS api_keys (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    key_hash            TEXT    NOT NULL UNIQUE,
    key_prefix          TEXT    NOT NULL,
    name                TEXT    NOT NULL DEFAULT '',
    enabled             INTEGER NOT NULL DEFAULT 1,
    token_quota         INTEGER NOT NULL DEFAULT 0,
    request_quota       INTEGER NOT NULL DEFAULT 0,
    daily_token_limit   INTEGER NOT NULL DEFAULT 0,
    daily_request_limit INTEGER NOT NULL DEFAULT 0,
    allowed_ips         TEXT    NOT NULL DEFAULT '',
    used_tokens         INTEGER NOT NULL DEFAULT 0,
    used_requests       INTEGER NOT NULL DEFAULT 0,
    daily_used_tokens   INTEGER NOT NULL DEFAULT 0,
    daily_used_requests INTEGER NOT NULL DEFAULT 0,
    daily_reset_ts      INTEGER NOT NULL DEFAULT 0,
    created_at          INTEGER NOT NULL DEFAULT 0,
    expires_at          INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS response_cache (
    cache_key       TEXT    PRIMARY KEY,
    target_model    TEXT    NOT NULL DEFAULT '',
    prompt_tokens   INTEGER NOT NULL DEFAULT 0,
    body            BLOB   NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    expires_at      INTEGER NOT NULL
);
`

// RequestRow is the Go representation of a logged request.
type RequestRow struct {
	ID                       int64     `json:"id"`
	Ts                       time.Time `json:"ts"`
	Method                   string    `json:"method"`
	Path                     string    `json:"path"`
	IncomingModel            string    `json:"incoming_model"`
	TargetModel              string    `json:"target_model"`
	Stream                   bool      `json:"stream"`
	Status                   int       `json:"status"`
	DurationMs               int64     `json:"duration_ms"`
	InputTokens              int       `json:"input_tokens"`
	OutputTokens             int       `json:"output_tokens"`
	CachedInputTokens        int       `json:"cached_input_tokens"`
	CacheCreationInputTokens int       `json:"cache_creation_input_tokens"`
	StopReason               string    `json:"stop_reason"`
	Error                    string    `json:"error"`
	ReqBody                  string    `json:"req_body"`
	RespBody                 string    `json:"resp_body"`
	APIKeyID                 int64     `json:"api_key_id"`
}

// InsertRequest writes one request row and bumps the hourly stats atomically.
func (s *Store) InsertRequest(ctx context.Context, r *RequestRow) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
INSERT INTO requests
(ts, method, path, incoming_model, target_model, stream, status, duration_ms,
 input_tokens, output_tokens, cached_input_tokens, cache_creation_input_tokens,
 stop_reason, error, req_body, resp_body, api_key_id)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.Ts.UnixMilli(), r.Method, r.Path, r.IncomingModel, r.TargetModel,
		boolToInt(r.Stream), r.Status, r.DurationMs, r.InputTokens, r.OutputTokens,
		r.CachedInputTokens, r.CacheCreationInputTokens,
		r.StopReason, r.Error, r.ReqBody, r.RespBody, r.APIKeyID,
	)
	if err != nil {
		return fmt.Errorf("insert request: %w", err)
	}

	hour := r.Ts.Truncate(time.Hour).Unix()
	errInc := 0
	if r.Status >= 400 || r.Error != "" {
		errInc = 1
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO stats_hourly (hour, requests, errors, input_tokens, output_tokens, cached_input_tokens, cache_creation_input_tokens)
VALUES (?, 1, ?, ?, ?, ?, ?)
ON CONFLICT(hour) DO UPDATE SET
    requests     = requests     + 1,
    errors       = errors       + excluded.errors,
    input_tokens = input_tokens + excluded.input_tokens,
    output_tokens= output_tokens+ excluded.output_tokens,
    cached_input_tokens = cached_input_tokens + excluded.cached_input_tokens,
    cache_creation_input_tokens = cache_creation_input_tokens + excluded.cache_creation_input_tokens`,
		hour, errInc, r.InputTokens, r.OutputTokens, r.CachedInputTokens, r.CacheCreationInputTokens,
	); err != nil {
		return fmt.Errorf("upsert stats: %w", err)
	}
	return tx.Commit()
}

// ListRequests returns the most recent `limit` request rows (max 500).
func (s *Store) ListRequests(ctx context.Context, limit int) ([]RequestRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, ts, method, path, incoming_model, target_model, stream, status,
       duration_ms, input_tokens, output_tokens, cached_input_tokens, cache_creation_input_tokens,
       stop_reason, error, req_body, resp_body, api_key_id
FROM requests ORDER BY ts DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRows(rows)
}

// GetRequest returns a single request row by id.
func (s *Store) GetRequest(ctx context.Context, id int64) (*RequestRow, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, ts, method, path, incoming_model, target_model, stream, status,
       duration_ms, input_tokens, output_tokens, cached_input_tokens, cache_creation_input_tokens,
       stop_reason, error, req_body, resp_body, api_key_id
FROM requests WHERE id = ?`, id)
	r := &RequestRow{}
	var ts int64
	var stream int
	if err := row.Scan(&r.ID, &ts, &r.Method, &r.Path, &r.IncomingModel, &r.TargetModel,
		&stream, &r.Status, &r.DurationMs, &r.InputTokens, &r.OutputTokens,
		&r.CachedInputTokens, &r.CacheCreationInputTokens,
		&r.StopReason, &r.Error, &r.ReqBody, &r.RespBody, &r.APIKeyID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	r.Ts = time.UnixMilli(ts)
	r.Stream = stream == 1
	return r, nil
}

func scanRows(rows *sql.Rows) ([]RequestRow, error) {
	out := make([]RequestRow, 0)
	for rows.Next() {
		var r RequestRow
		var ts int64
		var stream int
		if err := rows.Scan(&r.ID, &ts, &r.Method, &r.Path, &r.IncomingModel, &r.TargetModel,
			&stream, &r.Status, &r.DurationMs, &r.InputTokens, &r.OutputTokens,
			&r.CachedInputTokens, &r.CacheCreationInputTokens,
			&r.StopReason, &r.Error, &r.ReqBody, &r.RespBody, &r.APIKeyID); err != nil {
			return nil, err
		}
		r.Ts = time.UnixMilli(ts)
		r.Stream = stream == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// StatsSummary holds aggregated numbers for the panel.
type StatsSummary struct {
	TotalRequests              int64 `json:"total_requests"`
	TotalErrors                int64 `json:"total_errors"`
	TotalInputTok              int64 `json:"total_input_tokens"`
	TotalOutputTok             int64 `json:"total_output_tokens"`
	TotalCachedInputTok        int64 `json:"total_cached_input_tokens"`
	TotalCacheCreationInputTok int64 `json:"total_cache_creation_input_tokens"`
	RequestsLast24h            int64 `json:"requests_last_24h"`
	ErrorsLast24h              int64 `json:"errors_last_24h"`
}

// Summary returns lifetime + last-24h aggregates.
func (s *Store) Summary(ctx context.Context) (*StatsSummary, error) {
	cutoff := time.Now().Add(-24 * time.Hour).UnixMilli()
	row := s.db.QueryRowContext(ctx, `
SELECT
  COALESCE(SUM(requests),0), COALESCE(SUM(errors),0),
  COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
  COALESCE(SUM(cached_input_tokens),0), COALESCE(SUM(cache_creation_input_tokens),0)
FROM stats_hourly`)
	var sum StatsSummary
	if err := row.Scan(&sum.TotalRequests, &sum.TotalErrors, &sum.TotalInputTok, &sum.TotalOutputTok, &sum.TotalCachedInputTok, &sum.TotalCacheCreationInputTok); err != nil {
		return nil, err
	}
	row = s.db.QueryRowContext(ctx, `
SELECT COUNT(*), COALESCE(SUM(CASE WHEN status>=400 OR error!='' THEN 1 ELSE 0 END),0)
FROM requests WHERE ts >= ?`, cutoff)
	if err := row.Scan(&sum.RequestsLast24h, &sum.ErrorsLast24h); err != nil {
		return nil, err
	}
	return &sum, nil
}

// HourPoint is one bucket in a time series.
type HourPoint struct {
	Hour                     int64 `json:"hour"`
	Requests                 int64 `json:"requests"`
	Errors                   int64 `json:"errors"`
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CachedInputTokens        int64 `json:"cached_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

// HourlySeries returns the per-hour series for the last `hours` hours.
func (s *Store) HourlySeries(ctx context.Context, hours int) ([]HourPoint, error) {
	if hours <= 0 {
		hours = 24
	}
	from := time.Now().Truncate(time.Hour).Add(-time.Duration(hours-1) * time.Hour).Unix()
	rows, err := s.db.QueryContext(ctx, `
SELECT hour, requests, errors, input_tokens, output_tokens, cached_input_tokens, cache_creation_input_tokens
FROM stats_hourly WHERE hour >= ? ORDER BY hour ASC`, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]HourPoint, 0)
	for rows.Next() {
		var p HourPoint
		if err := rows.Scan(&p.Hour, &p.Requests, &p.Errors, &p.InputTokens, &p.OutputTokens, &p.CachedInputTokens, &p.CacheCreationInputTokens); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ModelUsagePoint tallies requests per target model.
type ModelUsagePoint struct {
	Model             string `json:"model"`
	Requests          int64  `json:"requests"`
	Tokens            int64  `json:"tokens"`
	CachedInputTokens int64  `json:"cached_input_tokens"`
}

// ModelUsage returns request/token counts grouped by target model.
func (s *Store) ModelUsage(ctx context.Context, since int64) ([]ModelUsagePoint, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT target_model, COUNT(*) AS req_count, COALESCE(SUM(input_tokens+output_tokens),0), COALESCE(SUM(cached_input_tokens),0)
FROM requests WHERE ts >= ? GROUP BY target_model ORDER BY req_count DESC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ModelUsagePoint, 0)
	for rows.Next() {
		var p ModelUsagePoint
		if err := rows.Scan(&p.Model, &p.Requests, &p.Tokens, &p.CachedInputTokens); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RecentLatency returns the last N durations (ms) for percentile calc.
func (s *Store) RecentLatency(ctx context.Context, limit int) ([]int64, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT duration_ms FROM requests WHERE duration_ms > 0 ORDER BY ts DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var d int64
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ResponseCacheRow is the on-disk representation of a cached response.
type ResponseCacheRow struct {
	CacheKey     string
	TargetModel  string
	PromptTokens int
	Body         []byte
	CreatedAt    time.Time
	ExpiresAt    time.Time
}

// SaveResponseCacheEntry upserts one response cache row.
func (s *Store) SaveResponseCacheEntry(ctx context.Context, key, model string, promptTokens int, body []byte, createdAt, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
INSERT OR REPLACE INTO response_cache (cache_key, target_model, prompt_tokens, body, created_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?)`,
		key, model, promptTokens, body, createdAt.UnixMilli(), expiresAt.UnixMilli())
	return err
}

// LoadResponseCacheEntries reads up to maxEntries non-expired cache rows,
// ordered by creation time (most recent first).
func (s *Store) LoadResponseCacheEntries(ctx context.Context, maxEntries int) ([]ResponseCacheRow, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT cache_key, target_model, prompt_tokens, body, created_at, expires_at
FROM response_cache WHERE expires_at > ? ORDER BY created_at DESC LIMIT ?`,
		time.Now().UnixMilli(), maxEntries)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ResponseCacheRow
	for rows.Next() {
		var r ResponseCacheRow
		var ca, ea int64
		if err := rows.Scan(&r.CacheKey, &r.TargetModel, &r.PromptTokens, &r.Body, &ca, &ea); err != nil {
			return nil, err
		}
		r.CreatedAt = time.UnixMilli(ca)
		r.ExpiresAt = time.UnixMilli(ea)
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteExpiredResponseCache removes rows whose expiry has passed.
func (s *Store) DeleteExpiredResponseCache(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM response_cache WHERE expires_at < ?`, time.Now().UnixMilli())
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
