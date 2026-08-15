package fusiongate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const qualityDetectorVersion = "4.0.1"

var qualityDetectorPresets = map[string]bool{"low": true, "medium": true, "high": true}
var qualityDetectorSecret = regexp.MustCompile(`(?i)\b(?:fg|sk)-?_[A-Za-z0-9_-]{8,}|\bsk-[A-Za-z0-9_-]{8,}`)

const qualityDetectorRouteTTL = 2 * time.Hour
const qualityDetectorRouteMaxRequests = 512

type qualityDetectorTarget struct {
	ID              string `json:"id"`
	Model           string `json:"model"`
	RouteID         int64  `json:"route_id"`
	UpstreamModel   string `json:"upstream_model"`
	ProviderID      int64  `json:"provider_id"`
	ProviderName    string `json:"provider_name"`
	ProviderType    string `json:"provider_type"`
	ProviderKeyID   int64  `json:"provider_key_id"`
	ProviderKeyName string `json:"provider_key_name"`
	ProviderKeyHint string `json:"provider_key_hint"`
	CredentialKind  string `json:"credential_kind"`
}

type qualityDetectorRouteSession struct {
	Target    qualityDetectorTarget
	ExpiresAt time.Time
	Remaining int
}

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

func qualityDetectorProviderSupported(providerType string) bool {
	switch providerType {
	case "openai", "grok", "openrouter", "openai_compatible", "opencode", "codex_oauth", "grok_oauth":
		return true
	default:
		return false
	}
}

func qualityDetectorTargetID(routeID, providerKeyID int64) string {
	return strconv.FormatInt(routeID, 10) + ":" + strconv.FormatInt(providerKeyID, 10)
}

func (a *App) qualityDetectorTargets(ctx context.Context) ([]qualityDetectorTarget, error) {
	type keyLabel struct {
		name string
		hint string
	}
	rows, err := a.reader().Query(`SELECT id,name,key_hint FROM provider_api_keys WHERE enabled=1`)
	if err != nil {
		return nil, err
	}
	labels := map[int64]keyLabel{}
	for rows.Next() {
		var id int64
		var label keyLabel
		if err := rows.Scan(&id, &label.name, &label.hint); err != nil {
			rows.Close()
			return nil, err
		}
		labels[id] = label
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	targets := make([]qualityDetectorTarget, 0)
	for _, model := range []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"} {
		routes, resolveErr := a.resolve(ctx, model, "chat")
		if resolveErr != nil {
			continue
		}
		for _, route := range routes {
			if !qualityDetectorProviderSupported(route.Provider.Type) {
				continue
			}
			if route.Provider.Type == "opencode" && opencodeRouteProtocol(route) != opencodeProtocolResponses {
				continue
			}
			target := qualityDetectorTarget{
				ID:             qualityDetectorTargetID(route.Route.ID, route.ProviderKeyID),
				Model:          model,
				RouteID:        route.Route.ID,
				UpstreamModel:  route.Route.UpstreamModel,
				ProviderID:     route.Provider.ID,
				ProviderName:   route.Provider.Name,
				ProviderType:   route.Provider.Type,
				ProviderKeyID:  route.ProviderKeyID,
				CredentialKind: "channel_credential",
			}
			if route.ProviderKeyID > 0 {
				label := labels[route.ProviderKeyID]
				target.ProviderKeyName = label.name
				target.ProviderKeyHint = label.hint
				target.CredentialKind = "api_key"
			} else {
				target.ProviderKeyName = "渠道凭据"
				target.ProviderKeyHint = route.Provider.AuthKind
			}
			targets = append(targets, target)
		}
	}
	sort.SliceStable(targets, func(i, j int) bool {
		if targets[i].Model != targets[j].Model {
			return targets[i].Model < targets[j].Model
		}
		if targets[i].ProviderName != targets[j].ProviderName {
			return targets[i].ProviderName < targets[j].ProviderName
		}
		return targets[i].ProviderKeyID < targets[j].ProviderKeyID
	})
	return targets, nil
}

func (a *App) qualityDetectorTarget(ctx context.Context, id string) (qualityDetectorTarget, error) {
	targets, err := a.qualityDetectorTargets(ctx)
	if err != nil {
		return qualityDetectorTarget{}, err
	}
	for _, target := range targets {
		if target.ID == id {
			return target, nil
		}
	}
	return qualityDetectorTarget{}, errors.New("the selected channel or credential is unavailable for this model")
}

func qualityDetectorRequestIsLoopback(r *http.Request) bool {
	for _, header := range []string{"Forwarded", "X-Forwarded-For", "X-Real-IP"} {
		if strings.TrimSpace(r.Header.Get(header)) != "" {
			return false
		}
	}
	address := strings.TrimSpace(r.RemoteAddr)
	if address == "" {
		return false
	}
	if parsed, err := netip.ParseAddrPort(address); err == nil {
		return parsed.Addr().IsLoopback()
	}
	parsed, err := netip.ParseAddr(address)
	return err == nil && parsed.IsLoopback()
}

func qualityDetectorRouteHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("%x", sum[:])
}

func (a *App) registerQualityDetectorRoute(target qualityDetectorTarget) (string, string) {
	raw := "fg_quality_" + base64.RawURLEncoding.EncodeToString(randomBytes(32))
	hash := qualityDetectorRouteHash(raw)
	a.qualityDetectorMu.Lock()
	a.qualityDetectorRoutes[hash] = &qualityDetectorRouteSession{Target: target, ExpiresAt: time.Now().Add(qualityDetectorRouteTTL), Remaining: qualityDetectorRouteMaxRequests}
	a.qualityDetectorMu.Unlock()
	return raw, hash
}

func (a *App) removeQualityDetectorRoute(hash string) {
	a.qualityDetectorMu.Lock()
	delete(a.qualityDetectorRoutes, hash)
	if a.qualityDetectorActive == hash {
		a.qualityDetectorActive = ""
	}
	a.qualityDetectorMu.Unlock()
}

func (a *App) activateQualityDetectorRoute(hash string, target qualityDetectorTarget) {
	a.qualityDetectorMu.Lock()
	if previous := a.qualityDetectorActive; previous != "" && previous != hash {
		delete(a.qualityDetectorRoutes, previous)
	}
	a.qualityDetectorActive = hash
	a.qualityDetectorLast = target
	a.qualityDetectorMu.Unlock()
}

func (a *App) clearActiveQualityDetectorRoute() {
	a.qualityDetectorMu.Lock()
	if a.qualityDetectorActive != "" {
		delete(a.qualityDetectorRoutes, a.qualityDetectorActive)
		a.qualityDetectorActive = ""
	}
	a.qualityDetectorMu.Unlock()
}

func (a *App) authenticateQualityDetectorRoute(r *http.Request, raw string) (authKey, bool) {
	if !strings.HasPrefix(raw, "fg_quality_") || r.Method != http.MethodPost || r.URL.Path != "/v1/responses" || r.URL.RawQuery != "" || r.URL.ForceQuery || !qualityDetectorRequestIsLoopback(r) {
		return authKey{}, false
	}
	hash := qualityDetectorRouteHash(raw)
	a.qualityDetectorMu.Lock()
	session := a.qualityDetectorRoutes[hash]
	if session == nil || time.Now().After(session.ExpiresAt) || session.Remaining < 1 {
		delete(a.qualityDetectorRoutes, hash)
		a.qualityDetectorMu.Unlock()
		return authKey{}, false
	}
	session.Remaining--
	target := session.Target
	a.qualityDetectorMu.Unlock()
	return authKey{
		Name:         "质量检测 · " + target.ProviderName + " / " + target.ProviderKeyName,
		Prefix:       "fg_quality",
		Hash:         "quality:" + hash,
		AllowModels:  target.Model,
		QualityRoute: session,
	}, true
}

func (a *App) qualityDetectorResponseWithTarget(w http.ResponseWriter, data []byte, status int, clear bool) {
	if clear {
		defer a.clearActiveQualityDetectorRoute()
	}
	var value map[string]any
	if json.Unmarshal(data, &value) != nil {
		writeQualityDetectorResponse(w, data, status)
		return
	}
	a.qualityDetectorMu.Lock()
	target := a.qualityDetectorLast
	a.qualityDetectorMu.Unlock()
	if target.ID != "" {
		value["fusiongate_target"] = target
	}
	writeJSON(w, status, value)
}

func (a *App) qualityDetectorSidecarStatus(r *http.Request) (string, error) {
	data, status, err := a.qualityDetectorClient.request(r, http.MethodGet, "/api/detector/status", nil, "")
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", errors.New(qualityDetectorError(data, status))
	}
	var value struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(data, &value) != nil || strings.TrimSpace(value.Status) == "" {
		return "", errors.New("quality detector returned an invalid status")
	}
	return strings.ToLower(strings.TrimSpace(value.Status)), nil
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
		targets, targetErr := a.qualityDetectorTargets(r.Context())
		if targetErr != nil {
			fail(w, http.StatusInternalServerError, "database_error", targetErr.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"available": true, "version": qualityDetectorVersion, "estimates": estimates, "targets": targets})
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
		clear := false
		if action == "/status" {
			var value struct {
				Status string `json:"status"`
			}
			if json.Unmarshal(data, &value) == nil {
				clear = value.Status == "complete" || value.Status == "stopped" || value.Status == "error"
			}
		}
		a.qualityDetectorResponseWithTarget(w, data, status, clear)
	case "/start":
		if r.Method != http.MethodPost {
			fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
			return
		}
		var input struct {
			Preset   string `json:"preset"`
			TargetID string `json:"target_id"`
		}
		if readJSON(r, &input) != nil || !qualityDetectorPresets[input.Preset] || strings.TrimSpace(input.TargetID) == "" || len(input.TargetID) > 128 {
			fail(w, http.StatusBadRequest, "invalid_request", "choose a supported preset, model, channel, and credential")
			return
		}
		a.qualityDetectorControlMu.Lock()
		defer a.qualityDetectorControlMu.Unlock()
		target, targetErr := a.qualityDetectorTarget(r.Context(), input.TargetID)
		if targetErr != nil {
			fail(w, http.StatusBadRequest, "invalid_quality_detector_target", targetErr.Error())
			return
		}
		detectorStatus, statusErr := a.qualityDetectorSidecarStatus(r)
		if statusErr != nil {
			fail(w, http.StatusBadGateway, "quality_detector_unavailable", statusErr.Error())
			return
		}
		if detectorStatus == "running" || detectorStatus == "stopping" {
			fail(w, http.StatusConflict, "quality_detector_busy", "quality detector is already running or stopping")
			return
		}
		a.clearActiveQualityDetectorRoute()
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
		routeToken, routeHash := a.registerQualityDetectorRoute(target)
		data, status, requestErr := client.request(r, http.MethodPost, "/api/detector/start", map[string]any{
			"base_url": baseURL, "model": target.Model, "api_key": routeToken, "config": config,
			"retention_enabled": false, "retention_directory": nil,
		}, fmt.Sprint(bootstrap["session_token"]))
		if requestErr != nil {
			a.removeQualityDetectorRoute(routeHash)
			fail(w, http.StatusBadGateway, "quality_detector_unavailable", requestErr.Error())
			return
		}
		if status >= 400 {
			a.removeQualityDetectorRoute(routeHash)
			fail(w, status, "quality_detector_failed", qualityDetectorError(data, status))
			return
		}
		a.activateQualityDetectorRoute(routeHash, target)
		a.qualityDetectorResponseWithTarget(w, data, status, false)
	case "/stop":
		if r.Method != http.MethodPost {
			fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
			return
		}
		a.qualityDetectorControlMu.Lock()
		defer a.qualityDetectorControlMu.Unlock()
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
		a.qualityDetectorResponseWithTarget(w, data, status, true)
	default:
		fail(w, http.StatusNotFound, "not_found", "quality detector action not found")
	}
}
