package fusiongate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

// inferenceSettings are reliability parameters for the single routing strategy.
// They are not a choice of selection algorithm: V3.13 always fails over by
// provider priority.
type inferenceSettings struct {
	Strategy              string `json:"strategy"`
	ChannelAttempts       int    `json:"channel_attempts"`
	MaxAttempts           int    `json:"max_attempts"`
	ChannelWindowSeconds  int    `json:"channel_window_seconds"`
	FailoverWindowSeconds int    `json:"failover_window_seconds"`
	SessionIdleDays       int    `json:"session_idle_days"`
	SessionMaxDays        int    `json:"session_max_days"`
	SessionCapacity       int    `json:"session_capacity"`
}

func defaultInferenceSettings() inferenceSettings {
	return inferenceSettings{
		Strategy:              "priority_failover",
		ChannelAttempts:       3,
		MaxAttempts:           9,
		ChannelWindowSeconds:  30,
		FailoverWindowSeconds: 120,
		SessionIdleDays:       7,
		SessionMaxDays:        30,
		SessionCapacity:       10000,
	}
}

func (a *App) inferenceSettingsFor(ctx context.Context) (inferenceSettings, error) {
	s := defaultInferenceSettings()
	var raw string
	if err := a.reader().QueryRowContext(ctx, `SELECT COALESCE((SELECT value FROM settings WHERE key='inference_settings'),'')`).Scan(&raw); err != nil {
		return s, err
	}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			return s, err
		}
	}
	// The environment cap remains authoritative when an operator set it.
	if a.cfg.MaxFailoverAttempts > 0 {
		s.MaxAttempts = a.cfg.MaxFailoverAttempts
	}
	if s.Strategy != "priority_failover" {
		s.Strategy = "priority_failover"
	}
	if err := s.validate(); err != nil {
		return defaultInferenceSettings(), nil
	}
	return s, nil
}

func (s inferenceSettings) validate() error {
	if s.Strategy != "priority_failover" {
		return fmt.Errorf("only priority_failover is supported")
	}
	if s.ChannelAttempts < 1 || s.ChannelAttempts > 5 {
		return fmt.Errorf("channel_attempts must be between 1 and 5")
	}
	if s.MaxAttempts < 1 || s.MaxAttempts > 100 {
		return fmt.Errorf("max_attempts must be between 1 and 100")
	}
	if s.ChannelWindowSeconds < 1 || s.ChannelWindowSeconds > 3600 {
		return fmt.Errorf("channel_window_seconds must be between 1 and 3600")
	}
	if s.FailoverWindowSeconds < 1 || s.FailoverWindowSeconds > 7200 {
		return fmt.Errorf("failover_window_seconds must be between 1 and 7200")
	}
	if s.SessionIdleDays < 1 || s.SessionIdleDays > 365 {
		return fmt.Errorf("session_idle_days must be between 1 and 365")
	}
	if s.SessionMaxDays < s.SessionIdleDays || s.SessionMaxDays > 365 {
		return fmt.Errorf("session_max_days must be at least session_idle_days and at most 365")
	}
	if s.SessionCapacity < 1 || s.SessionCapacity > 1000000 {
		return fmt.Errorf("session_capacity must be between 1 and 1000000")
	}
	return nil
}

// inferenceRoutingSettings serves the single routing-strategy endpoint. Any
// other strategy is rejected instead of being stored as an unread value.
func (a *App) inferenceRoutingSettings(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	current, err := a.inferenceSettingsFor(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", "cannot read routing settings")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, current)
	case http.MethodPatch:
		next := current
		if err := readJSON(r, &next); err != nil {
			fail(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if err := next.validate(); err != nil {
			fail(w, http.StatusBadRequest, "invalid_settings", err.Error())
			return
		}
		if a.cfg.MaxFailoverAttempts > 0 && next.MaxAttempts != a.cfg.MaxFailoverAttempts {
			fail(w, http.StatusConflict, "environment_override", "max_attempts is controlled by FUSIONGATE_MAX_FAILOVER_ATTEMPTS")
			return
		}
		raw, err := json.Marshal(next)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		tx, err := a.db.BeginTx(r.Context(), nil)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", "cannot save routing settings")
			return
		}
		defer tx.Rollback()
		values := map[string]string{
			"inference_settings":        string(raw),
			"routing_strategy":          "priority_failover",
			"routing_session_idle_days": strconv.Itoa(next.SessionIdleDays),
			"routing_session_max_days":  strconv.Itoa(next.SessionMaxDays),
			"routing_session_capacity":  strconv.Itoa(next.SessionCapacity),
		}
		for key, value := range values {
			if _, err := tx.ExecContext(r.Context(), `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value); err != nil {
				fail(w, http.StatusInternalServerError, "database_error", "cannot save routing settings")
				return
			}
		}
		if err := tx.Commit(); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", "cannot commit routing settings")
			return
		}
		writeJSON(w, http.StatusOK, next)
	default:
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET or PATCH required")
	}
}

// migrateInference records the pre-V3.13 strategy for audit, forces the single
// supported strategy, and creates the tables used by routing sessions, recovery
// state, recovery probe counters and per-attempt diagnostics. Legacy tables,
// accounts and credentials are left untouched.
func (a *App) migrateInference(ctx context.Context) error {
	if _, err := a.db.ExecContext(ctx, `
INSERT INTO settings(key,value) SELECT 'pre_v313_routing_strategy',value FROM settings WHERE key='routing_strategy' AND NOT EXISTS(SELECT 1 FROM settings WHERE key='pre_v313_routing_strategy');
INSERT INTO settings(key,value) VALUES('routing_strategy','priority_failover') ON CONFLICT(key) DO UPDATE SET value=excluded.value;`); err != nil {
		return err
	}
	if err := a.migrateRoutingSessions(ctx); err != nil {
		return err
	}
	if err := a.migrateInferenceProviderColumns(ctx); err != nil {
		return err
	}
	if err := a.migrateRecovery(ctx); err != nil {
		return err
	}
	if err := a.migrateRecoveryProbes(ctx); err != nil {
		return err
	}
	_, err := a.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS inference_attempts (
  request_id TEXT PRIMARY KEY,
  gateway_request_id TEXT NOT NULL DEFAULT '',
  task_hash TEXT NOT NULL DEFAULT '',
  task_source TEXT NOT NULL DEFAULT '',
  task_scope TEXT NOT NULL DEFAULT '',
  provider_id INTEGER NOT NULL DEFAULT 0,
  provider_key_id INTEGER NOT NULL DEFAULT 0,
  channel_attempt INTEGER NOT NULL DEFAULT 0,
  channel_hop INTEGER NOT NULL DEFAULT 0,
  adapter_id TEXT NOT NULL DEFAULT '',
  execution_mode TEXT NOT NULL DEFAULT '',
  upstream_protocol TEXT NOT NULL DEFAULT '',
  upstream_path TEXT NOT NULL DEFAULT '',
  candidate_order TEXT NOT NULL DEFAULT '[]',
  stop_reason TEXT NOT NULL DEFAULT '',
  fault_scope TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS inference_attempts_gateway ON inference_attempts(gateway_request_id,created_at);
CREATE INDEX IF NOT EXISTS inference_attempts_task ON inference_attempts(task_hash,task_scope,created_at);`)
	if err != nil {
		return err
	}
	// V3.32 carries learned protocol facts across restarts.
	return a.migrateProtocolCapabilities(ctx)
}

// migrateInferenceProviderColumns adds the opt-in recovery probe columns. They
// default to off: the gateway never starts paying for generation probes on its
// own.
func (a *App) migrateInferenceProviderColumns(ctx context.Context) error {
	for _, column := range []struct{ name, ddl string }{
		{"auto_recovery_probe", "INTEGER NOT NULL DEFAULT 0"},
		{"recovery_probe_daily_cap", "INTEGER NOT NULL DEFAULT 0"},
		{"recovery_probe_day", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := ensureColumn(ctx, a.db, "providers", column.name, column.ddl); err != nil {
			return err
		}
	}
	return nil
}
