package fusiongate

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// What the bridge policy did to a channel's fields, aggregated for an operator.
//
// The reason this exists: when a client starts sending a field no bridge can
// express, the only visible symptom is a 400 whose message names the field, and
// finding it used to mean attaching a capture proxy to a real client and reading
// one request body. The refusal says which field; the ledger says which request;
// neither answers "which channel is being held back, and by what".
//
// The record is deliberately in memory only. It answers a question about the
// running gateway, and writing an aggregate row per refused request would put
// exactly the traffic that is already failing onto the single SQLite writer
// shared with the request ledger. The durable facts -- which protocols a channel
// serves -- are persisted separately, in channel_protocol_capabilities.
type bridgeFieldEvent struct {
	ProviderID    int64  `json:"provider_id"`
	ProviderName  string `json:"provider_name"`
	UpstreamModel string `json:"upstream_model"`
	Protocol      string `json:"protocol"`
	Field         string `json:"field"`
	Disposition   string `json:"disposition"`
	Reason        string `json:"reason"`
	Hits          int64  `json:"hits"`
	FirstSeenAt   string `json:"first_seen_at"`
	LastSeenAt    string `json:"last_seen_at"`
}

const (
	bridgeDispositionDropped = "dropped"
	bridgeDispositionRefused = "refused"

	// bridgeFieldAuditLimit bounds the aggregate so a client inventing a new
	// field name on every request cannot grow it without bound. The least
	// recently seen entry is evicted once the limit is reached.
	bridgeFieldAuditLimit = 512
)

type bridgeFieldAudit struct {
	mu     sync.Mutex
	events map[string]*bridgeFieldEvent
}

func newBridgeFieldAudit() *bridgeFieldAudit {
	return &bridgeFieldAudit{events: map[string]*bridgeFieldEvent{}}
}

// auditBridgeRefusal records the field a rejection named, against the channel
// that could not serve it. Refusals are rare -- they are the requests that
// failed -- so this costs nothing on the path that works.
func (a *App) auditBridgeRefusal(z resolvedRoute, client string, rejection error) {
	if a.bridgeFields == nil {
		return
	}
	var capability *bridgeCapabilityError
	if errors.As(rejection, &capability) {
		a.recordBridgeField(z, client, capability.feature, bridgeDispositionRefused, capability.reason)
	}
}

// auditBridgeDrops records the fields the converter is about to leave behind. It
// reads the body the converter has already produced, so it costs no extra parse
// on the request path: the audit describes work the bridge did anyway.
func (a *App) auditBridgeDrops(z resolvedRoute, client string, chat map[string]any, opts bridgeOptions) {
	if a.bridgeFields == nil {
		return
	}
	for _, rule := range bridgeChatFieldPolicy {
		if rule.disposition != bridgeDrop || !rule.covers(client) || chat[rule.path] == nil {
			continue
		}
		a.recordBridgeField(z, client, rule.path, bridgeDispositionDropped, "")
	}
	// A loss the caller asked for is a drop like any other, and an operator needs
	// to see it: it is the one case where a channel is knowingly serving a
	// degraded answer.
	for field := range opts.allowedLosses {
		if chat[field] == nil {
			continue
		}
		a.recordBridgeField(z, client, field, bridgeDispositionDropped, "accepted by the caller; this channel cannot honour it")
	}
}

func (a *App) recordBridgeField(z resolvedRoute, client, field, disposition, reason string) {
	if field == "" {
		return
	}
	stamp := now()
	key := strings.Join([]string{strconv.FormatInt(z.Provider.ID, 10), z.Route.UpstreamModel, client, field, disposition}, "\x00")
	a.bridgeFields.mu.Lock()
	defer a.bridgeFields.mu.Unlock()
	if event, ok := a.bridgeFields.events[key]; ok {
		event.Hits++
		event.LastSeenAt = stamp
		if reason != "" {
			event.Reason = reason
		}
		return
	}
	if len(a.bridgeFields.events) >= bridgeFieldAuditLimit {
		a.bridgeFields.evictOldestLocked()
	}
	a.bridgeFields.events[key] = &bridgeFieldEvent{
		ProviderID:    z.Provider.ID,
		ProviderName:  firstNonEmpty(z.Provider.Name, z.Provider.Type),
		UpstreamModel: z.Route.UpstreamModel,
		Protocol:      client,
		Field:         field,
		Disposition:   disposition,
		Reason:        reason,
		Hits:          1,
		FirstSeenAt:   stamp,
		LastSeenAt:    stamp,
	}
}

// evictOldestLocked drops the least recently seen entry. Callers hold the lock.
func (a *bridgeFieldAudit) evictOldestLocked() {
	oldestKey := ""
	oldest := ""
	for key, event := range a.events {
		if oldestKey == "" || event.LastSeenAt < oldest {
			oldestKey, oldest = key, event.LastSeenAt
		}
	}
	if oldestKey != "" {
		delete(a.events, oldestKey)
	}
}

// bridgeFieldEvents returns the aggregate, most frequent first, so the channel
// being held back the most is the first thing an operator reads.
func (a *App) bridgeFieldEvents() []bridgeFieldEvent {
	if a.bridgeFields == nil {
		return []bridgeFieldEvent{}
	}
	a.bridgeFields.mu.Lock()
	events := make([]bridgeFieldEvent, 0, len(a.bridgeFields.events))
	for _, event := range a.bridgeFields.events {
		events = append(events, *event)
	}
	a.bridgeFields.mu.Unlock()
	sort.Slice(events, func(i, j int) bool {
		if events[i].Hits != events[j].Hits {
			return events[i].Hits > events[j].Hits
		}
		if events[i].ProviderID != events[j].ProviderID {
			return events[i].ProviderID < events[j].ProviderID
		}
		if events[i].Field != events[j].Field {
			return events[i].Field < events[j].Field
		}
		return events[i].Disposition < events[j].Disposition
	})
	return events
}

// capabilityAudit serves what the policy has been doing to each channel's
// fields, so a new client field is visible in the console instead of requiring a
// capture proxy on a live client.
func (a *App) capabilityAudit(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"events": a.bridgeFieldEvents(),
		"limit":  bridgeFieldAuditLimit,
		"scope":  "aggregated in memory since this process started",
	})
}
