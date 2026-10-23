package fusiongate

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
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

// bindRoutingSession merges a proposed forward cursor with the persisted task.
// A generation is only an observed revision, not proof of progress: concurrent
// success and advance callbacks can propose the same (or even older) generation.
func (a *App) bindRoutingSession(ctx context.Context, session routingSession) error {
	_, err := a.storeRoutingSession(ctx, session)
	return err
}

// storeRoutingSession reads, merges and writes on the single writer connection
// in one transaction. A read from reader() followed by an UPSERT would race with
// another request between the read and write and lose its departed identities.
func (a *App) storeRoutingSession(ctx context.Context, session routingSession) (routingSession, error) {
	if !session.usable() {
		return session, nil
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return session, err
	}
	defer tx.Rollback()
	stored := routingSession{TaskHash: session.TaskHash, TaskScope: session.TaskScope}
	var orderJSON string
	err = tx.QueryRowContext(ctx, `
SELECT task_source,order_json,cursor,provider_id,provider_key_id,plan_id,generation,created_at,updated_at
FROM routing_sessions WHERE task_hash=? AND task_scope=?`, session.TaskHash, session.TaskScope).Scan(
		&stored.TaskSource, &orderJSON, &stored.Cursor, &stored.ProviderID, &stored.ProviderKeyID,
		&stored.PlanID, &stored.Generation, &stored.CreatedAt, &stored.UpdatedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return session, err
	}
	if err == nil {
		if err := json.Unmarshal([]byte(orderJSON), &stored.Order); err != nil {
			return session, fmt.Errorf("decode routing history: %w", err)
		}
		session = mergeRoutingSession(stored, session)
	}
	if session.Order == nil {
		session.Order = []int64{}
	}
	encoded, err := json.Marshal(session.Order)
	if err != nil {
		return session, err
	}
	stamp := now()
	_, err = tx.ExecContext(ctx, `
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
  updated_at=excluded.updated_at`,
		session.TaskHash, session.TaskScope, session.TaskSource, string(encoded), session.Cursor,
		session.ProviderID, session.ProviderKeyID, session.PlanID, session.Generation, stamp, stamp)
	if err != nil {
		return session, err
	}
	if err := tx.Commit(); err != nil {
		return session, err
	}
	return session, nil
}

// departedProviders is an identity history, not an offset in today's priority
// list. Keep absent IDs too: temporarily disabling/removing a spent channel must
// not make it fresh if it is enabled again during this task's lifetime.
func (s routingSession) departedProviders() []int64 {
	if bound := slices.Index(s.Order, s.ProviderID); bound >= 0 {
		return s.Order[:bound]
	}
	return nil
}

// routingSessionOrder represents history without a schema change: the departed
// identities precede the bound provider; everything after it is still unvisited.
func routingSessionOrder(session routingSession, departed []int64, tails ...[]int64) routingSession {
	order := make([]int64, 0, len(departed)+1)
	seen := make(map[int64]bool)
	appendIDs := func(ids []int64) {
		for _, id := range ids {
			if id > 0 && !seen[id] && id != session.ProviderID {
				order = append(order, id)
				seen[id] = true
			}
		}
	}
	appendIDs(departed)
	session.Cursor = len(order)
	if session.ProviderID > 0 {
		order = append(order, session.ProviderID)
		seen[session.ProviderID] = true
	}
	for _, tail := range tails {
		appendIDs(tail)
	}
	session.Order = order
	return session
}

// mergeRoutingSession resolves concurrent proposals by proof of departure. A
// success on A cannot undo B's record that A was spent, regardless of generation.
// Conversely an older request that really exhausted A may advance past newer A
// successes. An unrelated concurrent binding does not steal the current task.
func mergeRoutingSession(stored, proposed routingSession) routingSession {
	past := stored.departedProviders()
	proposedPast := proposed.departedProviders()
	next := stored
	switch {
	case proposed.ProviderID <= 0 || slices.Contains(past, proposed.ProviderID):
		// Reject the stale destination, not independent departure evidence.
	case stored.ProviderID == proposed.ProviderID:
		// Concurrent successes on the same channel may remember a working Key;
		// an older snapshot must not erase a newer credential preference.
		if proposed.Generation >= stored.Generation {
			next = proposed
			if next.ProviderKeyID == 0 {
				// An advance arriving at an already-bound channel carries no
				// successful-Key evidence. Do not erase a concurrent success.
				next.ProviderKeyID = stored.ProviderKeyID
			}
		}
	case stored.ProviderID <= 0 || slices.Contains(proposedPast, stored.ProviderID):
		next = proposed
	default:
		// Keep the established binding when concurrent priority snapshots pick
		// unrelated destinations. Still merge any other exhausted identities.
	}
	next.Generation = max(stored.Generation, proposed.Generation)
	departed := append(append([]int64(nil), past...), proposedPast...)
	return routingSessionOrder(next, departed, proposed.Order, stored.Order)
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

// resumeInferenceChannels rebuilds the reachable suffix by provider identity.
// Merely counting a historical prefix and slicing the untouched live priority
// order can select a spent channel after a reorder. Keep live spent channels in
// a non-executable prefix (for diagnostics), lead with the bound channel if it
// exists, then keep every unvisited/new channel in today's priority order.
func resumeInferenceChannels(session routingSession, channels []inferenceChannel) ([]inferenceChannel, int) {
	spent := make(map[int64]bool)
	for _, id := range session.departedProviders() {
		spent[id] = true
	}
	ordered := make([]inferenceChannel, 0, len(channels))
	var bound *inferenceChannel
	remaining := make([]inferenceChannel, 0, len(channels))
	for _, channel := range channels {
		switch {
		case spent[channel.ProviderID]:
			ordered = append(ordered, channel)
		case channel.ProviderID == session.ProviderID:
			bound = &channel
		default:
			remaining = append(remaining, channel)
		}
	}
	start := len(ordered)
	if bound != nil {
		ordered = append(ordered, *bound)
	}
	return append(ordered, remaining...), start
}

// routingSessionAt records only identities before this request's selected
// channel as departed, while retaining earlier task history, including absent
// IDs. If the old bound channel disappeared, leaving it for another successful
// channel also makes that old binding spent rather than reviving it on return.
func routingSessionAt(previous routingSession, channels []inferenceChannel, index int) routingSession {
	next := previous
	next.ProviderID = channels[index].ProviderID
	if next.ProviderID != previous.ProviderID {
		next.ProviderKeyID = 0
	}
	next.PlanID = sessionPlanID(channels)
	next.Generation++
	departed := append([]int64(nil), previous.departedProviders()...)
	if previous.ProviderID > 0 && previous.ProviderID != next.ProviderID {
		departed = append(departed, previous.ProviderID)
	}
	order := storedOrderFromChannels(channels)
	departed = append(departed, order[:index]...)
	return routingSessionOrder(next, departed, order, previous.Order)
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
