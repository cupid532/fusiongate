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
	stamp := "2026-07-22T00:00:00Z"
	if _, err := db.Exec(`INSERT INTO providers(id,name,type,base_url,credential,created_at,updated_at) VALUES(1,'legacy','openai_compatible','https://example.test',X'00',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO model_routes(id,public_name,provider_id,upstream_model,created_at,updated_at) VALUES(7,'legacy-model',1,'legacy-a',?,?),(12,'legacy-model',1,'legacy-b',?,?)`, stamp, stamp, stamp, stamp); err != nil {
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
		"model_routes":   {"sort_order"},
		"api_keys":       {"encrypted_key"},
		"request_ledger": {"gateway_request_id", "attempt", "retry_reason", "first_byte_ms"},
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
	var policyTable string
	if err := a.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='route_policies'`).Scan(&policyTable); err != nil {
		t.Fatalf("route_policies table was not migrated: %v", err)
	}
	if policyTable != "route_policies" {
		t.Fatalf("route_policies table = %q", policyTable)
	}
	rows, err := a.db.Query(`SELECT id,sort_order FROM model_routes ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := [][2]int64{{7, 7}, {12, 12}}
	var got [][2]int64
	for rows.Next() {
		var id, order int64
		if err := rows.Scan(&id, &order); err != nil {
			t.Fatal(err)
		}
		got = append(got, [2]int64{id, order})
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("migrated route order = %v, want %v", got, want)
	}
}
