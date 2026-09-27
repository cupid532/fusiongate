package fusiongate

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPassthroughDiscoveryDoesNotGenerate(t *testing.T) {
	var generation atomic.Int32
	a, _, done := passthroughFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/models") {
			generation.Add(1)
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"claude-native","type":"model"}]}`))
	})
	defer done()
	p, err := a.loadDiscoveryProvider(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	p.Type = "anthropic"
	models, _, err := a.fetchDiscoveryCandidate(context.Background(), p)
	if err != nil || len(models) != 1 || generation.Load() != 0 {
		t.Fatalf("models=%v generation=%d err=%v", models, generation.Load(), err)
	}
}
