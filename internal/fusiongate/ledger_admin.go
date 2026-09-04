package fusiongate

import (
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// ledgerMaxMBKey persists the request-ledger capacity cap (in MB) in settings.
	ledgerMaxMBKey      = "ledger_max_mb"
	ledgerMaxMBDefault  = 100
	ledgerMaxMBMin      = 1
	ledgerMaxMBMax      = 10240 // 10 GB
	ledgerTrimBatchSize = 2000  // rows removed per capacity-trim chunk
	ledgerRowOverhead   = 600   // estimated bytes/row for btree pages, indexes and WAL slack
	ledgerExportLimit   = 200000
)

// ledgerMaxMB reads the configured capacity cap, falling back to the default.
func (a *App) ledgerMaxMB() int64 {
	var v string
	if err := a.reader().QueryRow(`SELECT value FROM settings WHERE key=?`, ledgerMaxMBKey).Scan(&v); err != nil {
		return ledgerMaxMBDefault
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n < ledgerMaxMBMin || n > ledgerMaxMBMax {
		return ledgerMaxMBDefault
	}
	return n
}

// ledgerUsage estimates the on-disk footprint (bytes) of request_ledger and its row count.
func (a *App) ledgerUsage() (estBytes, rows int64, err error) {
	cols := []string{
		"request_id", "created_at", "completed_at", "public_model", "upstream_model",
		"protocol", "error_type", "cost_type", "gateway_request_id", "retry_reason",
		"client_ip", "api_key_name", "api_key_prefix", "provider_name", "reasoning_effort",
	}
	parts := make([]string, 0, len(cols))
	for _, c := range cols {
		parts = append(parts, fmt.Sprintf("COALESCE(LENGTH(%s),0)", c))
	}
	textSQL := strings.Join(parts, "+")
	if err = a.reader().QueryRow(`SELECT COUNT(*),COALESCE(`+textSQL+`,0) FROM request_ledger`).Scan(&rows, &estBytes); err != nil {
		return 0, 0, err
	}
	estBytes += rows * ledgerRowOverhead
	return estBytes, rows, nil
}

// ledger handles GET (status) and PUT (update capacity cap) on /api/admin/ledger.
func (a *App) ledger(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	switch r.Method {
	case http.MethodGet:
		estBytes, rows, err := a.ledgerUsage()
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		maxMB := a.ledgerMaxMB()
		writeJSON(w, http.StatusOK, map[string]any{
			"max_mb":    maxMB,
			"used_mb":   mbCeil(estBytes),
			"est_bytes": estBytes,
			"rows":      rows,
			"capped":    estBytes > maxMB*1_000_000,
		})
	case http.MethodPut:
		var in struct {
			MaxMB *int64 `json:"max_mb"`
		}
		if err := readJSON(r, &in); err != nil || in.MaxMB == nil {
			fail(w, http.StatusBadRequest, "invalid_request", "max_mb is required")
			return
		}
		maxMB := *in.MaxMB
		if maxMB < ledgerMaxMBMin || maxMB > ledgerMaxMBMax {
			fail(w, http.StatusBadRequest, "invalid_capacity", fmt.Sprintf("max_mb must be between %d and %d", ledgerMaxMBMin, ledgerMaxMBMax))
			return
		}
		if _, err := a.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			ledgerMaxMBKey, strconv.FormatInt(maxMB, 10)); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		// Trim now so lowering the cap takes effect immediately.
		if err := a.pruneLedgerToCapacity(r.Context()); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		estBytes, rows, err := a.ledgerUsage()
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"max_mb": maxMB, "used_mb": mbCeil(estBytes), "est_bytes": estBytes, "rows": rows,
		})
	default:
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET or PUT required")
	}
}

// ledgerClear empties the request ledger.
func (a *App) ledgerClear(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	a.ledgerCleanupMu.Lock()
	_, err := a.db.Exec(`DELETE FROM request_ledger`)
	a.ledgerCleanupMu.Unlock()
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ledgerExport returns matching ledger rows as a JSON array, walking ascending id order
// with an optional ?since=<id> cursor, honoring the same filters as the list endpoint.
func (a *App) ledgerExport(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required")
		return
	}
	where := []string{"1=1"}
	args := []any{}
	for _, filter := range []struct{ name, operator string }{
		{"from", ">="}, {"to", "<="},
	} {
		value := strings.TrimSpace(r.URL.Query().Get(filter.name))
		if value == "" {
			continue
		}
		parsed, parseErr := time.Parse(time.RFC3339, value)
		if parseErr != nil {
			fail(w, http.StatusBadRequest, "invalid_time_filter", filter.name+" must be an RFC3339 timestamp")
			return
		}
		where = append(where, "l.created_at "+filter.operator+" ?")
		args = append(args, parsed.UTC().Format(time.RFC3339Nano))
	}
	if s := strings.TrimSpace(r.URL.Query().Get("since")); s != "" {
		id, parseErr := strconv.ParseInt(s, 10, 64)
		if parseErr != nil || id < 0 {
			fail(w, http.StatusBadRequest, "invalid_cursor", "since must be a non-negative integer id")
			return
		}
		where = append(where, "l.id > ?")
		args = append(args, id)
	}
	switch status := strings.TrimSpace(r.URL.Query().Get("status")); status {
	case "", "all":
	case "running":
		where = append(where, "l.completed_at IS NULL")
	case "success":
		where = append(where, "l.completed_at IS NOT NULL AND l.success=1")
	case "failed":
		where = append(where, "l.completed_at IS NOT NULL AND l.success=0")
	default:
		fail(w, http.StatusBadRequest, "invalid_status_filter", "status must be all, running, success, or failed")
		return
	}

	q := `SELECT l.id,l.request_id,l.gateway_request_id,l.attempt,l.retry_reason,l.created_at,COALESCE(l.completed_at,''),l.first_byte_ms,l.public_model,l.upstream_model,l.protocol,l.stream,l.success,l.status_code,l.error_type,l.latency_ms,l.input_tokens,l.output_tokens,l.cached_tokens,l.reasoning_tokens,l.cost_micros,l.cost_type,l.usage_reported,COALESCE(NULLIF(l.provider_name,''),p.name,''),l.client_ip,l.reasoning_effort,COALESCE(p.request_timeout_ms,0) AS request_timeout_ms
		FROM request_ledger l LEFT JOIN providers p ON p.id=l.provider_id WHERE ` + strings.Join(where, " AND ") + ` ORDER BY l.id ASC LIMIT ?`
	pageArgs := append([]any{}, args...)
	pageArgs = append(pageArgs, ledgerExportLimit)
	rows, err := a.reader().Query(q, pageArgs...)
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	defer rows.Close()

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="fusiongate-requests.csv"`)
	w.WriteHeader(http.StatusOK)
	// BOM so Excel opens UTF-8 columns correctly.
	w.Write([]byte{0xEF, 0xBB, 0xBF})
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"时间", "完成时间", "请求ID", "模型", "上游模型", "渠道", "协议", "流式", "状态", "HTTP状态码", "错误类型", "思考强度", "首字节(ms)", "延迟(ms)", "输入Token", "输出Token", "缓存Token", "思考Token", "总Token", "费用(微)", "费用类型", "客户端IP", "重试原因"})
	for rows.Next() {
		var id, attempt, stream, success, status, latency, usageReported int
		var rid, gatewayID, retryReason, created, completed, pm, um, proto, et, ct, providerName, clientIP, reasoningEffort string
		var firstByte sql.NullInt64
		var input, output, cached, reasoning, cost int64
		var requestTimeoutMS int64
		if err := rows.Scan(&id, &rid, &gatewayID, &attempt, &retryReason, &created, &completed, &firstByte, &pm, &um, &proto, &stream, &success, &status, &et, &latency, &input, &output, &cached, &reasoning, &cost, &ct, &usageReported, &providerName, &clientIP, &reasoningEffort, &requestTimeoutMS); err != nil {
			break
		}
		fb := ""
		if firstByte.Valid {
			fb = strconv.FormatInt(firstByte.Int64, 10)
		}
		statusLabel := "失败"
		if completed == "" {
			statusLabel = "进行中"
		} else if strBool(success) {
			statusLabel = "成功"
		}
		_ = cw.Write([]string{
			created, completed, rid, pm, um, providerName, proto,
			boolLabel(strBool(stream)), statusLabel, strconv.Itoa(status), et,
			reasoningEffort, fb, strconv.Itoa(latency),
			strconv.FormatInt(input, 10), strconv.FormatInt(output, 10),
			strconv.FormatInt(cached, 10), strconv.FormatInt(reasoning, 10),
			strconv.FormatInt(input+output, 10), strconv.FormatInt(cost, 10), ct,
			clientIP, retryReason,
		})
	}
	cw.Flush()
}

func boolLabel(b bool) string {
	if b {
		return "是"
	}
	return "否"
}

func mbCeil(bytes int64) int64 {
	if bytes <= 0 {
		return 0
	}
	return (bytes + 999_999) / 1_000_000
}

// pruneLedgerToCapacity trims the oldest ledger rows until estimated usage is within the cap.
// It acquires the ledger cleanup lock; use pruneLedgerToCapacityLocked when already holding it.
func (a *App) pruneLedgerToCapacity(ctx context.Context) error {
	a.ledgerCleanupMu.Lock()
	defer a.ledgerCleanupMu.Unlock()
	return a.pruneLedgerToCapacityLocked(ctx)
}

// pruneLedgerToCapacityLocked must be called with a.ledgerCleanupMu held.
func (a *App) pruneLedgerToCapacityLocked(ctx context.Context) error {
	maxBytes := a.ledgerMaxMB() * 1_000_000
	for {
		estBytes, _, err := a.ledgerUsage()
		if err != nil {
			return err
		}
		if estBytes <= maxBytes {
			return nil
		}
		res, err := a.db.ExecContext(ctx, `DELETE FROM request_ledger
			WHERE id IN (SELECT id FROM request_ledger ORDER BY created_at ASC, id ASC LIMIT ?)`, ledgerTrimBatchSize)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
	}
}
