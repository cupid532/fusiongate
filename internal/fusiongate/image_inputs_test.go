package fusiongate

import (
	"bytes"
	"mime/multipart"
	"strings"
	"testing"
)

func TestCodexImageInputRejectsUnsupportedReferences(t *testing.T) {
	for _, field := range []string{"mask", "file", "image[]"} {
		t.Run(field, func(t *testing.T) {
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			part, err := writer.CreateFormFile(field, "input.bin")
			if err != nil {
				t.Fatal(err)
			}
			part.Write([]byte("not an image"))
			writer.Close()
			if _, err := codexImageInput(body.Bytes(), writer.FormDataContentType()); err == nil {
				t.Fatal("unsupported input silently accepted")
			}
		})
	}
}

func TestImageEditsRequiresImagePermission(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	key := insertTestKey(t, a, false)
	rec := gatewayRequest(t, a, "/v1/images/edits", key, `{"model":"gpt-image-2","prompt":"cat"}`, "browser/1")
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "images_not_allowed") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
