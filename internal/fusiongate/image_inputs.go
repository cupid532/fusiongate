package fusiongate

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
)

// codexImageInput converts the Image API's multipart references to Responses
// image inputs. Native image channels keep the original multipart wire bytes.
func codexImageInput(raw []byte, contentType string) ([]byte, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" {
		return raw, nil
	}
	reader := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
	source := map[string]any{}
	images := []any{}
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(part)
		if err != nil {
			return nil, err
		}
		name := part.FormName()
		switch name {
		case "image", "image[]":
			kind := part.Header.Get("Content-Type")
			if kind == "" || kind == "application/octet-stream" {
				kind = http.DetectContentType(data)
			}
			if !strings.HasPrefix(kind, "image/") {
				return nil, errors.New("reference must be an image")
			}
			images = append(images, "data:"+kind+";base64,"+base64.StdEncoding.EncodeToString(data))
		case "mask":
			return nil, errors.New("Codex image editing does not support masks; use an OpenAI-compatible image channel")
		default:
			if part.FileName() != "" {
				return nil, errors.New("unsupported image file field")
			}
			if name == "n" || name == "output_compression" || name == "partial_images" {
				var number json.Number
				if err := json.Unmarshal(data, &number); err != nil {
					return nil, err
				}
				source[name] = number
			} else {
				source[name] = string(data)
			}
		}
	}
	if len(images) == 0 {
		return nil, errors.New("image is required for image editing")
	}
	source["_reference_images"] = images
	return json.Marshal(source)
}

func rewriteImageMultipartModel(raw []byte, contentType, model string) ([]byte, error) {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, err
	}
	reader := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
	var out bytes.Buffer
	writer := multipart.NewWriter(&out)
	if err := writer.SetBoundary(params["boundary"]); err != nil {
		return nil, err
	}
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		target, err := writer.CreatePart(part.Header)
		if err != nil {
			return nil, err
		}
		if part.FormName() == "model" {
			_, err = io.WriteString(target, model)
		} else {
			_, err = io.Copy(target, part)
		}
		if err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
