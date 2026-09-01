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
CREATE TABLE provider_api_keys (
 id INTEGER PRIMARY KEY, provider_id INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
 credential BLOB NOT NULL, fingerprint TEXT NOT NULL, key_hint TEXT NOT NULL, name TEXT NOT NULL DEFAULT '',
 model TEXT NOT NULL DEFAULT '', egress_mode TEXT NOT NULL DEFAULT 'inherit', ip_pool_node_id INTEGER,
 enabled INTEGER NOT NULL DEFAULT 1, sort_order INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL DEFAULT 'untested',
 last_error TEXT NOT NULL DEFAULT '', last_tested_at TEXT, last_test_latency_ms INTEGER NOT NULL DEFAULT 0,
 created_at TEXT NOT NULL, updated_at TEXT NOT NULL, UNIQUE(provider_id,fingerprint));
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
 cost_micros INTEGER NOT NULL DEFAULT 0, cost_type TEXT NOT NULL DEFAULT 'unknown');
CREATE TABLE route_policies (public_name TEXT PRIMARY KEY, strategy TEXT NOT NULL DEFAULT 'priority_failover', updated_at TEXT NOT NULL);`)
	if err != nil {
		t.Fatal(err)
	}
	stamp := "2026-07-22T00:00:00Z"
	if _, err := db.Exec(`INSERT INTO route_policies(public_name,strategy,updated_at) VALUES('legacy-model','priority_failover',?)`, stamp); err != nil {
		t.Fatal(err)
	}
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
		"providers":         {"passthrough_mode", "client_policy", "max_concurrency", "request_timeout_ms", "health_check_enabled", "failure_threshold", "cooldown_seconds", "consecutive_failures", "circuit_open_until", "last_latency_ms", "auth_kind", "auth_source", "auth_account_id", "auth_email", "auth_expires_at", "auth_last_refresh_at", "auth_status", "auth_fingerprint", "auth_has_refresh", "ip_pool_node_id", "sort_order", "archived"},
		"provider_api_keys": {"health_check_enabled"},
		"model_routes":      {"sort_order"},
		"api_keys":          {"encrypted_key"},
		"request_ledger":    {"gateway_request_id", "attempt", "retry_reason", "first_byte_ms", "usage_reported", "api_key_name", "api_key_prefix", "provider_name", "provider_key_id", "provider_key_name", "provider_key_hint", "client_ip"},
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
	var ipPoolTable string
	if err := a.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='ip_pool_nodes'`).Scan(&ipPoolTable); err != nil || ipPoolTable != "ip_pool_nodes" {
		t.Fatalf("ip_pool_nodes table was not migrated: table=%q err=%v", ipPoolTable, err)
	}
	var keyHealthTable string
	if err := a.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='provider_api_key_model_health'`).Scan(&keyHealthTable); err != nil || keyHealthTable != "provider_api_key_model_health" {
		t.Fatalf("provider_api_key_model_health table was not migrated: table=%q err=%v", keyHealthTable, err)
	}
	if _, err := a.db.Exec(`INSERT INTO provider_api_keys(provider_id,credential,fingerprint,key_hint,name,created_at,updated_at) VALUES(1,X'00','migration-key','hint','legacy-key',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	var keyLimitTrigger string
	if err := a.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='trigger' AND name='trg_provider_api_keys_limit'`).Scan(&keyLimitTrigger); err != nil || keyLimitTrigger != "trg_provider_api_keys_limit" {
		t.Fatalf("provider key limit trigger was not migrated: trigger=%q err=%v", keyLimitTrigger, err)
	}
	var keyHealthDefault int
	if err := a.db.QueryRow(`SELECT health_check_enabled FROM provider_api_keys WHERE fingerprint='migration-key'`).Scan(&keyHealthDefault); err != nil || keyHealthDefault != 1 {
		t.Fatalf("legacy API key health check default=%d err=%v", keyHealthDefault, err)
	}
	var aliasTable string
	if err := a.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='model_aliases'`).Scan(&aliasTable); err != nil || aliasTable != "model_aliases" {
		t.Fatalf("model_aliases table was not migrated: table=%q err=%v", aliasTable, err)
	}
	for _, table := range []string{"quality_detector_jobs", "quality_detector_job_items"} {
		var name string
		if err := a.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name); err != nil || name != table {
			t.Fatalf("%s table was not migrated: table=%q err=%v", table, name, err)
		}
	}
	if _, err := a.db.Exec(`INSERT INTO model_aliases(alias,target_model,enabled,created_at,updated_at) VALUES('legacy-alias','legacy-model',1,?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO model_routes(public_name,provider_id,upstream_model,created_at,updated_at) VALUES('legacy-alias',1,'conflict',?,?)`, stamp, stamp); err == nil {
		t.Fatal("route/alias conflict trigger was not installed")
	}
	var directCount int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM providers WHERE ip_pool_node_id IS NULL`).Scan(&directCount); err != nil || directCount != 1 {
		t.Fatalf("legacy provider did not remain in direct mode: count=%d err=%v", directCount, err)
	}
	var healthCheckEnabled int
	if err := a.db.QueryRow(`SELECT health_check_enabled FROM providers WHERE id=1`).Scan(&healthCheckEnabled); err != nil || healthCheckEnabled != 1 {
		t.Fatalf("legacy provider health check default=%d err=%v", healthCheckEnabled, err)
	}
	var policyTable string
	if err := a.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='route_policies'`).Scan(&policyTable); err != sql.ErrNoRows {
		t.Fatalf("route_policies table should be dropped by migration: table=%q err=%v", policyTable, err)
	}
	var authIndex string
	if err := a.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_provider_auth_fingerprint'`).Scan(&authIndex); err != nil {
		t.Fatalf("OAuth credential fingerprint index was not migrated: %v", err)
	}
	if authIndex != "idx_provider_auth_fingerprint" {
		t.Fatalf("OAuth credential fingerprint index = %q", authIndex)
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
	var providerOrder int64
	if err := a.db.QueryRow(`SELECT sort_order FROM providers WHERE id=1`).Scan(&providerOrder); err != nil || providerOrder != 1 {
		t.Fatalf("migrated provider order=%d err=%v, want 1", providerOrder, err)
	}
}
