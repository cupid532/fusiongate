package fusiongate

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Config struct {
	Addr, DataDir, MasterKey, AdminPassword       string
	AllowInsecureUpstreams, AllowPrivateUpstreams bool
}

type App struct {
	db                *sql.DB
	cfg               Config
	aead              cipher.AEAD
	client            *http.Client
	log               *slog.Logger
	mu                sync.Mutex
	rate              map[string]*rateWindow
	routeMu           sync.Mutex
	providerStates    map[int64]*providerRuntime
	roundRobinCursor  map[string]int
	ledgerCleanupMu   sync.Mutex
	lastLedgerCleanup time.Time
}
type rateWindow struct {
	At    time.Time
	Count int
}
type Provider struct {
	ID                  int64  `json:"id"`
	Name                string `json:"name"`
	Type                string `json:"type"`
	BaseURL             string `json:"base_url"`
	CredentialHint      string `json:"credential_hint"`
	Status              string `json:"status"`
	Notes               string `json:"notes"`
	Enabled             bool   `json:"enabled"`
	Priority            int    `json:"priority"`
	Weight              int    `json:"weight"`
	PassthroughMode     string `json:"passthrough_mode"`
	ClientPolicy        string `json:"client_policy"`
	MaxConcurrency      int    `json:"max_concurrency"`
	RequestTimeoutMS    int    `json:"request_timeout_ms"`
	FailureThreshold    int    `json:"failure_threshold"`
	CooldownSeconds     int    `json:"cooldown_seconds"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	CircuitOpenUntil    string `json:"circuit_open_until,omitempty"`
	LastError           string `json:"last_error,omitempty"`
	LastLatencyMS       int64  `json:"last_latency_ms"`
	LastSuccessAt       string `json:"last_success_at,omitempty"`
	LastFailureAt       string `json:"last_failure_at,omitempty"`
	Inflight            int    `json:"inflight"`
	ModelCount          int    `json:"model_count"`
}

type Route struct {
	ID                int64  `json:"id"`
	ProviderID        int64  `json:"provider_id"`
	PublicName        string `json:"public_name"`
	UpstreamModel     string `json:"upstream_model"`
	Capabilities      string `json:"capabilities"`
	Enabled           bool   `json:"enabled"`
	Priority          int    `json:"priority"`
	InputPriceMicros  int64  `json:"input_price_micros"`
	OutputPriceMicros int64  `json:"output_price_micros"`
	ProviderName      string `json:"provider_name,omitempty"`
	ProviderType      string `json:"provider_type,omitempty"`
	ProviderEnabled   bool   `json:"provider_enabled"`
	SortOrder         int    `json:"sort_order"`
	Strategy          string `json:"strategy,omitempty"`
	ProviderStatus    string `json:"provider_status,omitempty"`
	ProviderLatencyMS int64  `json:"provider_latency_ms"`
	ProviderFailures  int    `json:"provider_failures"`
	ProviderInflight  int    `json:"provider_inflight"`
	HealthScore       int    `json:"health_score"`
}

type APIKey struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	Prefix      string     `json:"prefix"`
	AllowModels string     `json:"allow_models"`
	DenyModels  string     `json:"deny_models"`
	AllowAll    bool       `json:"allow_all"`
	AllowImages bool       `json:"allow_images"`
	Revoked     bool       `json:"revoked"`
	RPMLimit    int        `json:"rpm_limit"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	CanReveal   bool       `json:"can_reveal"`
	Raw         string     `json:"key,omitempty"`
}

type Usage struct {
	Input, Output, Cached, Reasoning int64
	CostMicros                       int64
	CostType                         string
	Reported                         bool
}

func New(cfg Config) (*App, error) {
	if cfg.MasterKey == "" {
		return nil, errors.New("FUSIONGATE_MASTER_KEY is required (32 random bytes, base64 encoded)")
	}
	if cfg.AdminPassword == "" {
		return nil, errors.New("FUSIONGATE_ADMIN_PASSWORD is required on first and subsequent startup")
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "./data"
	}
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:8787"
	}
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(cfg.MasterKey)
	if err != nil || len(raw) != 32 {
		return nil, errors.New("FUSIONGATE_MASTER_KEY must be base64 encoded 32 bytes")
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", path.Join(cfg.DataDir, "fusiongate.db")+"?_busy_timeout=5000&_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	a := &App{db: db, cfg: cfg, aead: aead, client: newUpstreamHTTPClient(cfg), log: slog.New(slog.NewJSONHandler(os.Stdout, nil)), rate: map[string]*rateWindow{}, providerStates: map[int64]*providerRuntime{}, roundRobinCursor: map[string]int{}}
	if err := a.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err := a.pruneRequestLedger(context.Background(), true); err != nil {
		db.Close()
		return nil, err
	}
	if err := a.ensureAdmin(cfg.AdminPassword); err != nil {
		db.Close()
		return nil, err
	}
	return a, nil
}
func (a *App) Close() error { return a.db.Close() }

func (a *App) migrate(ctx context.Context) error {
	_, err := a.db.ExecContext(ctx, `
  CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
  CREATE TABLE IF NOT EXISTS providers (
    id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, type TEXT NOT NULL, base_url TEXT NOT NULL,
    credential BLOB NOT NULL, enabled INTEGER NOT NULL DEFAULT 1, priority INTEGER NOT NULL DEFAULT 1,
    weight INTEGER NOT NULL DEFAULT 100, status TEXT NOT NULL DEFAULT 'unknown', notes TEXT NOT NULL DEFAULT '',
    passthrough_mode TEXT NOT NULL DEFAULT 'normalized', client_policy TEXT NOT NULL DEFAULT 'any',
    max_concurrency INTEGER NOT NULL DEFAULT 0, request_timeout_ms INTEGER NOT NULL DEFAULT 120000,
    failure_threshold INTEGER NOT NULL DEFAULT 3, cooldown_seconds INTEGER NOT NULL DEFAULT 30,
    consecutive_failures INTEGER NOT NULL DEFAULT 0, circuit_open_until TEXT, last_error TEXT NOT NULL DEFAULT '',
    last_latency_ms INTEGER NOT NULL DEFAULT 0, last_success_at TEXT, last_failure_at TEXT,
    created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
  CREATE TABLE IF NOT EXISTS model_routes (
    id INTEGER PRIMARY KEY, public_name TEXT NOT NULL, provider_id INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    upstream_model TEXT NOT NULL, capabilities TEXT NOT NULL DEFAULT 'chat,stream', enabled INTEGER NOT NULL DEFAULT 1,
    priority INTEGER NOT NULL DEFAULT 0, sort_order INTEGER NOT NULL DEFAULT 0,
    input_price_micros INTEGER NOT NULL DEFAULT 0, output_price_micros INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL, updated_at TEXT NOT NULL, UNIQUE(public_name,provider_id,upstream_model));
  CREATE TABLE IF NOT EXISTS route_policies (
    public_name TEXT PRIMARY KEY, strategy TEXT NOT NULL DEFAULT 'priority_failover', updated_at TEXT NOT NULL);
  CREATE TABLE IF NOT EXISTS api_keys (
    id INTEGER PRIMARY KEY, name TEXT NOT NULL, key_prefix TEXT NOT NULL, key_hash TEXT NOT NULL UNIQUE,
    allow_all INTEGER NOT NULL DEFAULT 1, allow_models TEXT NOT NULL DEFAULT '', deny_models TEXT NOT NULL DEFAULT '',
    allow_images INTEGER NOT NULL DEFAULT 0, rpm_limit INTEGER NOT NULL DEFAULT 120, revoked INTEGER NOT NULL DEFAULT 0,
    expires_at TEXT, created_at TEXT NOT NULL, last_used_at TEXT, encrypted_key BLOB);
  CREATE TABLE IF NOT EXISTS request_ledger (
    id INTEGER PRIMARY KEY, request_id TEXT NOT NULL UNIQUE, created_at TEXT NOT NULL, completed_at TEXT,
    api_key_id INTEGER, provider_id INTEGER, route_id INTEGER, public_model TEXT NOT NULL, upstream_model TEXT NOT NULL,
    protocol TEXT NOT NULL, stream INTEGER NOT NULL DEFAULT 0, success INTEGER NOT NULL DEFAULT 0, status_code INTEGER NOT NULL DEFAULT 0,
    error_type TEXT NOT NULL DEFAULT '', latency_ms INTEGER NOT NULL DEFAULT 0, input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0, cached_tokens INTEGER NOT NULL DEFAULT 0, reasoning_tokens INTEGER NOT NULL DEFAULT 0,
    cost_micros INTEGER NOT NULL DEFAULT 0, cost_type TEXT NOT NULL DEFAULT 'unknown',
    gateway_request_id TEXT NOT NULL DEFAULT '', attempt INTEGER NOT NULL DEFAULT 1, retry_reason TEXT NOT NULL DEFAULT '',
    first_byte_ms INTEGER, usage_reported INTEGER NOT NULL DEFAULT 0,
    api_key_name TEXT NOT NULL DEFAULT '', api_key_prefix TEXT NOT NULL DEFAULT '', provider_name TEXT NOT NULL DEFAULT '');
  CREATE INDEX IF NOT EXISTS idx_ledger_created ON request_ledger(created_at DESC);
  CREATE INDEX IF NOT EXISTS idx_routes_public ON model_routes(public_name, enabled, priority);
  `)
	if err != nil {
		return err
	}
	if _, err := a.db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES('routing_strategy','priority_failover') ON CONFLICT(key) DO NOTHING`); err != nil {
		return err
	}
	hadSortOrder, err := hasColumn(ctx, a.db, "model_routes", "sort_order")
	if err != nil {
		return err
	}
	for _, column := range []struct{ table, name, ddl string }{
		{"providers", "passthrough_mode", "TEXT NOT NULL DEFAULT 'normalized'"},
		{"providers", "client_policy", "TEXT NOT NULL DEFAULT 'any'"},
		{"providers", "max_concurrency", "INTEGER NOT NULL DEFAULT 0"},
		{"providers", "request_timeout_ms", "INTEGER NOT NULL DEFAULT 120000"},
		{"providers", "failure_threshold", "INTEGER NOT NULL DEFAULT 3"},
		{"providers", "cooldown_seconds", "INTEGER NOT NULL DEFAULT 30"},
		{"providers", "consecutive_failures", "INTEGER NOT NULL DEFAULT 0"},
		{"providers", "circuit_open_until", "TEXT"},
		{"providers", "last_error", "TEXT NOT NULL DEFAULT ''"},
		{"providers", "last_latency_ms", "INTEGER NOT NULL DEFAULT 0"},
		{"providers", "last_success_at", "TEXT"},
		{"providers", "last_failure_at", "TEXT"},
		{"request_ledger", "gateway_request_id", "TEXT NOT NULL DEFAULT ''"},
		{"request_ledger", "attempt", "INTEGER NOT NULL DEFAULT 1"},
		{"request_ledger", "retry_reason", "TEXT NOT NULL DEFAULT ''"},
		{"request_ledger", "first_byte_ms", "INTEGER"},
		{"request_ledger", "usage_reported", "INTEGER NOT NULL DEFAULT 0"},
		{"request_ledger", "api_key_name", "TEXT NOT NULL DEFAULT ''"},
		{"request_ledger", "api_key_prefix", "TEXT NOT NULL DEFAULT ''"},
		{"request_ledger", "provider_name", "TEXT NOT NULL DEFAULT ''"},
		{"api_keys", "encrypted_key", "BLOB"},
		{"model_routes", "sort_order", "INTEGER NOT NULL DEFAULT 0"},
	} {
		if err := ensureColumn(ctx, a.db, column.table, column.name, column.ddl); err != nil {
			return err
		}
	}
	if !hadSortOrder {
		if _, err := a.db.ExecContext(ctx, `UPDATE model_routes SET sort_order=id`); err != nil {
			return err
		}
	}
	_, err = a.db.ExecContext(ctx, `
CREATE INDEX IF NOT EXISTS idx_ledger_gateway_request ON request_ledger(gateway_request_id, attempt);
CREATE INDEX IF NOT EXISTS idx_ledger_usage_dimensions ON request_ledger(created_at,api_key_id,provider_id,public_model);
CREATE INDEX IF NOT EXISTS idx_ledger_key_created ON request_ledger(api_key_id,created_at);
CREATE INDEX IF NOT EXISTS idx_ledger_provider_created ON request_ledger(provider_id,created_at);
CREATE INDEX IF NOT EXISTS idx_ledger_model_created ON request_ledger(public_model,created_at);
CREATE INDEX IF NOT EXISTS idx_routes_order ON model_routes(public_name, sort_order, id);
UPDATE request_ledger SET usage_reported=1 WHERE usage_reported=0 AND (input_tokens>0 OR output_tokens>0 OR cached_tokens>0 OR reasoning_tokens>0);`)
	return err
}

func hasColumn(ctx context.Context, db *sql.DB, table, name string) (bool, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var columnName, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &columnName, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if columnName == name {
			return true, nil
		}
	}
	return false, rows.Err()
}

func ensureColumn(ctx context.Context, db *sql.DB, table, name, ddl string) error {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid, notNull, pk int
		var columnName, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &columnName, &columnType, &notNull, &defaultValue, &pk); err != nil {
			rows.Close()
			return err
		}
		if columnName == name {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = db.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN "+name+" "+ddl)
	return err
}
func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}
func passwordHash(password string, salt []byte) string { // PBKDF2-HMAC-SHA256, 310k iterations
	hlen := 32
	out := make([]byte, hlen)
	prev := append(append([]byte{}, salt...), 0, 0, 0, 1)
	u := hmac.New(sha256.New, []byte(password))
	u.Write(prev)
	x := u.Sum(nil)
	copy(out, x)
	for i := 1; i < 310000; i++ {
		u = hmac.New(sha256.New, []byte(password))
		u.Write(x)
		x = u.Sum(nil)
		for j := range out {
			out[j] ^= x[j]
		}
	}
	return base64.RawStdEncoding.EncodeToString(salt) + ":" + base64.RawStdEncoding.EncodeToString(out)
}
func checkPassword(password, encoded string) bool {
	p := strings.Split(encoded, ":")
	if len(p) != 2 {
		return false
	}
	s, e := base64.RawStdEncoding.DecodeString(p[0])
	if e != nil {
		return false
	}
	want := passwordHash(password, s)
	return hmac.Equal([]byte(want), []byte(encoded))
}
func (a *App) ensureAdmin(password string) error {
	var h string
	err := a.db.QueryRow(`SELECT value FROM settings WHERE key='admin_password_hash'`).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = a.db.Exec(`INSERT INTO settings(key,value) VALUES('admin_password_hash',?)`, passwordHash(password, randomBytes(16)))
		return err
	}
	if err != nil {
		return err
	}
	if !checkPassword(password, h) {
		return errors.New("FUSIONGATE_ADMIN_PASSWORD does not match the configured administrator password")
	}
	return nil
}
func (a *App) encrypt(v string) ([]byte, error) {
	n := randomBytes(a.aead.NonceSize())
	return append(n, a.aead.Seal(nil, n, []byte(v), nil)...), nil
}
func (a *App) decrypt(v []byte) (string, error) {
	if len(v) < a.aead.NonceSize() {
		return "", errors.New("invalid encrypted credential")
	}
	p, e := a.aead.Open(nil, v[:a.aead.NonceSize()], v[a.aead.NonceSize():], nil)
	return string(p), e
}
func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
func parseTime(s string) *time.Time {
	if s == "" {
		return nil
	}
	v, e := time.Parse(time.RFC3339Nano, s)
	if e != nil {
		return nil
	}
	return &v
}
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
func strBool(i int) bool { return i != 0 }

func (a *App) Router() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", a.health)
	mux.HandleFunc("/", a.ui)
	mux.HandleFunc("/api/admin/login", a.login)
	mux.HandleFunc("/api/admin/logout", a.logout)
	mux.HandleFunc("/api/admin/session", a.admin(a.session))
	mux.HandleFunc("/api/admin/providers", a.admin(a.providers))
	mux.HandleFunc("/api/admin/providers/", a.admin(a.providerByID))
	mux.HandleFunc("/api/admin/routes", a.admin(a.routes))
	mux.HandleFunc("/api/admin/routes/", a.admin(a.routeByID))
	mux.HandleFunc("/api/admin/keys", a.admin(a.keys))
	mux.HandleFunc("/api/admin/keys/", a.admin(a.keyByID))
	mux.HandleFunc("/api/admin/dashboard", a.admin(a.dashboard))
	mux.HandleFunc("/api/admin/routing", a.admin(a.routing))
	mux.HandleFunc("/api/admin/requests", a.admin(a.requests))
	mux.HandleFunc("/api/admin/token-usage", a.admin(a.tokenUsage))
	mux.HandleFunc("/v1/models", a.api(a.models))
	mux.HandleFunc("/v1/chat/completions", a.api(a.chat))
	mux.HandleFunc("/v1/responses", a.api(a.responses))
	mux.HandleFunc("/v1/messages", a.api(a.messages))
	mux.HandleFunc("/v1/images/generations", a.api(a.images))
	return a.security(mux)
}
func (a *App) security(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func readJSON(r *http.Request, v any) error {
	d := json.NewDecoder(io.LimitReader(r.Body, 10<<20))
	d.DisallowUnknownFields()
	return d.Decode(v)
}
func fail(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": code, "code": code}})
}
