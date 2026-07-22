package fusiongate

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestOldSchemaMigrationAddsReliabilityColumns(t *testing.T) {
	cfg := testConfig(t)
	db, err := sql.Open("sqlite3", filepath.Join(cfg.DataDir, "fusiongate.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE providers (
 id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, type TEXT NOT NULL, base_url TEXT NOT NULL,
 credential BLOB NOT NULL, enabled INTEGER NOT NULL DEFAULT 1, priority INTEGER NOT NULL DEFAULT 100,
 weight INTEGER NOT NULL DEFAULT 100, status TEXT NOT NULL DEFAULT 'unknown', notes TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE model_routes (
 id INTEGER PRIMARY KEY, public_name TEXT NOT NULL, provider_id INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
 upstream_model TEXT NOT NULL, capabilities TEXT NOT NULL DEFAULT 'chat,stream', enabled INTEGER NOT NULL DEFAULT 1,
 priority INTEGER NOT NULL DEFAULT 100, input_price_micros INTEGER NOT NULL DEFAULT 0, output_price_micros INTEGER NOT NULL DEFAULT 0,
 created_at TEXT NOT NULL, updated_at TEXT NOT NULL, UNIQUE(public_name,provider_id,upstream_model));
CREATE TABLE api_keys (
 id INTEGER PRIMARY KEY, name TEXT NOT NULL, key_prefix TEXT NOT NULL, key_hash TEXT NOT NULL UNIQUE,
 allow_all INTEGER NOT NULL DEFAULT 1, allow_models TEXT NOT NULL DEFAULT '', deny_models TEXT NOT NULL DEFAULT '',
 allow_images INTEGER NOT NULL DEFAULT 0, rpm_limit INTEGER NOT NULL DEFAULT 120, revoked INTEGER NOT NULL DEFAULT 0,
 expires_at TEXT, created_at TEXT NOT NULL, last_used_at TEXT);
CREATE TABLE request_ledger (
 id INTEGER PRIMARY KEY, request_id TEXT NOT NULL UNIQUE, created_at TEXT NOT NULL, completed_at TEXT,
 api_key_id INTEGER, provider_id INTEGER, route_id INTEGER, public_model TEXT NOT NULL, upstream_model TEXT NOT NULL,
 protocol TEXT NOT NULL, stream INTEGER NOT NULL DEFAULT 0, success INTEGER NOT NULL DEFAULT 0, status_code INTEGER NOT NULL DEFAULT 0,
 error_type TEXT NOT NULL DEFAULT '', latency_ms INTEGER NOT NULL DEFAULT 0, input_tokens INTEGER NOT NULL DEFAULT 0,
 output_tokens INTEGER NOT NULL DEFAULT 0, cached_tokens INTEGER NOT NULL DEFAULT 0, reasoning_tokens INTEGER NOT NULL DEFAULT 0,
 cost_micros INTEGER NOT NULL DEFAULT 0, cost_type TEXT NOT NULL DEFAULT 'unknown');`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for table, columns := range map[string][]string{
		"providers":      {"passthrough_mode", "client_policy", "max_concurrency", "request_timeout_ms", "failure_threshold", "cooldown_seconds", "consecutive_failures", "circuit_open_until", "last_latency_ms"},
		"request_ledger": {"gateway_request_id", "attempt", "retry_reason"},
	} {
		rows, err := a.db.Query("PRAGMA table_info(" + table + ")")
		if err != nil {
			t.Fatal(err)
		}
		found := map[string]bool{}
		for rows.Next() {
			var cid, notNull, pk int
			var name, columnType string
			var defaultValue any
			if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
				t.Fatal(err)
			}
			found[name] = true
		}
		_ = rows.Close()
		for _, column := range columns {
			if !found[column] {
				t.Errorf("%s.%s was not migrated", table, column)
			}
		}
	}
}
