package fusiongate

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Task stickiness needs one identifier that names a task, not a request. Only a
// client-declared identifier is trusted. A body fingerprint, IP address or
// User-Agent cannot tell "the same task, next turn" from "a new task that opens
// with the same words", so those are never used to bind a channel here.
const (
	taskIDHeader = "X-Fusiongate-Task-ID"
)

// sessionHeaderNames are the client-session identifiers that are known to be
// stable for a whole task and are already sent by supported clients.
var sessionHeaderNames = []string{"x-opencode-session", "session_id", "x-session-id", "conversation_id"}

// routingSession is the per-task forward cursor. It records how far a task has
// advanced through the channel order so that a later request of the same task
// resumes on the channel it was moved to instead of starting over at the top.
type routingSession struct {
	TaskHash      string  `json:"task_hash"`
	TaskScope     string  `json:"task_scope"`
	TaskSource    string  `json:"task_source"`
	Order         []int64 `json:"order"`
	Cursor        int     `json:"cursor"`
	ProviderID    int64   `json:"provider_id"`
	ProviderKeyID int64   `json:"provider_key_id"`
	PlanID        string  `json:"plan_id"`
	Generation    int64   `json:"generation"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
}

func (s routingSession) usable() bool {
	return s.TaskHash != "" && s.TaskSource != ""
}

// inferenceTaskIdentity resolves the task identifier and returns an
// authenticated hash of it. The hash is scoped to the downstream API key so two
// tenants that happen to pick the same session string never share a binding,
// and the raw client identifier is not stored.
//
// source is empty when the client sent no reliable task identifier. In that case
// request-level failover still works, but strict task stickiness is disabled and
// the console says so.
func inferenceTaskIdentity(r *http.Request, key authKey, keyMaterial []byte) (hash, source, taskID string) {
	if r == nil {
		return "", "", ""
	}
	if v := strings.TrimSpace(r.Header.Get(taskIDHeader)); v != "" {
		return taskHashFor(key, keyMaterial, "explicit", v), "explicit", v
	}
	for _, name := range sessionHeaderNames {
		if v := strings.TrimSpace(r.Header.Get(name)); v != "" {
			return taskHashFor(key, keyMaterial, name, v), name, v
		}
	}
	return "", "", ""
}

func taskHashFor(key authKey, keyMaterial []byte, source, taskID string) string {
	mac := hmac.New(sha256.New, keyMaterial)
	mac.Write([]byte(strconv.FormatInt(key.ID, 10)))
	mac.Write([]byte{0})
	mac.Write([]byte(source))
	mac.Write([]byte{0})
	mac.Write([]byte(taskID))
	return hex.EncodeToString(mac.Sum(nil))
}

// inferenceTaskScope scopes a binding to the model and capability it was made
// for. The same task may legitimately talk to a different model later; that is a
// different sub-binding, so it must not inherit a channel that never served it.
func inferenceTaskScope(model, capability string) string {
	return strings.ToLower(strings.TrimSpace(model)) + "\x00" + capability
}

func (a *App) migrateRoutingSessions(ctx context.Context) error {
	_, err := a.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS routing_sessions (
  task_hash TEXT NOT NULL,
  task_scope TEXT NOT NULL,
  task_source TEXT NOT NULL DEFAULT '',
  order_json TEXT NOT NULL DEFAULT '[]',
  cursor INTEGER NOT NULL DEFAULT 0,
  provider_id INTEGER NOT NULL DEFAULT 0,
  provider_key_id INTEGER NOT NULL DEFAULT 0,
  plan_id TEXT NOT NULL DEFAULT '',
  generation INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (task_hash, task_scope));
CREATE INDEX IF NOT EXISTS routing_sessions_updated ON routing_sessions(updated_at);
CREATE INDEX IF NOT EXISTS routing_sessions_provider ON routing_sessions(provider_id);`)
	return err
}

// loadRoutingSession returns the stored forward cursor for one task scope.
func (a *App) loadRoutingSession(ctx context.Context, taskHash, taskScope string) (routingSession, bool) {
	session := routingSession{}
	if taskHash == "" {
		return session, false
	}
	var orderJSON string
	if err := a.reader().QueryRowContext(ctx, `
SELECT task_source,order_json,cursor,provider_id,provider_key_id,plan_id,generation,created_at,updated_at
FROM routing_sessions WHERE task_hash=? AND task_scope=?`, taskHash, taskScope).Scan(
		&session.TaskSource, &orderJSON, &session.Cursor, &session.ProviderID, &session.ProviderKeyID,
		&session.PlanID, &session.Generation, &session.CreatedAt, &session.UpdatedAt); err != nil {
		return routingSession{}, false
	}
	session.TaskHash = taskHash
	session.TaskScope = taskScope
	if orderJSON != "" {
		_ = json.Unmarshal([]byte(orderJSON), &session.Order)
	}
	return session, session.usable()
}

// bindRoutingSession stores a forward cursor. Writes are monotonic: a writer
// carrying an older generation than the stored row is ignored, so a slow request
// that loses a race can never move the task back to a channel it already left.
func (a *App) bindRoutingSession(ctx context.Context, session routingSession) error {
	if !session.usable() {
		return nil
	}
	order := session.Order
	if order == nil {
		order = []int64{}
	}
	encoded, err := json.Marshal(order)
	if err != nil {
		return err
	}
	stamp := now()
	_, err = a.db.ExecContext(ctx, `
INSERT INTO routing_sessions(task_hash,task_scope,task_source,order_json,cursor,provider_id,provider_key_id,plan_id,generation,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(task_hash,task_scope) DO UPDATE SET
  task_source=excluded.task_source,
  order_json=excluded.order_json,
  cursor=excluded.cursor,
  provider_id=excluded.provider_id,
  provider_key_id=excluded.provider_key_id,
  plan_id=excluded.plan_id,
  generation=excluded.generation,
  updated_at=excluded.updated_at
WHERE excluded.generation >= routing_sessions.generation`,
		session.TaskHash, session.TaskScope, session.TaskSource, string(encoded), session.Cursor,
		session.ProviderID, session.ProviderKeyID, session.PlanID, session.Generation, stamp, stamp)
	return err
}

// pruneRoutingSessions bounds the table by idle age, hard age and row capacity.
// Expiry is not a recovery action: a dropped binding simply means the next
// request of that task is treated as new.
func (a *App) pruneRoutingSessions(ctx context.Context, settings inferenceSettings) error {
	idleDays := settings.SessionIdleDays
	if idleDays <= 0 {
		idleDays = defaultInferenceSettings().SessionIdleDays
	}
	maxDays := settings.SessionMaxDays
	if maxDays <= 0 {
		maxDays = defaultInferenceSettings().SessionMaxDays
	}
	capacity := settings.SessionCapacity
	if capacity <= 0 {
		capacity = defaultInferenceSettings().SessionCapacity
	}
	idleCutoff := time.Now().AddDate(0, 0, -idleDays).UTC().Format(time.RFC3339Nano)
	maxCutoff := time.Now().AddDate(0, 0, -maxDays).UTC().Format(time.RFC3339Nano)
	if _, err := a.db.ExecContext(ctx, `DELETE FROM routing_sessions WHERE updated_at < ? OR created_at < ?`, idleCutoff, maxCutoff); err != nil {
		return err
	}
	_, err := a.db.ExecContext(ctx, `DELETE FROM routing_sessions WHERE rowid IN (
SELECT rowid FROM routing_sessions ORDER BY updated_at DESC LIMIT -1 OFFSET ?)`, capacity)
	return err
}

// resumeStartIndex maps a stored channel order onto the freshly computed plan.
//
// The stored order is the task's history. Channels that sat ahead of the bound
// channel when the task moved forward stay behind it forever, even if they have
// recovered since or a new higher-priority channel appeared: a task never walks
// back up the order. Channels the task never reached are still reachable.
func resumeStartIndex(storedOrder []int64, boundProviderID int64, channels []inferenceChannel) int {
	if boundProviderID <= 0 || len(channels) == 0 {
		return 0
	}
	bound := -1
	for i, id := range storedOrder {
		if id == boundProviderID {
			bound = i
			break
		}
	}
	if bound < 0 {
		// No history for the bound channel: fall back to its live position.
		for i, channel := range channels {
			if channel.ProviderID == boundProviderID {
				return i
			}
		}
		return 0
	}
	present := map[int64]bool{}
	for _, channel := range channels {
		present[channel.ProviderID] = true
	}
	behind := 0
	for _, id := range storedOrder[:bound] {
		if present[id] {
			behind++
		}
	}
	if behind > len(channels) {
		return len(channels)
	}
	return behind
}

func sessionPlanID(channels []inferenceChannel) string {
	parts := make([]string, 0, len(channels))
	for _, channel := range channels {
		parts = append(parts, strconv.FormatInt(channel.ProviderID, 10))
	}
	return strings.Join(parts, ",")
}

func storedOrderFromChannels(channels []inferenceChannel) []int64 {
	order := make([]int64, 0, len(channels))
	for _, channel := range channels {
		order = append(order, channel.ProviderID)
	}
	return order
}

// preferSessionKey moves the Key a task already used successfully to the front
// of its channel so the same credential keeps serving the task while it works.
func preferSessionKey(routes []resolvedRoute, keyID int64) []resolvedRoute {
	if keyID <= 0 || len(routes) < 2 {
		return routes
	}
	out := make([]resolvedRoute, 0, len(routes))
	for _, z := range routes {
		if z.ProviderKeyID == keyID {
			out = append(out, z)
		}
	}
	if len(out) == 0 {
		return routes
	}
	for _, z := range routes {
		if z.ProviderKeyID != keyID {
			out = append(out, z)
		}
	}
	return out
}

func (a *App) routingSessionCount(ctx context.Context) (int, error) {
	var count int
	err := a.reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM routing_sessions`).Scan(&count)
	return count, err
}

func routingSessionSummary(session routingSession) string {
	if !session.usable() {
		return ""
	}
	return fmt.Sprintf("%s:%d", session.TaskSource, session.ProviderID)
}
