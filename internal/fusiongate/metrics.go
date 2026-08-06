package fusiongate

import (
	"sync/atomic"
	"time"
)

type gatewayMetrics struct {
	startedAt       time.Time
	active          atomic.Int64
	requests        atomic.Int64
	attempts        atomic.Int64
	failovers       atomic.Int64
	completed       atomic.Int64
	successes       atomic.Int64
	failures        atomic.Int64
	overloaded      atomic.Int64
	firstByteCount  atomic.Int64
	firstByteMillis atomic.Int64
}

func newGatewayMetrics() gatewayMetrics {
	return gatewayMetrics{startedAt: time.Now().UTC()}
}

func (a *App) tryAcquireRequestSlot() bool {
	if a.requestSlots == nil {
		return true
	}
	select {
	case a.requestSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (a *App) releaseRequestSlot() {
	if a.requestSlots == nil {
		return
	}
	select {
	case <-a.requestSlots:
	default:
	}
}

func (m *gatewayMetrics) snapshot() map[string]any {
	firstByteCount := m.firstByteCount.Load()
	averageFirstByte := float64(0)
	if firstByteCount > 0 {
		averageFirstByte = float64(m.firstByteMillis.Load()) / float64(firstByteCount)
	}
	return map[string]any{
		"started_at":              m.startedAt,
		"active_requests":         m.active.Load(),
		"requests_total":          m.requests.Load(),
		"upstream_attempts_total": m.attempts.Load(),
		"failover_attempts_total": m.failovers.Load(),
		"completed_total":         m.completed.Load(),
		"successes_total":         m.successes.Load(),
		"failures_total":          m.failures.Load(),
		"overloaded_total":        m.overloaded.Load(),
		"first_byte_count":        firstByteCount,
		"average_first_byte_ms":   averageFirstByte,
	}
}
