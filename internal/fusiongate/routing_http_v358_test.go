package fusiongate

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func v358HTTPRoutingFixture(t *testing.T, handler http.HandlerFunc) (*App, string, []int64) {
	t.Helper()
	app, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close() })
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	key := insertTestKey(t, app, true)
	var providers []int64
	for index, name := range []string{"A", "B", "C"} {
		id := insertTestProvider(t, app, name, "openai_compatible", server.URL, name, 3-index, 1, "normalized", "any", 0, 100, 30)
		insertTestRoute(t, app, id, "v358-http-model", "v358-http-model", "chat,stream", 0)
		providers = append(providers, id)
	}
	// One call per channel makes completion order a barrier, not retry timing.
	if _, err := app.db.Exec(`INSERT INTO settings(key,value) VALUES('inference_settings','{"channel_attempts":1}')`); err != nil {
		t.Fatal(err)
	}
	return app, key, providers
}

func v358HTTPRequest(app *App, key, task, text string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"v358-http-model","messages":[{"role":"user","content":%q}]}`, text)))
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(taskIDHeader, task)
	recorder := httptest.NewRecorder()
	app.Router().ServeHTTP(recorder, request)
	return recorder
}

func v358HTTPSession(t *testing.T, app *App, task string) routingSession {
	t.Helper()
	var keyID int64
	if err := app.db.QueryRow(`SELECT id FROM api_keys`).Scan(&keyID); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	request.Header.Set(taskIDHeader, task)
	hash, _, _ := inferenceTaskIdentity(request, authKey{ID: keyID}, app.keyMaterial)
	session, ok := app.loadRoutingSession(context.Background(), hash, inferenceTaskScopeFor("v358-http-model", passthroughProtocol(request.URL.Path)))
	if !ok {
		t.Fatal("successful identified request did not bind a session")
	}
	return session
}

func v358HTTPAnswer(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, name)
}

// Both requests read A before the failing one advances to B. Completing the old
// A response afterwards must not affect the next real HTTP request's channel.
func TestV358RoutingHTTPStaleSuccessDoesNotUndoFailover(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	bStarted, bRelease := make(chan struct{}), make(chan struct{})
	var once, bOnce sync.Once
	defer once.Do(func() { close(release) })
	defer bOnce.Do(func() { close(bRelease) })
	var aCalls atomic.Int32
	app, key, providers := v358HTTPRoutingFixture(t, func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if name == "A" {
			aCalls.Add(1)
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), "warm") {
				v358HTTPAnswer(w, name)
				return
			}
			if strings.Contains(string(body), "slow") {
				close(started)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			} else {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"error":{"message":"temporary failure"}}`)
				return
			}
		}
		if name == "B" {
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), "fast") {
				close(bStarted)
				select {
				case <-bRelease:
				case <-r.Context().Done():
					return
				}
			}
		}
		v358HTTPAnswer(w, name)
	})
	// Start from the persisted A binding that both concurrent turns read.
	if rec := v358HTTPRequest(app, key, "http-concurrent-task", "warm"); rec.Code != 200 {
		t.Fatalf("initial binding status=%d", rec.Code)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- v358HTTPRequest(app, key, "http-concurrent-task", "slow") }()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("old request never reached A")
	}
	fastDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { fastDone <- v358HTTPRequest(app, key, "http-concurrent-task", "fast") }()
	select {
	case <-bStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("failover never reached B")
	}
	// A late success must not undo the persisted advance even before B has
	// completed and incremented the task's generation a second time.
	if session := v358HTTPSession(t, app, "http-concurrent-task"); session.ProviderID != providers[1] {
		t.Fatalf("failed-over task bound=%d, want B=%d", session.ProviderID, providers[1])
	}
	once.Do(func() { close(release) })
	select {
	case rec := <-done:
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"content":"A"`) {
			t.Fatalf("in-flight response changed: status=%d body=%s", rec.Code, rec.Body.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("old request did not complete after release")
	}
	if session := v358HTTPSession(t, app, "http-concurrent-task"); session.ProviderID != providers[1] {
		t.Errorf("late success undid persisted B advance: provider=%d, want B=%d", session.ProviderID, providers[1])
	}
	bOnce.Do(func() { close(bRelease) })
	select {
	case rec := <-fastDone:
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"content":"B"`) {
			t.Fatalf("fast request status=%d body=%s", rec.Code, rec.Body.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("B did not complete after release")
	}
	if rec := v358HTTPRequest(app, key, "http-concurrent-task", "next"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"content":"B"`) {
		t.Fatalf("next request lost B binding: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if aCalls.Load() != 3 {
		t.Fatalf("old channel was retried after failover: calls=%d, want warmup and exactly two concurrent calls", aCalls.Load())
	}
}

// Reprioritizing C above A/B must keep bound B, yet C remains reachable if B
// later fails. Re-enabling departed A cannot let it re-enter this task.
func TestV358RoutingHTTPPriorityReorderKeepsBoundAndUnvisited(t *testing.T) {
	var failB atomic.Bool
	var aCalls, bCalls, cCalls atomic.Int32
	app, key, providers := v358HTTPRoutingFixture(t, func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch name {
		case "A":
			aCalls.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		case "B":
			bCalls.Add(1)
			if failB.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		case "C":
			cCalls.Add(1)
		}
		v358HTTPAnswer(w, name)
	})
	assertServed := func(task, want string) {
		t.Helper()
		rec := v358HTTPRequest(app, key, task, "turn")
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"content":"`+want+`"`) {
			t.Fatalf("task=%s expected=%s status=%d body=%s", task, want, rec.Code, rec.Body.String())
		}
	}
	assertServed("http-reordered-task", "B")
	if _, err := app.db.Exec(`UPDATE providers SET priority=10 WHERE id=?`, providers[2]); err != nil {
		t.Fatal(err)
	}
	assertServed("http-reordered-task", "B")
	assertServed("http-fresh-task", "C")
	failB.Store(true)
	assertServed("http-reordered-task", "C")
	if _, err := app.db.Exec(`UPDATE providers SET enabled=0 WHERE id=?`, providers[0]); err != nil {
		t.Fatal(err)
	}
	assertServed("http-reordered-task", "C")
	if _, err := app.db.Exec(`UPDATE providers SET enabled=1,priority=20 WHERE id=?`, providers[0]); err != nil {
		t.Fatal(err)
	}
	assertServed("http-reordered-task", "C")
	if aCalls.Load() != 1 || bCalls.Load() != 3 || cCalls.Load() != 4 {
		t.Fatalf("unexpected request history A=%d B=%d C=%d; want 1,3,4", aCalls.Load(), bCalls.Load(), cCalls.Load())
	}
	if session := v358HTTPSession(t, app, "http-reordered-task"); session.ProviderID != providers[2] {
		t.Fatalf("task ended on provider=%d, want C=%d", session.ProviderID, providers[2])
	}
}
