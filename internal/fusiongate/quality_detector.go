package fusiongate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const qualityDetectorVersion = "4.0.1"

var qualityDetectorPresets = map[string]bool{"low": true, "medium": true, "high": true}
var qualityDetectorModels = map[string]bool{"gpt-5.6-sol": true, "gpt-5.6-terra": true, "gpt-5.6-luna": true}
var qualityDetectorSecret = regexp.MustCompile(`(?i)\b(?:fg|sk)-?_[A-Za-z0-9_-]{8,}|\bsk-[A-Za-z0-9_-]{8,}`)

type qualityDetectorClient struct {
	baseURL string
	client  *http.Client
}

func newQualityDetectorClient(rawURL string) (*qualityDetectorClient, error) {
	if strings.TrimSpace(rawURL) == "" {
		return nil, errors.New("quality detector is not configured")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed == nil {
		return nil, errors.New("quality detector URL must be a loopback HTTP endpoint")
	}
	host := strings.ToLower(parsed.Hostname())
	if parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (host != "127.0.0.1" && host != "localhost" && host != "::1") {
		return nil, errors.New("quality detector URL must be a loopback HTTP endpoint")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return &qualityDetectorClient{
		baseURL: parsed.String(),
		client: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
			Proxy:                 nil,
			MaxIdleConns:          8,
			MaxIdleConnsPerHost:   4,
			IdleConnTimeout:       60 * time.Second,
			ResponseHeaderTimeout: 25 * time.Second,
		}},
	}, nil
}

func (c *qualityDetectorClient) request(r *http.Request, method, requestPath string, body any, token string) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(r.Context(), method, c.baseURL+requestPath, reader)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("X-GPT56-Session", token)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	return data, response.StatusCode, err
}

func (c *qualityDetectorClient) bootstrap(r *http.Request) (map[string]any, error) {
	data, status, err := c.request(r, http.MethodGet, "/api/bootstrap", nil, "")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("detector bootstrap returned HTTP %d", status)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func qualityDetectorError(data []byte, status int) string {
	var value struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &value) == nil && strings.TrimSpace(value.Error) != "" {
		return qualityDetectorSecret.ReplaceAllString(value.Error, "<redacted-key>")
	}
	return fmt.Sprintf("quality detector returned HTTP %d", status)
}

func writeQualityDetectorResponse(w http.ResponseWriter, data []byte, status int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func (a *App) qualityDetector(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	client := a.qualityDetectorClient
	if client == nil {
		fail(w, http.StatusServiceUnavailable, "quality_detector_unavailable", "quality detector is not configured")
		return
	}
	action := strings.TrimPrefix(r.URL.Path, "/api/admin/quality-detector")
	switch action {
	case "":
		if r.Method != http.MethodGet {
			fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required")
			return
		}
		bootstrap, err := client.bootstrap(r)
		if err != nil {
			fail(w, http.StatusBadGateway, "quality_detector_unavailable", err.Error())
			return
		}
		estimates := map[string]any{}
		presets, _ := bootstrap["single_presets"].(map[string]any)
		for _, name := range []string{"low", "medium", "high"} {
			config, ok := presets[name]
			if !ok {
				continue
			}
			data, status, requestErr := client.request(r, http.MethodPost, "/api/detector/estimate", map[string]any{"config": config}, fmt.Sprint(bootstrap["session_token"]))
			if requestErr != nil || status != http.StatusOK {
				continue
			}
			var estimate any
			if json.Unmarshal(data, &estimate) == nil {
				estimates[name] = estimate
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"available": true, "version": qualityDetectorVersion, "estimates": estimates})
	case "/status", "/report":
		if r.Method != http.MethodGet {
			fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required")
			return
		}
		data, status, requestErr := client.request(r, http.MethodGet, "/api/detector"+action, nil, "")
		if requestErr != nil {
			fail(w, http.StatusBadGateway, "quality_detector_unavailable", requestErr.Error())
			return
		}
		writeQualityDetectorResponse(w, data, status)
	case "/start":
		if r.Method != http.MethodPost {
			fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
			return
		}
		var input struct {
			Preset string `json:"preset"`
			Model  string `json:"model"`
			APIKey string `json:"api_key"`
		}
		if readJSON(r, &input) != nil || !qualityDetectorPresets[input.Preset] || !qualityDetectorModels[input.Model] || strings.TrimSpace(input.APIKey) == "" || len(input.APIKey) > 512 {
			fail(w, http.StatusBadRequest, "invalid_request", "choose a supported preset and model and provide an API key")
			return
		}
		bootstrap, err := client.bootstrap(r)
		if err != nil {
			fail(w, http.StatusBadGateway, "quality_detector_unavailable", err.Error())
			return
		}
		presets, _ := bootstrap["single_presets"].(map[string]any)
		config, ok := presets[input.Preset]
		if !ok {
			fail(w, http.StatusBadGateway, "quality_detector_invalid", "detector preset is unavailable")
			return
		}
		baseURL := strings.TrimRight(a.cfg.QualityDetectorBaseURL, "/")
		if baseURL == "" {
			baseURL = "http://127.0.0.1:8787/v1"
		}
		data, status, requestErr := client.request(r, http.MethodPost, "/api/detector/start", map[string]any{
			"base_url": baseURL, "model": input.Model, "api_key": input.APIKey, "config": config,
			"retention_enabled": false, "retention_directory": nil,
		}, fmt.Sprint(bootstrap["session_token"]))
		if requestErr != nil {
			fail(w, http.StatusBadGateway, "quality_detector_unavailable", requestErr.Error())
			return
		}
		if status >= 400 {
			fail(w, status, "quality_detector_failed", qualityDetectorError(data, status))
			return
		}
		writeQualityDetectorResponse(w, data, status)
	case "/stop":
		if r.Method != http.MethodPost {
			fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
			return
		}
		bootstrap, err := client.bootstrap(r)
		if err != nil {
			fail(w, http.StatusBadGateway, "quality_detector_unavailable", err.Error())
			return
		}
		data, status, requestErr := client.request(r, http.MethodPost, "/api/detector/stop", map[string]any{}, fmt.Sprint(bootstrap["session_token"]))
		if requestErr != nil {
			fail(w, http.StatusBadGateway, "quality_detector_unavailable", requestErr.Error())
			return
		}
		if status >= 400 {
			fail(w, status, "quality_detector_failed", qualityDetectorError(data, status))
			return
		}
		writeQualityDetectorResponse(w, data, status)
	default:
		fail(w, http.StatusNotFound, "not_found", "quality detector action not found")
	}
}
