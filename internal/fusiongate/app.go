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
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Config struct {
	Addr, DataDir, MasterKey, AdminPassword       string
	AllowInsecureUpstreams, AllowPrivateUpstreams bool
	MaxFailoverAttempts                           int
	MaxConcurrentRequests                         int
	StreamStartTimeout                            time.Duration
	StreamIdleTimeout                             time.Duration
	CORSOrigins                                   string
	QualityDetectorURL, QualityDetectorBaseURL    string
}

const DefaultStreamStartTimeout = 30 * time.Second

type App struct {
	db                       *sql.DB
	readDB                   *sql.DB
	cfg                      Config
	aead                     cipher.AEAD
	client                   *http.Client
	qualityDetectorClient    *qualityDetectorClient
	qualityDetectorControlMu sync.Mutex
	qualityDetectorMu        sync.Mutex
	qualityDetectorRoutes    map[string]*qualityDetectorRouteSession
	qualityDetectorActive    string
	qualityDetectorLast      qualityDetectorTarget
	log                      *slog.Logger
	mu                       sync.Mutex
	rate                     map[string]*rateWindow
	routeMu                  sync.Mutex
	providerStates           map[int64]*providerRuntime
	providerKeyCooldowns     map[int64]time.Time
	roundRobinCursor         map[string]int
	smoothWeights            map[string]map[int64]float64
	ledgerMu                 sync.RWMutex
	ledgerWrites             chan ledgerWrite
	ledgerWriterDone         chan struct{}
	ledgerClosed             bool
	authMu                   sync.Mutex
	refreshMu                sync.Mutex
	oauthSessions            map[string]oauthSession
	authImports              map[string]credentialImportSession
	ledgerCleanupMu          sync.Mutex
	lastLedgerCleanup        time.Time
	healthChecker            *HealthChecker
	healthCheckJobs          *healthCheckJobManager
	healthProbeMu            sync.Mutex
	healthProbes             map[int64]struct{}
	balanceMu                sync.Mutex
	balanceCache             map[int64]ProviderUpstreamBalance
	loginMu                  sync.Mutex
	loginAttempts            map[string]*rateWindow
	loginVerifiers           chan struct{}
	sessionMu                sync.Mutex
	adminSessions            map[string]adminSession
	ready                    atomic.Bool
	pricingSyncMu            sync.Mutex
	pricingSyncTrigger       chan struct{}
	ipPool                   *ipPoolManager
	requestSlots             chan struct{}
	lastUsedMu               sync.Mutex
	lastUsedAt               map[int64]time.Time
	metrics                  gatewayMetrics
}
type rateWindow struct {
	At    time.Time
	Count int
	// Prev holds the previous window's count so the API key limiter can weight it
	// into a sliding estimate. The login limiter only uses At and Count.
	Prev int
}
type Provider struct {
	ID                      int64   `json:"id"`
	Name                    string  `json:"name"`
	Type                    string  `json:"type"`
	BaseURL                 string  `json:"base_url"`
	CredentialHint          string  `json:"credential_hint"`
	AuthKind                string  `json:"auth_kind"`
	AuthSource              string  `json:"auth_source"`
	AuthEmail               string  `json:"auth_email,omitempty"`
	AuthAccountID           string  `json:"auth_account_id,omitempty"`
	AuthExpiresAt           string  `json:"auth_expires_at,omitempty"`
	AuthStatus              string  `json:"auth_status"`
	HasRefreshToken         bool    `json:"has_refresh_token"`
	Status                  string  `json:"status"`
	Notes                   string  `json:"notes"`
	Enabled                 bool    `json:"enabled"`
	Archived                bool    `json:"archived"`
	Priority                int     `json:"priority"`
	SortOrder               int     `json:"sort_order"`
	Weight                  int     `json:"weight"`
	PassthroughMode         string  `json:"passthrough_mode"`
	ClientPolicy            string  `json:"client_policy"`
	HealthCheckEnabled      bool    `json:"health_check_enabled"`
	MaxConcurrency          int     `json:"max_concurrency"`
	RequestTimeoutMS        int     `json:"request_timeout_ms"`
	FailureThreshold        int     `json:"failure_threshold"`
	CooldownSeconds         int     `json:"cooldown_seconds"`
	ConsecutiveFailures     int     `json:"consecutive_failures"`
	CircuitOpenUntil        string  `json:"circuit_open_until,omitempty"`
	LastError               string  `json:"last_error,omitempty"`
	LastLatencyMS           int64   `json:"last_latency_ms"`
	LastFirstByteMS         int64   `json:"last_first_byte_ms"`
	LastSuccessAt           string  `json:"last_success_at,omitempty"`
	LastFailureAt           string  `json:"last_failure_at,omitempty"`
	Inflight                int     `json:"inflight"`
	ModelCount              int     `json:"model_count"`
	GroupID                 *int64  `json:"group_id,omitempty"`
	GroupSortOrder          int     `json:"group_sort_order"`
	LastHealthCheckAt       string  `json:"last_health_check_at,omitempty"`
	HealthCheckStatus       string  `json:"health_check_status"`
	HealthCheckError        string  `json:"health_check_error,omitempty"`
	HealthCheckLatencyMS    int64   `json:"health_check_latency_ms"`
	HealthCheckMode         string  `json:"health_check_mode"`
	HealthCheckFirstByteMS  int64   `json:"health_check_first_byte_ms"`
	HealthCheckModel        string  `json:"health_check_model,omitempty"`
	HealthCheckModelCount   int     `json:"health_check_model_count"`
	HealthScore             int     `json:"health_score"`
	ManualBalanceMicros     *int64  `json:"manual_balance_micros,omitempty"`
	BalanceBaselineAt       string  `json:"balance_baseline_at,omitempty"`
	BalanceMultiplierOpenAI float64 `json:"balance_multiplier_openai"`
	BalanceMultiplierClaude float64 `json:"balance_multiplier_claude"`
	BalanceMultiplierGrok   float64 `json:"balance_multiplier_grok"`
	BalanceMultiplierGemini float64 `json:"balance_multiplier_gemini"`
	BalanceMultiplierOther  float64 `json:"balance_multiplier_other"`
	IPPoolNodeID            *int64  `json:"ip_pool_node_id,omitempty"`
	IPPoolNodeName          string  `json:"ip_pool_node_name,omitempty"`
	IPPoolNodeProtocol      string  `json:"ip_pool_node_protocol,omitempty"`
	DefaultModel            string  `json:"default_model,omitempty"`
	APIKeyCount             int     `json:"api_key_count"`
	EnabledAPIKeyCount      int     `json:"enabled_api_key_count"`
}

type ProviderGroup struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Collapsed    bool   `json:"collapsed"`
	SortOrder    int    `json:"sort_order"`
	MemberCount  int    `json:"member_count"`
	HealthyCount int    `json:"healthy_count"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

type Route struct {
	ID                     int64  `json:"id"`
	ProviderID             int64  `json:"provider_id"`
	PublicName             string `json:"public_name"`
	UpstreamModel          string `json:"upstream_model"`
	Capabilities           string `json:"capabilities"`
	Enabled                bool   `json:"enabled"`
	Priority               int    `json:"priority"`
	InputPriceMicros       int64  `json:"input_price_micros"`
	CachedPriceMicros      int64  `json:"cached_price_micros"`
	OutputPriceMicros      int64  `json:"output_price_micros"`
	LongContextThreshold   int64  `json:"long_context_threshold"`
	LongInputPriceMicros   int64  `json:"long_input_price_micros"`
	LongCachedPriceMicros  int64  `json:"long_cached_price_micros"`
	LongOutputPriceMicros  int64  `json:"long_output_price_micros"`
	PricingSource          string `json:"pricing_source,omitempty"`
	PricingUpdatedAt       string `json:"pricing_updated_at,omitempty"`
	ProviderName           string `json:"provider_name,omitempty"`
	ProviderType           string `json:"provider_type,omitempty"`
	ProviderEnabled        bool   `json:"provider_enabled"`
	SortOrder              int    `json:"sort_order"`
	ProviderStatus         string `json:"provider_status,omitempty"`
	ProviderLatencyMS      int64  `json:"provider_latency_ms"`
	ProviderFirstByteMS    int64  `json:"provider_first_byte_ms"`
	ProviderFailures       int    `json:"provider_failures"`
	ProviderInflight       int    `json:"provider_inflight"`
	HealthScore            int    `json:"health_score"`
	LastHealthCheckAt      string `json:"last_health_check_at,omitempty"`
	HealthCheckStatus      string `json:"health_check_status"`
	HealthCheckError       string `json:"health_check_error,omitempty"`
	HealthCheckLatencyMS   int64  `json:"health_check_latency_ms"`
	HealthCheckFirstByteMS int64  `json:"health_check_first_byte_ms"`
}

type APIKey struct {
	ID              int64      `json:"id"`
	Name            string     `json:"name"`
	Prefix          string     `json:"prefix"`
	AllowModels     string     `json:"allow_models"`
	DenyModels      string     `json:"deny_models"`
	AllowAll        bool       `json:"allow_all"`
	AllowImages     bool       `json:"allow_images"`
	Revoked         bool       `json:"revoked"`
	RPMLimit        int        `json:"rpm_limit"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	CanReveal       bool       `json:"can_reveal"`
	BudgetMicros    int64      `json:"budget_micros"`
	SpentMicros     int64      `json:"spent_micros"`
	RemainingMicros int64      `json:"remaining_micros"`
	Raw             string     `json:"key,omitempty"`
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
	if cfg.MaxFailoverAttempts <= 0 {
		cfg.MaxFailoverAttempts = 8
	}
	if cfg.MaxConcurrentRequests <= 0 {
		cfg.MaxConcurrentRequests = 64
	}
	if cfg.StreamStartTimeout <= 0 {
		cfg.StreamStartTimeout = DefaultStreamStartTimeout
	}
	if cfg.StreamIdleTimeout <= 0 {
		cfg.StreamIdleTimeout = 5 * time.Minute
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
	dbPath := path.Join(cfg.DataDir, "fusiongate.db")
	// A single writer connection keeps SQLite free of write contention. synchronous
	//=NORMAL is the standard WAL setting: a commit still survives a process crash,
	// only a host power loss can lose the most recent transactions, and it removes an
	// fsync from every write on the request path.
	db, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000&_journal_mode=WAL&_foreign_keys=on&_synchronous=NORMAL")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	a := &App{
		db: db, cfg: cfg, aead: aead, client: newUpstreamHTTPClient(cfg),
		log:  slog.New(slog.NewJSONHandler(os.Stdout, nil)),
		rate: map[string]*rateWindow{}, providerStates: map[int64]*providerRuntime{},
		providerKeyCooldowns: map[int64]time.Time{}, roundRobinCursor: map[string]int{},
		smoothWeights: map[string]map[int64]float64{},
		oauthSessions: map[string]oauthSession{}, authImports: map[string]credentialImportSession{},
		healthProbes: map[int64]struct{}{}, balanceCache: map[int64]ProviderUpstreamBalance{},
		loginAttempts: map[string]*rateWindow{}, loginVerifiers: make(chan struct{}, 4),
		adminSessions:         map[string]adminSession{},
		qualityDetectorRoutes: map[string]*qualityDetectorRouteSession{},
		pricingSyncTrigger:    make(chan struct{}, 1), requestSlots: make(chan struct{}, cfg.MaxConcurrentRequests),
		lastUsedAt: map[int64]time.Time{}, metrics: newGatewayMetrics(),
	}
	if strings.TrimSpace(cfg.QualityDetectorURL) != "" {
		a.qualityDetectorClient, err = newQualityDetectorClient(cfg.QualityDetectorURL)
		if err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := a.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	// WAL lets readers run concurrently with the writer, so reads get their own pool.
	// Without it every dashboard aggregate over a year of request ledger would queue
	// behind live gateway traffic on the one writer connection.
	readDB, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000&_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		db.Close()
		return nil, err
	}
	readDB.SetMaxOpenConns(readPoolSize())
	readDB.SetMaxIdleConns(readPoolSize())
	a.readDB = readDB
	a.startLedgerWriter()
	if err := a.pruneRequestLedger(context.Background(), true); err != nil {
		a.closeDatabases()
		return nil, err
	}
	if err := a.ensureAdmin(cfg.AdminPassword); err != nil {
		a.closeDatabases()
		return nil, err
	}
	a.ipPool = newIPPoolManager(a)
	if err := a.reconcileIPPool(context.Background()); err != nil {
		a.log.Warn("IP pool starts degraded", "error", err)
	}
	a.healthChecker = NewHealthChecker(a, healthCheckIntervalFromEnv(), healthCheckConcurrencyFromEnv())
	a.healthCheckJobs = newHealthCheckJobManager(a)
	for _, file := range []string{"fusiongate.db", "fusiongate.db-wal", "fusiongate.db-shm"} {
		if err := os.Chmod(path.Join(cfg.DataDir, file), 0600); err != nil && !errors.Is(err, os.ErrNotExist) {
			db.Close()
			return nil, err
		}
	}
	a.ready.Store(true)
	return a, nil
}

// reader returns the read-only pool. Every plain SELECT should use it so that
// dashboard aggregates and gateway lookups run concurrently instead of queueing
// behind the single writer connection. Transactions and writes keep using a.db.
func (a *App) reader() *sql.DB {
	if a.readDB != nil {
		return a.readDB
	}
	return a.db
}

// ledgerWrite is one queued request-ledger statement.
type ledgerWrite struct {
	query string
	args  []any
	done  chan struct{}
}

// ledgerWriteQueueSize bounds how many ledger statements may wait to be applied.
const ledgerWriteQueueSize = 4096

func readPoolSize() int {
	size := runtime.NumCPU()
	if size < 4 {
		size = 4
	}
	if size > 16 {
		size = 16
	}
	return size
}

func (a *App) startLedgerWriter() {
	a.ledgerWrites = make(chan ledgerWrite, ledgerWriteQueueSize)
	a.ledgerWriterDone = make(chan struct{})
	go a.runLedgerWriter()
}

// runLedgerWriter applies queued ledger statements in FIFO order. Order matters:
// an attempt's INSERT has to land before the UPDATEs that address it by request_id.
func (a *App) runLedgerWriter() {
	defer close(a.ledgerWriterDone)
	for write := range a.ledgerWrites {
		if write.query != "" {
			if _, err := a.db.Exec(write.query, write.args...); err != nil {
				a.metrics.ledgerWriteErrors.Add(1)
				a.log.Error("request ledger write", "error", err)
			}
		}
		if write.done != nil {
			close(write.done)
		}
	}
}

// queueLedgerWrite hands one ledger statement to the writer goroutine. The request
// ledger is observability data that sits on the response hot path, so no caller
// should wait for SQLite to accept it. A full queue blocks instead of dropping,
// because a dropped row would also drop the cost it carries.
func (a *App) queueLedgerWrite(query string, args ...any) {
	a.ledgerMu.RLock()
	if a.ledgerWrites == nil || a.ledgerClosed {
		a.ledgerMu.RUnlock()
		if _, err := a.db.Exec(query, args...); err != nil {
			a.metrics.ledgerWriteErrors.Add(1)
			a.log.Error("request ledger write", "error", err)
		}
		return
	}
	defer a.ledgerMu.RUnlock()
	a.metrics.ledgerQueued.Add(1)
	select {
	case a.ledgerWrites <- ledgerWrite{query: query, args: args}:
	default:
		a.metrics.ledgerQueueWaits.Add(1)
		a.ledgerWrites <- ledgerWrite{query: query, args: args}
	}
}

// flushLedgerWrites blocks until every statement queued so far has been applied.
// Tests and shutdown use it to read the ledger without racing the writer.
func (a *App) flushLedgerWrites() {
	a.ledgerMu.RLock()
	queue, closed := a.ledgerWrites, a.ledgerClosed
	if queue == nil || closed {
		a.ledgerMu.RUnlock()
		return
	}
	done := make(chan struct{})
	queue <- ledgerWrite{done: done}
	a.ledgerMu.RUnlock()
	<-done
}

func (a *App) closeDatabases() error {
	a.ledgerMu.Lock()
	queue := a.ledgerWrites
	if queue != nil && !a.ledgerClosed {
		a.ledgerClosed = true
		close(queue)
	}
	a.ledgerMu.Unlock()
	if a.ledgerWriterDone != nil {
		<-a.ledgerWriterDone
	}
	if a.readDB != nil {
		if err := a.readDB.Close(); err != nil {
			a.log.Error("read pool close", "error", err)
		}
	}
	return a.db.Close()
}

func (a *App) Close() error {
	a.ready.Store(false)
	if a.healthChecker != nil {
		a.healthChecker.Stop()
	}
	if a.healthCheckJobs != nil {
		a.healthCheckJobs.Close()
	}
	if a.ipPool != nil {
		a.ipPool.Close()
	}
	return a.closeDatabases()
}

func (a *App) BeginShutdown() { a.ready.Store(false) }

// runLedgerRetentionLoop trims expired ledger rows on a timer. Retention used to be
// re-checked on every gateway request, which put a DELETE scan behind the request
// path even though the cutoff only moves once a day.
func (a *App) runLedgerRetentionLoop(ctx context.Context) {
	ticker := time.NewTicker(requestLedgerCleanupEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.pruneRequestLedger(ctx, false); err != nil {
				a.log.Error("request ledger retention cleanup", "error", err)
			}
		}
	}
}

// StartBackgroundTasks starts the health checker and other periodic background
// jobs. The caller supplies a context that, when canceled, stops all tasks.
func (a *App) StartBackgroundTasks(ctx context.Context) {
	if a.healthChecker != nil {
		a.healthChecker.Start(ctx)
	}
	go a.runOAuthRefreshLoop(ctx)
	go a.runPricingSyncLoop(ctx)
	go a.runLedgerRetentionLoop(ctx)
	// Circuit recovery is performed by the next real request after cooldown.
}

func healthCheckIntervalFromEnv() time.Duration {
	v := strings.TrimSpace(os.Getenv("FUSIONGATE_HEALTH_CHECK_INTERVAL"))
	if v == "" {
		return 15 * time.Minute
	}
	if v == "0" || strings.EqualFold(v, "off") || strings.EqualFold(v, "false") {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 15 * time.Minute
	}
	return d
}

func healthCheckConcurrencyFromEnv() int {
	v := strings.TrimSpace(os.Getenv("FUSIONGATE_HEALTH_CHECK_CONCURRENCY"))
	if v == "" {
		return 5
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 5
	}
	if n > 20 {
		n = 20
	}
	return n
}

func (a *App) migrate(ctx context.Context) error {
	var schemaVersion int
	if err := a.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&schemaVersion); err != nil {
		return err
	}
	if schemaVersion > 1 {
		return fmt.Errorf("database schema version %d is newer than supported version 1", schemaVersion)
	}
	_, err := a.db.ExecContext(ctx, `
  CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
  CREATE TABLE IF NOT EXISTS ip_pool_nodes (
    id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, protocol TEXT NOT NULL, server TEXT NOT NULL,
    share_link BLOB NOT NULL, enabled INTEGER NOT NULL DEFAULT 1, local_port INTEGER NOT NULL UNIQUE,
    status TEXT NOT NULL DEFAULT 'pending', last_error TEXT NOT NULL DEFAULT '', last_checked_at TEXT,
    last_latency_ms INTEGER NOT NULL DEFAULT 0, exit_ip TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
  CREATE TABLE IF NOT EXISTS providers (
    id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, type TEXT NOT NULL, base_url TEXT NOT NULL,
     credential BLOB NOT NULL, enabled INTEGER NOT NULL DEFAULT 1, archived INTEGER NOT NULL DEFAULT 0, priority INTEGER NOT NULL DEFAULT 1,
    sort_order INTEGER NOT NULL DEFAULT 0,
    weight INTEGER NOT NULL DEFAULT 100, status TEXT NOT NULL DEFAULT 'unknown', notes TEXT NOT NULL DEFAULT '',
    passthrough_mode TEXT NOT NULL DEFAULT 'normalized', client_policy TEXT NOT NULL DEFAULT 'any',
    max_concurrency INTEGER NOT NULL DEFAULT 0, request_timeout_ms INTEGER NOT NULL DEFAULT 120000,
    health_check_enabled INTEGER NOT NULL DEFAULT 1,
    failure_threshold INTEGER NOT NULL DEFAULT 3, cooldown_seconds INTEGER NOT NULL DEFAULT 30,
    consecutive_failures INTEGER NOT NULL DEFAULT 0, circuit_open_until TEXT, last_error TEXT NOT NULL DEFAULT '',
    last_latency_ms INTEGER NOT NULL DEFAULT 0, last_success_at TEXT, last_failure_at TEXT,
    auth_kind TEXT NOT NULL DEFAULT 'api_key', auth_source TEXT NOT NULL DEFAULT 'manual',
    auth_account_id TEXT NOT NULL DEFAULT '', auth_email TEXT NOT NULL DEFAULT '', auth_expires_at TEXT,
    auth_last_refresh_at TEXT, auth_status TEXT NOT NULL DEFAULT 'ready', auth_fingerprint TEXT NOT NULL DEFAULT '',
    auth_has_refresh INTEGER NOT NULL DEFAULT 0,
    default_model TEXT NOT NULL DEFAULT '', multi_key_initialized INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
  CREATE TABLE IF NOT EXISTS provider_api_keys (
    id INTEGER PRIMARY KEY, provider_id INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    credential BLOB NOT NULL, fingerprint TEXT NOT NULL, key_hint TEXT NOT NULL, name TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '', egress_mode TEXT NOT NULL DEFAULT 'inherit',
    ip_pool_node_id INTEGER REFERENCES ip_pool_nodes(id) ON DELETE RESTRICT,
    enabled INTEGER NOT NULL DEFAULT 1, sort_order INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'untested', last_error TEXT NOT NULL DEFAULT '', last_tested_at TEXT,
    last_test_latency_ms INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
    UNIQUE(provider_id,fingerprint));
  CREATE TABLE IF NOT EXISTS provider_api_key_models (
    provider_key_id INTEGER NOT NULL REFERENCES provider_api_keys(id) ON DELETE CASCADE,
    model TEXT NOT NULL, display_name TEXT NOT NULL DEFAULT '', capabilities TEXT NOT NULL DEFAULT 'chat,stream',
    discovered_at TEXT NOT NULL, PRIMARY KEY(provider_key_id,model));
  CREATE TABLE IF NOT EXISTS model_routes (
    id INTEGER PRIMARY KEY, public_name TEXT NOT NULL, provider_id INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    upstream_model TEXT NOT NULL, capabilities TEXT NOT NULL DEFAULT 'chat,stream', enabled INTEGER NOT NULL DEFAULT 1,
    priority INTEGER NOT NULL DEFAULT 0, sort_order INTEGER NOT NULL DEFAULT 0,
    input_price_micros INTEGER NOT NULL DEFAULT 0, output_price_micros INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL, updated_at TEXT NOT NULL, UNIQUE(public_name,provider_id,upstream_model));
	CREATE TABLE IF NOT EXISTS model_route_exclusions (
	  provider_id INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
	  public_name TEXT NOT NULL, upstream_model TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL,
	  PRIMARY KEY(provider_id,public_name));
	  CREATE TABLE IF NOT EXISTS api_keys (
    id INTEGER PRIMARY KEY, name TEXT NOT NULL, key_prefix TEXT NOT NULL, key_hash TEXT NOT NULL UNIQUE,
    allow_all INTEGER NOT NULL DEFAULT 1, allow_models TEXT NOT NULL DEFAULT '', deny_models TEXT NOT NULL DEFAULT '',
    allow_images INTEGER NOT NULL DEFAULT 0, rpm_limit INTEGER NOT NULL DEFAULT 120, revoked INTEGER NOT NULL DEFAULT 0,
	    expires_at TEXT, budget_micros INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, last_used_at TEXT, encrypted_key BLOB);
  CREATE TABLE IF NOT EXISTS request_ledger (
    id INTEGER PRIMARY KEY, request_id TEXT NOT NULL UNIQUE, created_at TEXT NOT NULL, completed_at TEXT,
    api_key_id INTEGER, provider_id INTEGER, route_id INTEGER, public_model TEXT NOT NULL, upstream_model TEXT NOT NULL,
    protocol TEXT NOT NULL, stream INTEGER NOT NULL DEFAULT 0, success INTEGER NOT NULL DEFAULT 0, status_code INTEGER NOT NULL DEFAULT 0,
    error_type TEXT NOT NULL DEFAULT '', latency_ms INTEGER NOT NULL DEFAULT 0, input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0, cached_tokens INTEGER NOT NULL DEFAULT 0, reasoning_tokens INTEGER NOT NULL DEFAULT 0,
    cost_micros INTEGER NOT NULL DEFAULT 0, cost_type TEXT NOT NULL DEFAULT 'unknown',
    gateway_request_id TEXT NOT NULL DEFAULT '', attempt INTEGER NOT NULL DEFAULT 1, retry_reason TEXT NOT NULL DEFAULT '',
    first_byte_ms INTEGER, usage_reported INTEGER NOT NULL DEFAULT 0, client_ip TEXT NOT NULL DEFAULT '',
    api_key_name TEXT NOT NULL DEFAULT '', api_key_prefix TEXT NOT NULL DEFAULT '', provider_name TEXT NOT NULL DEFAULT '');
  CREATE INDEX IF NOT EXISTS idx_ledger_created ON request_ledger(created_at DESC);
  CREATE INDEX IF NOT EXISTS idx_routes_public ON model_routes(public_name, enabled, priority);
	CREATE INDEX IF NOT EXISTS idx_model_route_exclusions_public ON model_route_exclusions(public_name);
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
	hadProviderSortOrder, err := hasColumn(ctx, a.db, "providers", "sort_order")
	if err != nil {
		return err
	}
	hadKeySpend, err := hasColumn(ctx, a.db, "api_keys", "spent_micros")
	if err != nil {
		return err
	}
	for _, column := range []struct{ table, name, ddl string }{
		{"providers", "passthrough_mode", "TEXT NOT NULL DEFAULT 'normalized'"},
		{"providers", "client_policy", "TEXT NOT NULL DEFAULT 'any'"},
		{"providers", "max_concurrency", "INTEGER NOT NULL DEFAULT 0"},
		{"providers", "health_check_enabled", "INTEGER NOT NULL DEFAULT 1"},
		{"providers", "request_timeout_ms", "INTEGER NOT NULL DEFAULT 120000"},
		{"providers", "failure_threshold", "INTEGER NOT NULL DEFAULT 3"},
		{"providers", "cooldown_seconds", "INTEGER NOT NULL DEFAULT 30"},
		{"providers", "consecutive_failures", "INTEGER NOT NULL DEFAULT 0"},
		{"providers", "circuit_open_until", "TEXT"},
		{"providers", "last_error", "TEXT NOT NULL DEFAULT ''"},
		{"providers", "last_latency_ms", "INTEGER NOT NULL DEFAULT 0"},
		{"providers", "last_first_byte_ms", "INTEGER NOT NULL DEFAULT 0"},
		{"providers", "last_success_at", "TEXT"},
		{"providers", "last_failure_at", "TEXT"},
		{"providers", "auth_kind", "TEXT NOT NULL DEFAULT 'api_key'"},
		{"providers", "auth_source", "TEXT NOT NULL DEFAULT 'manual'"},
		{"providers", "auth_account_id", "TEXT NOT NULL DEFAULT ''"},
		{"providers", "auth_email", "TEXT NOT NULL DEFAULT ''"},
		{"providers", "auth_expires_at", "TEXT"},
		{"providers", "auth_last_refresh_at", "TEXT"},
		{"providers", "auth_status", "TEXT NOT NULL DEFAULT 'ready'"},
		{"providers", "auth_fingerprint", "TEXT NOT NULL DEFAULT ''"},
		{"providers", "auth_has_refresh", "INTEGER NOT NULL DEFAULT 0"},
		{"providers", "manual_balance_micros", "INTEGER"},
		{"providers", "balance_baseline_at", "TEXT"},
		{"providers", "balance_multiplier_openai", "REAL NOT NULL DEFAULT 1"},
		{"providers", "balance_multiplier_claude", "REAL NOT NULL DEFAULT 1"},
		{"providers", "balance_multiplier_grok", "REAL NOT NULL DEFAULT 1"},
		{"providers", "balance_multiplier_gemini", "REAL NOT NULL DEFAULT 1"},
		{"providers", "balance_multiplier_other", "REAL NOT NULL DEFAULT 1"},
		{"providers", "ip_pool_node_id", "INTEGER REFERENCES ip_pool_nodes(id) ON DELETE SET NULL"},
		{"providers", "default_model", "TEXT NOT NULL DEFAULT ''"},
		{"providers", "multi_key_initialized", "INTEGER NOT NULL DEFAULT 0"},
		{"providers", "sort_order", "INTEGER NOT NULL DEFAULT 0"},
		{"providers", "archived", "INTEGER NOT NULL DEFAULT 0"},
		{"provider_api_key_models", "enabled", "INTEGER NOT NULL DEFAULT 1"},
		{"request_ledger", "gateway_request_id", "TEXT NOT NULL DEFAULT ''"},
		{"request_ledger", "attempt", "INTEGER NOT NULL DEFAULT 1"},
		{"request_ledger", "retry_reason", "TEXT NOT NULL DEFAULT ''"},
		{"request_ledger", "first_byte_ms", "INTEGER"},
		{"request_ledger", "usage_reported", "INTEGER NOT NULL DEFAULT 0"},
		{"request_ledger", "api_key_name", "TEXT NOT NULL DEFAULT ''"},
		{"request_ledger", "api_key_prefix", "TEXT NOT NULL DEFAULT ''"},
		{"request_ledger", "provider_name", "TEXT NOT NULL DEFAULT ''"},
		{"request_ledger", "reasoning_effort", "TEXT NOT NULL DEFAULT ''"},
		{"request_ledger", "client_ip", "TEXT NOT NULL DEFAULT ''"},
		{"api_keys", "encrypted_key", "BLOB"},
		{"api_keys", "budget_micros", "INTEGER NOT NULL DEFAULT 0"},
		{"api_keys", "spent_micros", "INTEGER NOT NULL DEFAULT 0"},
		{"model_routes", "sort_order", "INTEGER NOT NULL DEFAULT 0"},
		{"model_routes", "cached_price_micros", "INTEGER NOT NULL DEFAULT 0"},
		{"model_routes", "long_context_threshold", "INTEGER NOT NULL DEFAULT 0"},
		{"model_routes", "long_input_price_micros", "INTEGER NOT NULL DEFAULT 0"},
		{"model_routes", "long_cached_price_micros", "INTEGER NOT NULL DEFAULT 0"},
		{"model_routes", "long_output_price_micros", "INTEGER NOT NULL DEFAULT 0"},
		{"model_routes", "pricing_source", "TEXT NOT NULL DEFAULT ''"},
		{"model_routes", "pricing_updated_at", "TEXT"},
		{"model_routes", "last_health_check_at", "TEXT"},
		{"model_routes", "health_check_status", "TEXT NOT NULL DEFAULT 'pending'"},
		{"model_routes", "health_check_error", "TEXT NOT NULL DEFAULT ''"},
		{"model_routes", "health_check_latency_ms", "INTEGER NOT NULL DEFAULT 0"},
		{"model_routes", "health_check_first_byte_ms", "INTEGER NOT NULL DEFAULT 0"},
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
	if !hadProviderSortOrder {
		if _, err := a.db.ExecContext(ctx, `UPDATE providers SET sort_order=id`); err != nil {
			return err
		}
	}
	if !hadKeySpend {
		// Seed the running total once from the ledger. From here on it is maintained
		// incrementally, so retention pruning can no longer hand budget back.
		if _, err := a.db.ExecContext(ctx, `UPDATE api_keys SET spent_micros=COALESCE((SELECT SUM(cost_micros) FROM request_ledger WHERE api_key_id=api_keys.id AND completed_at IS NOT NULL),0)`); err != nil {
			return err
		}
	}
	// Older builds permanently disabled a provider after five failures. Those
	// rows are distinguishable from an administrator toggle by their status.
	if _, err := a.db.ExecContext(ctx, `UPDATE providers SET enabled=1,status='circuit_open',circuit_open_until=COALESCE(circuit_open_until,?),updated_at=? WHERE enabled=0 AND status='disabled' AND consecutive_failures>=5`, now(), now()); err != nil {
		return err
	}
	_, err = a.db.ExecContext(ctx, `
CREATE INDEX IF NOT EXISTS idx_ledger_gateway_request ON request_ledger(gateway_request_id, attempt);
CREATE INDEX IF NOT EXISTS idx_ledger_usage_dimensions ON request_ledger(created_at,api_key_id,provider_id,public_model);
CREATE INDEX IF NOT EXISTS idx_ledger_key_created ON request_ledger(api_key_id,created_at);
CREATE INDEX IF NOT EXISTS idx_ledger_provider_created ON request_ledger(provider_id,created_at);
CREATE INDEX IF NOT EXISTS idx_ledger_model_created ON request_ledger(public_model,created_at);
CREATE INDEX IF NOT EXISTS idx_routes_order ON model_routes(public_name, sort_order, id);
CREATE INDEX IF NOT EXISTS idx_providers_order ON providers(sort_order, id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_provider_auth_fingerprint ON providers(auth_fingerprint) WHERE auth_fingerprint <> '';
CREATE INDEX IF NOT EXISTS idx_providers_ip_pool_node ON providers(ip_pool_node_id);
CREATE INDEX IF NOT EXISTS idx_ip_pool_nodes_enabled ON ip_pool_nodes(enabled,id);
CREATE INDEX IF NOT EXISTS idx_provider_api_keys_selection ON provider_api_keys(provider_id,enabled,sort_order,id);
CREATE INDEX IF NOT EXISTS idx_provider_api_keys_node ON provider_api_keys(ip_pool_node_id);
CREATE INDEX IF NOT EXISTS idx_provider_api_key_models_lookup ON provider_api_key_models(provider_key_id,model);
UPDATE request_ledger SET usage_reported=1 WHERE usage_reported=0 AND (input_tokens>0 OR output_tokens>0 OR cached_tokens>0 OR reasoning_tokens>0);`)
	if err != nil {
		return err
	}

	// Provider groups table
	_, err = a.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS provider_groups (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  collapsed INTEGER NOT NULL DEFAULT 0,
  sort_order INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_groups_order ON provider_groups(sort_order, id);`)
	if err != nil {
		return err
	}

	// Add health check and group columns to providers
	for _, column := range []struct{ table, name, ddl string }{
		{"providers", "group_id", "INTEGER REFERENCES provider_groups(id) ON DELETE SET NULL"},
		{"providers", "group_sort_order", "INTEGER NOT NULL DEFAULT 0"},
		{"providers", "last_health_check_at", "TEXT"},
		{"providers", "health_check_status", "TEXT NOT NULL DEFAULT 'pending'"},
		{"providers", "health_check_error", "TEXT NOT NULL DEFAULT ''"},
		{"providers", "health_check_latency_ms", "INTEGER NOT NULL DEFAULT 0"},
		{"providers", "health_check_mode", "TEXT NOT NULL DEFAULT 'generation'"},
		{"providers", "health_check_first_byte_ms", "INTEGER NOT NULL DEFAULT 0"},
		{"providers", "health_check_model", "TEXT NOT NULL DEFAULT ''"},
		{"providers", "health_check_model_count", "INTEGER NOT NULL DEFAULT 0"},
	} {
		if err := ensureColumn(ctx, a.db, column.table, column.name, column.ddl); err != nil {
			return err
		}
	}
	if _, err := a.db.ExecContext(ctx, `UPDATE providers SET health_check_status='reachable' WHERE health_check_mode='connectivity' AND health_check_status='healthy'`); err != nil {
		return err
	}

	_, err = a.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_providers_group ON providers(group_id, group_sort_order, id);`)
	if err != nil {
		return err
	}
	// route_policies predates the global routing strategy: every write kept a
	// per-model row that no request path ever read. Drop the leftover table.
	if _, err := a.db.ExecContext(ctx, `DROP TABLE IF EXISTS route_policies`); err != nil {
		return err
	}
	if err := a.migrateProviderAPIKeys(ctx); err != nil {
		return err
	}
	_, err = a.db.ExecContext(ctx, `PRAGMA user_version=1`)
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
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05Z07:00", "2006-01-02 15:04:05", "2006-01-02"} {
		if v, e := time.Parse(layout, s); e == nil {
			return &v
		}
	}
	return nil
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
	mux.HandleFunc("/livez", a.live)
	mux.HandleFunc("/readyz", a.readyHealth)
	mux.HandleFunc("/healthz", a.readyHealth)
	mux.HandleFunc("/ui/", a.uiAsset)
	mux.HandleFunc("/", a.ui)
	mux.HandleFunc("/api/admin/login", a.login)
	mux.HandleFunc("/api/admin/logout", a.logout)
	mux.HandleFunc("/api/admin/session", a.admin(a.session))
	mux.HandleFunc("/api/admin/providers", a.admin(a.providers))
	mux.HandleFunc("/api/admin/providers/reorder", a.admin(a.reorderProviders))
	mux.HandleFunc("/api/admin/providers/export", a.admin(a.providerBackupExport))
	mux.HandleFunc("/api/admin/providers/import", a.admin(a.providerBackupImport))
	mux.HandleFunc("/api/admin/providers/batch", a.admin(a.providerBatch))
	mux.HandleFunc("/api/admin/ip-pool", a.admin(a.ipPoolNodes))
	mux.HandleFunc("/api/admin/ip-pool/", a.admin(a.ipPoolNodeByID))
	mux.HandleFunc("/api/admin/providers/", a.admin(a.providerByID))
	mux.HandleFunc("/api/admin/health-checks", a.admin(a.healthChecks))
	mux.HandleFunc("/api/admin/health-checks/", a.admin(a.healthCheckByID))
	mux.HandleFunc("/api/admin/provider-groups", a.admin(a.providerGroups))
	mux.HandleFunc("/api/admin/provider-groups/", a.admin(a.providerGroupByID))
	mux.HandleFunc("/api/admin/routes", a.admin(a.routes))
	mux.HandleFunc("/api/admin/routes/reorder", a.admin(a.reorderRoutes))
	mux.HandleFunc("/api/admin/routes/", a.admin(a.routeByID))
	mux.HandleFunc("/api/admin/models", a.admin(a.adminModels))
	mux.HandleFunc("/api/admin/models/", a.admin(a.modelByName))
	mux.HandleFunc("/api/admin/pricing", a.admin(a.pricing))
	mux.HandleFunc("/api/admin/keys", a.admin(a.keys))
	mux.HandleFunc("/api/admin/keys/", a.admin(a.keyByID))
	mux.HandleFunc("/api/admin/dashboard", a.admin(a.dashboard))
	mux.HandleFunc("/api/admin/metrics", a.admin(a.runtimeMetrics))
	mux.HandleFunc("/api/admin/routing", a.admin(a.routing))
	mux.HandleFunc("/api/admin/requests", a.admin(a.requests))
	mux.HandleFunc("/api/admin/quality-detector", a.admin(a.qualityDetector))
	mux.HandleFunc("/api/admin/quality-detector/", a.admin(a.qualityDetector))
	mux.HandleFunc("/api/admin/token-usage", a.admin(a.tokenUsage))
	mux.HandleFunc("/api/admin/auth/import/preview", a.admin(a.authImportPreview))
	mux.HandleFunc("/api/admin/auth/import/commit", a.admin(a.authImportCommit))
	mux.HandleFunc("/api/admin/auth/export", a.admin(a.authExport))
	mux.HandleFunc("/api/admin/auth/models/sync", a.admin(a.authModelSync))
	mux.HandleFunc("/api/admin/auth/oauth/start", a.admin(a.oauthStart))
	mux.HandleFunc("/api/admin/auth/oauth/complete", a.admin(a.oauthComplete))
	mux.HandleFunc("/api/admin/auth/quota/", a.admin(a.authQuota))
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
		// The console assembles its markup client-side from data that upstreams
		// partially control (model IDs, error strings). script-src still needs
		// 'unsafe-inline' for the pre-paint theme script and inline handlers, but
		// connect-src/img-src/form-action 'self' cut off the exfiltration channels
		// an injected fragment would need, and frame-ancestors/base-uri/object-src
		// close the remaining embedding vectors.
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'; object-src 'none'")
		next.ServeHTTP(w, r)
	})
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

var errRequestBodyTooLarge = errors.New("request body too large")

func readJSON(r *http.Request, v any) error {
	const limit = 10 << 20
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return err
	}
	if len(body) > limit {
		return errRequestBodyTooLarge
	}
	d := json.NewDecoder(strings.NewReader(string(body)))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain exactly one JSON value")
		}
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return nil
}
func fail(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": code, "code": code}})
}
