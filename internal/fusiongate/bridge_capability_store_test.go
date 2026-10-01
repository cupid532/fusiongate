package fusiongate

import (
	"strings"
	"testing"
	"time"
)

// capabilityRoute is the identity a persisted protocol fact is keyed by.
func capabilityRoute() resolvedRoute {
	return resolvedRoute{
		Route:         Route{ID: 4, UpstreamModel: "upstream-model", Capabilities: "chat,stream"},
		Provider:      Provider{ID: 9, Type: "openai_compatible", BaseURL: "https://relay.example", ProtocolPolicy: protocolAuto},
		ProviderKeyID: 3,
		Credential:    "access-token",
	}
}

func emptyProtocolMemory() {
	protocolMemory = &wireMemory{entries: map[string]time.Time{}}
}

// What a channel proved about its protocols must survive a restart. Otherwise
// the first request after every restart probes a native endpoint that was
// already known to be missing, which is the cost the index exists to avoid.
func TestLearnedProtocolFactSurvivesRestart(t *testing.T) {
	cfg := testConfig(t)
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	z := capabilityRoute()
	a.rememberProtocol(z, wireResponses, "endpoint not served")
	a.flushLedgerWrites()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	// A new process starts from an empty index; only the table can refill it.
	emptyProtocolMemory()
	reopened, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if !protocolMemory.unsupported(z, wireResponses) {
		t.Fatal("a learned protocol fact must be restored at startup")
	}
	if protocolMemory.unsupported(z, wireChat) {
		t.Fatal("restoring one protocol must not mark every protocol unsupported")
	}
}

// A fact that is no longer in force must not be restored, and must not stay in
// the table either.
func TestExpiredProtocolFactIsDroppedAtStartup(t *testing.T) {
	cfg := testConfig(t)
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	z := capabilityRoute()
	a.rememberProtocol(z, wireResponses, "endpoint not served")
	a.flushLedgerWrites()
	if _, err := a.db.Exec(`UPDATE channel_protocol_capabilities SET expires_at=?`, time.Now().Add(-time.Minute).UnixNano()); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	emptyProtocolMemory()
	reopened, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if protocolMemory.unsupported(z, wireResponses) {
		t.Fatal("an expired fact must not be restored")
	}
	var rows int
	if err := reopened.db.QueryRow(`SELECT COUNT(*) FROM channel_protocol_capabilities`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("expired rows kept = %d, want 0", rows)
	}
}

// A successful call disproves the fact, so it must not linger in either place.
func TestForgetProtocolClearsThePersistedFact(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	z := capabilityRoute()
	a.rememberProtocol(z, wireResponses, "endpoint not served")
	a.flushLedgerWrites()
	if !protocolMemory.unsupported(z, wireResponses) {
		t.Fatal("the fact must be in force after it is learned")
	}
	a.forgetProtocol(z, wireResponses)
	a.flushLedgerWrites()
	if protocolMemory.unsupported(z, wireResponses) {
		t.Fatal("a proven protocol must be usable again immediately")
	}
	var rows int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM channel_protocol_capabilities`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("persisted facts kept = %d, want 0", rows)
	}
}

// The persisted row is an observation about a channel, never a credential.
func TestPersistedFactCarriesNoCredential(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	z := capabilityRoute()
	z.Credential = "sk-live-secret-token"
	a.rememberProtocol(z, wireResponses, "endpoint not served")
	a.flushLedgerWrites()
	var key, protocol, state, model string
	var providerID, keyID int64
	if err := a.db.QueryRow(`SELECT memory_key,provider_id,provider_key_id,upstream_model,protocol,state FROM channel_protocol_capabilities`).
		Scan(&key, &providerID, &keyID, &model, &protocol, &state); err != nil {
		t.Fatal(err)
	}
	if providerID != 9 || keyID != 3 || model != "upstream-model" || protocol != wireResponses || state != wireCapabilityUnsupported {
		t.Fatalf("row = %d/%d/%q/%q/%q", providerID, keyID, model, protocol, state)
	}
	if len(key) == 0 || strings.Contains(key, "secret") {
		t.Fatalf("memory key looks like a credential: %q", key)
	}
}

// A declared protocol is suppressed, not deleted, when its endpoint refuses. One
// failure used to erase the declaration permanently: nothing ever wrote it back,
// so an endpoint that was briefly down -- or that appeared later -- stayed
// unreachable for the life of the row.
func TestDeclaredProtocolIsSuppressedNotDeleted(t *testing.T) {
	emptyProtocolMemory()
	z := capabilityRoute()
	z.Provider.Type = "anthropic"
	z.Route.Capabilities = "chat,stream,protocol:responses"
	if !routeProtocolEnabled(z, protocolResponses) {
		t.Fatal("a declared protocol must be selectable")
	}
	protocolMemory.remember(z, wireResponses)
	if routeProtocolEnabled(z, protocolResponses) {
		t.Fatal("a refused protocol must not be selected while the fact is in force")
	}
	protocolMemory.forget(z, wireResponses)
	if !routeProtocolEnabled(z, protocolResponses) {
		t.Fatal("clearing the fact must restore the declaration without an operator edit")
	}
}

// A route that declares no protocol says nothing about protocols, so learning
// must not invent a declaration either.
func TestUndeclaredProtocolStaysUndeclared(t *testing.T) {
	emptyProtocolMemory()
	z := capabilityRoute()
	protocolMemory.remember(z, wireResponses)
	if routeProtocolEnabled(z, protocolResponses) {
		t.Fatal("an undeclared protocol stays undeclared regardless of learning")
	}
}

// A successful native call is the common case, and it must not queue a statement
// just to say that nothing was learned. The single SQLite writer is shared with
// the request ledger, so an in-flight stream must never wait for a saturated
// writer to write about itself.
func TestSuccessfulCallQueuesNoCapabilityWrite(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	z := capabilityRoute()
	// The index is process-wide; a previous case must not decide this one.
	emptyProtocolMemory()

	before := a.metrics.ledgerQueued.Load()
	a.forgetProtocol(z, wireResponses)
	if queued := a.metrics.ledgerQueued.Load() - before; queued != 0 {
		t.Fatalf("queued %d statements for a fact that was never learned", queued)
	}

	// Learning a fact persists it once; restating it persists nothing.
	a.rememberProtocol(z, wireResponses, "endpoint not served")
	a.flushLedgerWrites()
	before = a.metrics.ledgerQueued.Load()
	a.rememberProtocol(z, wireResponses, "endpoint not served")
	if queued := a.metrics.ledgerQueued.Load() - before; queued != 0 {
		t.Fatalf("queued %d statements to restate a fact already in force", queued)
	}

	// Clearing it still writes, because that is a real state change.
	before = a.metrics.ledgerQueued.Load()
	a.forgetProtocol(z, wireResponses)
	a.flushLedgerWrites()
	if queued := a.metrics.ledgerQueued.Load() - before; queued != 1 {
		t.Fatalf("queued %d statements to clear a learned fact, want 1", queued)
	}
}
