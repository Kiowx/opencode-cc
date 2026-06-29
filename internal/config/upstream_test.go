package config

import (
	"testing"
)

// TestNextUpstreamStickyPrimary verifies requests always go to index 0
// (primary) until MarkUpstreamFailed is called, which advances to the next
// enabled upstream. Disabled and empty-key entries are skipped.
func TestNextUpstreamStickyPrimary(t *testing.T) {
	c := Default()
	c.Upstreams = []Upstream{
		{BaseURL: "https://a.example", APIKey: "ka", Enabled: true},
		{BaseURL: "https://b.example", APIKey: "kb", Enabled: true},
		{BaseURL: "https://c.example", APIKey: "kc", Enabled: false}, // disabled, skipped
		{BaseURL: "https://d.example", APIKey: "", Enabled: true},    // empty key, skipped
	}

	// All 6 requests go to the primary (a) by default.
	for i := 0; i < 6; i++ {
		base, key, ok := c.NextUpstream()
		if !ok {
			t.Fatalf("request %d: expected ok", i)
		}
		if base != "https://a.example" || key != "ka" {
			t.Errorf("request %d: got %s/%s, want a.example/ka", i, base, key)
		}
	}

	// After marking failed, switch to b.
	c.MarkUpstreamFailed()
	base, key, ok := c.NextUpstream()
	if !ok || base != "https://b.example" || key != "kb" {
		t.Errorf("after failover: got %s/%s, want b.example/kb", base, key)
	}

	// disabled/empty must never be selected.
	c.MarkUpstreamFailed()
	_, _, ok = c.NextUpstream()
	// After b, we wrap around since c and d are skipped → back to a.
	base, key, ok = c.NextUpstream()
	if !ok || base != "https://a.example" {
		t.Errorf("after wrap: got %s, want a.example", base)
	}
}

// TestNextUpstreamLegacyFallback confirms the pool-empty case falls back to the
// legacy single UpstreamBase/ZenAPIKey fields (so existing configs keep working).
func TestNextUpstreamLegacyFallback(t *testing.T) {
	c := Default()
	c.Upstreams = nil
	c.UpstreamBase = "https://legacy.example/"
	c.ZenAPIKey = "legacy-key"

	base, key, ok := c.NextUpstream()
	if !ok {
		t.Fatalf("expected ok via legacy fallback")
	}
	if base != "https://legacy.example" {
		t.Errorf("base = %q, want https://legacy.example (trailing slash trimmed)", base)
	}
	if key != "legacy-key" {
		t.Errorf("key = %q, want legacy-key", key)
	}
}

// TestNextUpstreamNoneConfigured returns ok=false when nothing is set.
func TestNextUpstreamNoneConfigured(t *testing.T) {
	c := Default()
	c.Upstreams = nil
	c.UpstreamBase = ""
	c.ZenAPIKey = ""
	if _, _, ok := c.NextUpstream(); ok {
		t.Errorf("expected ok=false with no upstream configured")
	}
}

// TestMigrateLegacyUpstream promotes the legacy pair into the pool exactly once.
func TestMigrateLegacyUpstream(t *testing.T) {
	c := Default()
	c.UpstreamBase = "https://opencode.ai/zen/go"
	c.ZenAPIKey = "sk-test"
	c.Upstreams = nil

	c.migrateLegacyUpstream()
	if len(c.Upstreams) != 1 {
		t.Fatalf("expected 1 upstream after migration, got %d", len(c.Upstreams))
	}
	u := c.Upstreams[0]
	if u.BaseURL != "https://opencode.ai/zen/go" || u.APIKey != "sk-test" || !u.Enabled {
		t.Errorf("migrated upstream wrong: %+v", u)
	}

	// Idempotent: running again must not duplicate.
	c.migrateLegacyUpstream()
	if len(c.Upstreams) != 1 {
		t.Errorf("migration not idempotent: got %d upstreams", len(c.Upstreams))
	}
}

// TestMigrateLegacyUpstreamSkipsWhenPoolPresent ensures we never clobber an
// existing pool with the legacy fields.
func TestMigrateLegacyUpstreamSkipsWhenPoolPresent(t *testing.T) {
	c := Default()
	c.UpstreamBase = "https://legacy.example"
	c.ZenAPIKey = "legacy-key"
	c.Upstreams = []Upstream{{BaseURL: "https://pool.example", APIKey: "pk", Enabled: true}}

	c.migrateLegacyUpstream()
	if len(c.Upstreams) != 1 || c.Upstreams[0].BaseURL != "https://pool.example" {
		t.Errorf("pool clobbered by migration: %+v", c.Upstreams)
	}
}

func TestApplyPatchPreservesAPIKeyWhenBlank(t *testing.T) {
	c := Default()
	c.Upstreams = []Upstream{{
		BaseURL: "https://old.example/",
		APIKey:  "old-api-key",
		Enabled: true,
	}}

	next := []Upstream{{
		BaseURL: "https://new.example/",
		APIKey:  "",
		Enabled: true,
	}}
	c.ApplyPatch(&Patch{Upstreams: &next})

	got := c.Snapshot().Upstreams[0]
	if got.APIKey != "old-api-key" {
		t.Fatalf("API key was not preserved: %+v", got)
	}
	if got.BaseURL != "https://new.example" {
		t.Fatalf("base URL was not trimmed/updated: %+v", got)
	}
}
