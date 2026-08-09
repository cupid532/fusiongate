package fusiongate

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A client can put anything in X-Forwarded-For. The gateway must report the address
// its own proxy observed, not the one the caller asked for.
func TestRequestClientIPIgnoresForgedForwardedEntries(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		forwarded  string
		want       string
	}{
		{"single proxy hop", "127.0.0.1:1234", "203.0.113.8", "203.0.113.8"},
		{"client forged a leading entry", "127.0.0.1:1234", "9.9.9.9, 203.0.113.8", "203.0.113.8"},
		{"trusted hops appended after the client", "127.0.0.1:1234", "203.0.113.8, 127.0.0.1", "203.0.113.8"},
		{"docker bridge peer", "172.25.0.1:4321", "203.0.113.8, 10.0.0.5", "203.0.113.8"},
		{"forged entry only, no public hop", "127.0.0.1:1234", "10.1.2.3", "127.0.0.1"},
		{"public peer ignores the header entirely", "198.51.100.7:4321", "9.9.9.9", "198.51.100.7"},
		{"garbage header falls back to the peer", "127.0.0.1:1234", "not-an-ip", "127.0.0.1"},
		{"no header at all", "127.0.0.1:1234", "", "127.0.0.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
			r.RemoteAddr = tc.remoteAddr
			if tc.forwarded != "" {
				r.Header.Set("X-Forwarded-For", tc.forwarded)
			}
			if got := requestClientIP(r); got != tc.want {
				t.Fatalf("client IP = %q, want %q", got, tc.want)
			}
		})
	}
}

// A fixed window lets a caller spend the whole limit just before the boundary and the
// whole limit again just after, so twice the configured rate gets through.
func TestRateLimitDoesNotAllowDoubleRateAcrossTheBoundary(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	key := authKey{Hash: "sliding", RPMLimit: 10}

	allowed := 0
	for range 10 {
		if a.allowRate(key) {
			allowed++
		}
	}
	if allowed != 10 {
		t.Fatalf("first burst allowed %d of 10", allowed)
	}
	if a.allowRate(key) {
		t.Fatal("the limit was exceeded within one window")
	}

	// Step just past the window boundary. A fixed window would reset completely here.
	a.mu.Lock()
	a.rate[key.Hash].At = time.Now().Add(-61 * time.Second)
	a.mu.Unlock()

	afterBoundary := 0
	for range 10 {
		if a.allowRate(key) {
			afterBoundary++
		}
	}
	if afterBoundary >= 10 {
		t.Fatalf("allowed %d immediately after the boundary, expected the previous window to still count", afterBoundary)
	}

	// Once a full window has genuinely passed, the key recovers completely.
	a.mu.Lock()
	a.rate[key.Hash].At = time.Now().Add(-3 * time.Minute)
	a.mu.Unlock()
	recovered := 0
	for range 10 {
		if a.allowRate(key) {
			recovered++
		}
	}
	if recovered != 10 {
		t.Fatalf("after a full idle window the key allowed %d of 10", recovered)
	}
}

func TestPermissionMatchingSemanticsAreUnchanged(t *testing.T) {
	cases := []struct {
		patterns, model string
		want            bool
	}{
		{"gpt-4.1", "gpt-4.1", true},
		{"gpt-4.1", "gpt-4.2", false},
		{"gpt-*", "gpt-4.1", true},
		{"*", "anything", true},
		{"", "gpt-4.1", false},
		{" gpt-4.1 , claude-3 ", "claude-3", true},
		{"a,,b", "b", true},
		{"gpt-*,claude-*", "claude-3", true},
	}
	for _, tc := range cases {
		if got := matches(tc.patterns, tc.model); got != tc.want {
			t.Fatalf("matches(%q,%q)=%v, want %v", tc.patterns, tc.model, got, tc.want)
		}
	}
	if !matchesCapability("chat, image", "IMAGE") {
		t.Fatal("capability matching lost its case-insensitive comparison")
	}
	if matchesCapability("chat", "image") {
		t.Fatal("capability matching accepted a capability that is not present")
	}
	if !matchesCapability("chat", "") {
		t.Fatal("an empty requirement must always match")
	}
}

func BenchmarkPermissionCheck(b *testing.B) {
	key := authKey{AllowModels: "gpt-4.1,gpt-5,claude-*,gemini-2.5-pro", DenyModels: "gpt-3.5*"}
	for b.Loop() {
		allowed(key, "claude-sonnet-4.5")
	}
}
