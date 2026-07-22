package fusiongate

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAPIKeyRevealAndLegacyBehavior(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	createReq := httptest.NewRequest(http.MethodPost, "/api/admin/keys", strings.NewReader(`{
		"name":"Desktop Client",
		"allow_all":false,
		"allow_models":"GPT-5,GPT-5,gpt-5-mini",
		"deny_models":"GPT-5-MINI",
		"allow_images":false,
		"rpm_limit":60
	}`))
	createRec := httptest.NewRecorder()
	a.keys(createRec, createReq, adminCtx{})
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	var created struct {
		ID  int64  `json:"id"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Key == "" {
		t.Fatal("created key was not returned")
	}

	var encrypted []byte
	var allowModels, denyModels string
	if err := a.db.QueryRow(`SELECT encrypted_key,allow_models,deny_models FROM api_keys WHERE id=?`, created.ID).Scan(&encrypted, &allowModels, &denyModels); err != nil {
		t.Fatal(err)
	}
	if len(encrypted) == 0 || strings.Contains(string(encrypted), created.Key) {
		t.Fatal("API key was not encrypted at rest")
	}
	if allowModels != "gpt-5,gpt-5-mini" || denyModels != "gpt-5-mini" {
		t.Fatalf("normalized permissions allow=%q deny=%q", allowModels, denyModels)
	}

	listRec := httptest.NewRecorder()
	a.keys(listRec, httptest.NewRequest(http.MethodGet, "/api/admin/keys", nil), adminCtx{})
	if listRec.Code != http.StatusOK || strings.Contains(listRec.Body.String(), created.Key) {
		t.Fatalf("key list leaked plaintext: status=%d body=%s", listRec.Code, listRec.Body.String())
	}
	var listed []APIKey
	if err := json.Unmarshal(listRec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || !listed[0].CanReveal {
		t.Fatalf("listed keys=%+v", listed)
	}

	revealRec := httptest.NewRecorder()
	a.keyByID(revealRec, httptest.NewRequest(http.MethodPost, "/api/admin/keys/"+intString(created.ID)+"/reveal", nil), adminCtx{})
	if revealRec.Code != http.StatusOK {
		t.Fatalf("reveal status=%d body=%s", revealRec.Code, revealRec.Body.String())
	}
	var revealed map[string]string
	if err := json.Unmarshal(revealRec.Body.Bytes(), &revealed); err != nil {
		t.Fatal(err)
	}
	if revealed["key"] != created.Key {
		t.Fatalf("revealed key mismatch")
	}

	authReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	authReq.Header.Set("Authorization", "Bearer "+created.Key)
	if _, ok := a.authenticateKey(authReq); !ok {
		t.Fatal("revealed key no longer authenticates")
	}

	legacy := "fg_legacy_unrecoverable"
	sum := sha256.Sum256([]byte(legacy))
	res, err := a.db.Exec(`INSERT INTO api_keys(name,key_prefix,key_hash,allow_all,allow_models,deny_models,allow_images,rpm_limit,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, "legacy", legacy[:11], hex.EncodeToString(sum[:]), 1, "", "", 0, 120, now())
	if err != nil {
		t.Fatal(err)
	}
	legacyID, _ := res.LastInsertId()
	legacyRec := httptest.NewRecorder()
	a.keyByID(legacyRec, httptest.NewRequest(http.MethodPost, "/api/admin/keys/"+intString(legacyID)+"/reveal", nil), adminCtx{})
	if legacyRec.Code != http.StatusConflict || !strings.Contains(legacyRec.Body.String(), "key_not_recoverable") {
		t.Fatalf("legacy reveal status=%d body=%s", legacyRec.Code, legacyRec.Body.String())
	}
}

func TestLiveRequestLedgerAndFirstByteTiming(t *testing.T) {
	upstreamStarted := make(chan struct{})
	releaseFirstByte := make(chan struct{}, 1)
	finishResponse := make(chan struct{}, 1)
	t.Cleanup(func() {
		select {
		case releaseFirstByte <- struct{}{}:
		default:
		}
		select {
		case finishResponse <- struct{}{}:
		default:
		}
	})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(upstreamStarted)
		<-releaseFirstByte
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[]}\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-finishResponse
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "slow-stream", "openai_compatible", upstream.URL, "secret", 1, 1, "normalized", "any", 0, 3, 30)
	insertTestRoute(t, a, providerID, "live-model", "upstream-model", "chat,stream", 1)
	key := insertTestKey(t, a, false)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"live-model","stream":true,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		a.Router().ServeHTTP(rec, req)
		close(done)
	}()
	select {
	case <-upstreamStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream was not reached")
	}

	initial := requestListForTest(t, a)
	if len(initial) != 1 || initial[0].Running != true || initial[0].CompletedAt != "" || initial[0].FirstByteMS.Valid {
		t.Fatalf("initial ledger=%+v", initial)
	}

	time.Sleep(80 * time.Millisecond)
	releaseFirstByte <- struct{}{}
	deadline := time.Now().Add(2 * time.Second)
	for {
		var first sql.NullInt64
		if err := a.db.QueryRow(`SELECT first_byte_ms FROM request_ledger LIMIT 1`).Scan(&first); err != nil {
			t.Fatal(err)
		}
		if first.Valid {
			if first.Int64 < 50 {
				t.Fatalf("first_byte_ms=%d, expected delayed first byte", first.Int64)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first byte timing was not recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}

	mid := requestListForTest(t, a)
	if len(mid) != 1 || !mid[0].Running || !mid[0].FirstByteMS.Valid {
		t.Fatalf("mid-flight ledger=%+v", mid)
	}
	finishResponse <- struct{}{}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("gateway request did not finish")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("gateway status=%d body=%s", rec.Code, rec.Body.String())
	}

	final := requestListForTest(t, a)
	if len(final) != 1 || final[0].Running || final[0].CompletedAt == "" || !final[0].Success || !final[0].FirstByteMS.Valid {
		t.Fatalf("final ledger=%+v", final)
	}
	if final[0].LatencyMS < final[0].FirstByteMS.Int64 {
		t.Fatalf("latency=%d first_byte=%d", final[0].LatencyMS, final[0].FirstByteMS.Int64)
	}
}

type requestListRow struct {
	Running     bool          `json:"running"`
	CompletedAt string        `json:"completed_at"`
	FirstByteMS sql.NullInt64 `json:"-"`
	Success     bool          `json:"success"`
	LatencyMS   int64         `json:"latency_ms"`
}

func requestListForTest(t *testing.T, a *App) []requestListRow {
	t.Helper()
	rec := httptest.NewRecorder()
	a.requests(rec, httptest.NewRequest(http.MethodGet, "/api/admin/requests", nil), adminCtx{})
	if rec.Code != http.StatusOK {
		t.Fatalf("requests status=%d body=%s", rec.Code, rec.Body.String())
	}
	var raw []struct {
		Running     bool   `json:"running"`
		CompletedAt string `json:"completed_at"`
		FirstByteMS *int64 `json:"first_byte_ms"`
		Success     bool   `json:"success"`
		LatencyMS   int64  `json:"latency_ms"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	out := make([]requestListRow, 0, len(raw))
	for _, row := range raw {
		converted := requestListRow{Running: row.Running, CompletedAt: row.CompletedAt, Success: row.Success, LatencyMS: row.LatencyMS}
		if row.FirstByteMS != nil {
			converted.FirstByteMS = sql.NullInt64{Int64: *row.FirstByteMS, Valid: true}
		}
		out = append(out, converted)
	}
	return out
}

func intString(v int64) string {
	return strconv.FormatInt(v, 10)
}
