package fusiongate

import "testing"

func TestJoinEndpointPathAvoidsDuplicateVersionPrefix(t *testing.T) {
	cases := []struct {
		name     string
		basePath string
		endpoint string
		want     string
	}{
		{"bare origin", "", "/v1/chat/completions", "/v1/chat/completions"},
		{"origin with trailing slash", "/", "/v1/chat/completions", "/v1/chat/completions"},
		{"opencode zen go", "/zen/go/v1", "/v1/chat/completions", "/zen/go/v1/chat/completions"},
		{"opencode zen paid", "/zen/v1", "/v1/responses", "/zen/v1/responses"},
		{"standard openai", "/v1", "/v1/messages", "/v1/messages"},
		{"api/v1 prefix", "/api/v1", "/v1/chat/completions", "/api/v1/chat/completions"},
		{"v1beta endpoint", "/v1beta", "/v1beta/models/x:generateContent", "/v1beta/models/x:generateContent"},
		{"no version in base", "/some/path", "/v1/chat/completions", "/some/path/v1/chat/completions"},
		{"endpoint without slash", "/zen/go/v1", "chat/completions", "/zen/go/v1/chat/completions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := joinEndpointPath(tc.basePath, tc.endpoint); got != tc.want {
				t.Fatalf("joinEndpointPath(%q, %q) = %q, want %q", tc.basePath, tc.endpoint, got, tc.want)
			}
		})
	}
}

func TestJoinURLQueryDeduplicatesV1(t *testing.T) {
	got, err := joinURLQuery("https://opencode.ai/zen/go/v1", "/v1/chat/completions", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://opencode.ai/zen/go/v1/chat/completions" {
		t.Fatalf("upstream URL = %q", got)
	}
}
