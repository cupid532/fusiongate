package fusiongate

import (
	"context"
	"time"
)

// Protocol facts outlive the process.
//
// The in-memory index answers "does this channel serve this protocol" without a
// database read on the request path, but it dies with the process. A restart
// therefore used to reintroduce the exact cost the index exists to avoid: the
// first request to every chat-only channel probed a native endpoint that had
// already been proven missing. Unexpired facts are now written through to a
// table and restored at startup, so a restart resumes the same learning.
//
// The lifetime stays deliberately short. A learned fact is an observation, not a
// declaration: it has to expire so a channel that gained, or had restored, an
// endpoint is tried again. That is also why a failure no longer erases the
// operator's declaration in model_routes.capabilities -- see the Responses
// demotion in gateway.go, which used to delete the declaration permanently.
const wireCapabilityUnsupported = "unsupported"

// migrateProtocolCapabilities creates the table that carries learned protocol
// facts across restarts.
//
// expires_at is unix nanoseconds rather than a formatted timestamp so the expiry
// comparison is numeric. RFC3339Nano trims trailing zeros from the fraction, and
// the same instant then compares as greater or smaller lexicographically
// depending on where its digits happen to end.
func (a *App) migrateProtocolCapabilities(ctx context.Context) error {
	_, err := a.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS channel_protocol_capabilities (
  memory_key TEXT PRIMARY KEY,
  provider_id INTEGER NOT NULL DEFAULT 0,
  provider_key_id INTEGER NOT NULL DEFAULT 0,
  upstream_model TEXT NOT NULL DEFAULT '',
  protocol TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT '',
  evidence TEXT NOT NULL DEFAULT '',
  sampled_at TEXT NOT NULL DEFAULT '',
  expires_at INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS channel_protocol_capabilities_expiry ON channel_protocol_capabilities(expires_at);`)
	return err
}

// seedProtocolCapabilities restores the facts that are still in force and drops
// the ones that are not, so the table cannot accumulate rows that no longer say
// anything.
func (a *App) seedProtocolCapabilities(ctx context.Context) error {
	stamp := time.Now().UnixNano()
	if _, err := a.db.ExecContext(ctx, `DELETE FROM channel_protocol_capabilities WHERE expires_at <= ?`, stamp); err != nil {
		return err
	}
	rows, err := a.db.QueryContext(ctx, `SELECT memory_key,expires_at FROM channel_protocol_capabilities WHERE expires_at > ?`, stamp)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var expires int64
		if err := rows.Scan(&key, &expires); err != nil {
			return err
		}
		protocolMemory.restore(key, time.Unix(0, expires))
	}
	return rows.Err()
}

// rememberProtocol records that a channel does not serve a protocol: in the
// index the request path reads, and in the table a later process reads.
//
// It writes only when the fact is new. The alternative -- a statement per call --
// put an upsert on the hot path of every refused attempt, and the single SQLite
// writer is shared with the request ledger, so a saturated writer would have
// delayed the answers it was writing about.
func (a *App) rememberProtocol(z resolvedRoute, protocol, evidence string) {
	until := time.Now().Add(wireMemoryTTL)
	if !protocolMemory.rememberUntil(z, protocol, until) {
		return
	}
	a.queueLedgerWrite(`INSERT INTO channel_protocol_capabilities(memory_key,provider_id,provider_key_id,upstream_model,protocol,state,evidence,sampled_at,expires_at)
VALUES(?,?,?,?,?,?,?,?,?)
ON CONFLICT(memory_key) DO UPDATE SET provider_id=excluded.provider_id,provider_key_id=excluded.provider_key_id,upstream_model=excluded.upstream_model,protocol=excluded.protocol,state=excluded.state,evidence=excluded.evidence,sampled_at=excluded.sampled_at,expires_at=excluded.expires_at`,
		wireMemoryKey(z, protocol), z.Provider.ID, z.ProviderKeyID, z.Route.UpstreamModel, protocol, wireCapabilityUnsupported, evidence, now(), until.UnixNano())
}

// forgetProtocol clears a fact the channel has just disproved. A successful call
// is proof the protocol works, so the next request goes native again without
// waiting for the lifetime to elapse.
//
// Like rememberProtocol it writes only when something actually changed: a
// successful native call is the common case, and it must not queue a statement
// just to say that nothing was learned.
func (a *App) forgetProtocol(z resolvedRoute, protocol string) {
	if !protocolMemory.forget(z, protocol) {
		return
	}
	a.queueLedgerWrite(`DELETE FROM channel_protocol_capabilities WHERE memory_key=?`, wireMemoryKey(z, protocol))
}
