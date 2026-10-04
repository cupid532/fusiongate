package fusiongate

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// A routing window limits work while failover is still possible, not the
// lifetime of a response already on the wire. Active streams keep their
// provider idle timer after this guard is disarmed at response commitment.
type inferenceAttemptWindow struct {
	mu        sync.Mutex
	timer     *time.Timer
	cancel    context.CancelFunc
	settled   bool
	expired   bool
	committed bool
}

func newInferenceAttemptWindow(parent context.Context, deadline time.Time) (context.Context, *inferenceAttemptWindow) {
	ctx, cancel := context.WithCancel(parent)
	guard := &inferenceAttemptWindow{cancel: cancel}
	guard.timer = time.AfterFunc(max(0, time.Until(deadline)), func() {
		guard.mu.Lock()
		defer guard.mu.Unlock()
		if !guard.settled {
			guard.expired = true
			guard.settled = true
			guard.cancel()
		}
	})
	return ctx, guard
}

func (g *inferenceAttemptWindow) commit() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.expired {
		return false
	}
	g.settled = true
	g.committed = true
	g.timer.Stop()
	return true
}

func (g *inferenceAttemptWindow) close() (expired, committed bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.settled = true
	g.timer.Stop()
	g.cancel()
	return g.expired, g.committed
}

type inferenceWindowWriter struct {
	http.ResponseWriter
	guard *inferenceAttemptWindow
}

func (w *inferenceWindowWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *inferenceWindowWriter) WriteHeader(status int) {
	if w.guard.commit() {
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *inferenceWindowWriter) Write(body []byte) (int, error) {
	if !w.guard.commit() {
		return 0, context.DeadlineExceeded
	}
	return w.ResponseWriter.Write(body)
}
func (w *inferenceWindowWriter) Flush() {
	if !w.guard.commit() {
		return
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func restoreInferenceHeaders(dst, saved http.Header) {
	clear(dst)
	for key, values := range saved {
		dst[key] = append([]string(nil), values...)
	}
}

// Count identities, not mappings: an OAuth provider's repeated zero Key ID is
// one credential, while multiple API keys can each survive another key's fault.
func inferenceCredentialCount(routes []resolvedRoute) int {
	ids := map[int64]bool{}
	for _, z := range routes {
		ids[z.ProviderKeyID] = true
	}
	return len(ids)
}

// Preserve one attempt for each later channel while there is enough budget.
// The global cap remains authoritative; a cap of one still permits only one
// call. Spare attempts stay available to the current channel's other keys.
func inferenceChannelBudget(configured, identities, remaining, laterChannels int) int {
	budget := max(configured, identities)
	reserved := min(laterChannels, max(0, remaining-1))
	return min(budget, max(0, remaining-reserved))
}
