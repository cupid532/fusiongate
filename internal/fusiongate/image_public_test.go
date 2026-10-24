package fusiongate

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublicImageEditsMultipart(t *testing.T) {
	for _, codex := range []bool{false, true} {
		name := "native"
		if codex {
			name = "codex"
		}
		t.Run(name, func(t *testing.T) {
			encoded := base64.StdEncoding.EncodeToString([]byte("image-result"))
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if codex {
					if r.URL.Path != "/responses" {
						t.Errorf("path=%s", r.URL.Path)
					}
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					if body["model"] != "gpt-5.5" {
						t.Errorf("model=%v", body["model"])
					}
					input := body["input"].([]any)[0].(map[string]any)["content"].([]any)
					if len(input) != 2 || input[1].(map[string]any)["image_url"] != "data:image/png;base64,"+base64.StdEncoding.EncodeToString([]byte("reference")) {
						t.Errorf("input=%v", input)
					}
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"image_generation_call\",\"result\":\""+encoded+"\"}}\n\n")
				} else {
					if r.URL.Path != "/v1/images/edits" {
						t.Errorf("path=%s", r.URL.Path)
					}
					if err := r.ParseMultipartForm(maxPassthroughBody); err != nil {
						t.Fatal(err)
					}
					defer r.MultipartForm.RemoveAll()
					if r.FormValue("model") != "upstream-image" || r.FormValue("prompt") != "edit cat" {
						t.Errorf("fields=%v", r.Form)
					}
					f, _, err := r.FormFile("image[]")
					if err != nil {
						t.Fatal(err)
					}
					defer f.Close()
					data, _ := io.ReadAll(f)
					if string(data) != "reference" {
						t.Errorf("reference changed")
					}
					io.WriteString(w, `{"data":[{"b64_json":"`+encoded+`"}]}`)
				}
			}))
			defer upstream.Close()
			a, err := New(testConfig(t))
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			var provider int64
			if codex {
				provider = insertCodexOAuthTestProvider(t, a, upstream.URL)
				insertTestRoute(t, a, provider, "gpt-image-2", "gpt-5.5", "image", 10)
			} else {
				provider = insertTestProvider(t, a, "image", "openai_compatible", upstream.URL, "test-secret", 10, 100, "normalized", "any", 0, 3, 30)
				insertTestRoute(t, a, provider, "gpt-image-2", "upstream-image", "image", 10)
			}
			key := insertTestKey(t, a, true)
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			writer.WriteField("model", "gpt-image-2")
			writer.WriteField("prompt", "edit cat")
			writer.WriteField("n", "1")
			part, _ := writer.CreatePart(map[string][]string{"Content-Disposition": {`form-data; name="image[]"; filename="cat.png"`}, "Content-Type": {"image/png"}})
			part.Write([]byte("reference"))
			writer.Close()
			req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
			req.Header.Set("Content-Type", writer.FormDataContentType())
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("Origin", "https://canvas.codelee.de")
			rec := httptest.NewRecorder()
			a.Router().ServeHTTP(rec, req)
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), encoded) || calls != 1 {
				t.Fatalf("code=%d body=%s calls=%d", rec.Code, rec.Body.String(), calls)
			}
			if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
				t.Fatal("missing CORS")
			}
			req = httptest.NewRequest(http.MethodOptions, "/v1/images/edits", nil)
			req.Header.Set("Origin", "https://canvas.codelee.de")
			rec = httptest.NewRecorder()
			a.Router().ServeHTTP(rec, req)
			if rec.Code != 204 {
				t.Fatalf("preflight=%d", rec.Code)
			}
		})
	}
}

func TestPublicCodexImageGeneration(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("path=%s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"image_generation_call\",\"result\":\"aW1hZ2U=\"}}\n\n")
	}))
	defer upstream.Close()
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	id := insertCodexOAuthTestProvider(t, a, upstream.URL)
	insertTestRoute(t, a, id, "gpt-image-2", "gpt-5.5", "image", 10)
	rec := gatewayRequest(t, a, "/v1/images/generations", insertTestKey(t, a, true), `{"model":"gpt-image-2","prompt":"cat","n":1}`, "browser/1")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "aW1hZ2U=") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
