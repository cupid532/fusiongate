package fusiongate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const maxModelDiscoveryBody = 8 << 20

type discoveredModel struct {
	ID                      string   `json:"id"`
	DisplayName             string   `json:"display_name,omitempty"`
	Capabilities            string   `json:"capabilities"`
	SupportedGenerationAPIs []string `json:"-"`
}

type modelDiscoveryResult struct {
	Discovered int               `json:"discovered"`
	Added      int               `json:"added"`
	Existing   int               `json:"existing"`
	Skipped    int               `json:"skipped"`
	Models     []discoveredModel `json:"models,omitempty"`
}

type discoveryProvider struct {
	ID               int64
	Name             string
	Type             string
	BaseURL          string
	Credential       string
	RequestTimeoutMS int
}

type discoveryEnvelope struct {
	Data          json.RawMessage `json:"data"`
	Models        json.RawMessage `json:"models"`
	NextPageToken string          `json:"nextPageToken"`
}

type discoveryModelEntry struct {
	ID                         string   `json:"id"`
	Name                       string   `json:"name"`
	Model                      string   `json:"model"`
	DisplayName                string   `json:"display_name"`
	DisplayNameCamel           string   `json:"displayName"`
	SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
}

func (a *App) loadDiscoveryProvider(ctx context.Context, id int64) (discoveryProvider, error) {
	var p discoveryProvider
	var encrypted []byte
	err := a.db.QueryRowContext(ctx, `SELECT id,name,type,base_url,credential,request_timeout_ms FROM providers WHERE id=?`, id).Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &encrypted, &p.RequestTimeoutMS)
	if err != nil {
		return p, err
	}
	p.Credential, err = a.decrypt(encrypted)
	return p, err
}

func discoveryURLs(p discoveryProvider) ([]string, error) {
	u, err := url.Parse(p.BaseURL)
	if err != nil {
		return nil, err
	}
	basePath := strings.TrimRight(u.Path, "/")
	var paths []string
	switch p.Type {
	case "openai", "openrouter", "openai_compatible", "anthropic":
		if strings.HasSuffix(basePath, "/v1") {
			paths = []string{basePath + "/models"}
		} else {
			paths = []string{basePath + "/v1/models", basePath + "/models"}
		}
	case "gemini":
		if strings.HasSuffix(basePath, "/v1beta") {
			paths = []string{basePath + "/models"}
		} else {
			paths = []string{basePath + "/v1beta/models", basePath + "/models"}
		}
	default:
		return nil, fmt.Errorf("provider type %q does not support model discovery", p.Type)
	}
	out := make([]string, 0, len(paths))
	seen := map[string]bool{}
	for _, modelPath := range paths {
		copyURL := *u
		copyURL.Path = modelPath
		q := copyURL.Query()
		if p.Type == "gemini" {
			q.Set("key", p.Credential)
			q.Set("pageSize", "1000")
		} else if p.Type == "anthropic" {
			q.Set("limit", "1000")
		}
		copyURL.RawQuery = q.Encode()
		value := copyURL.String()
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out, nil
}

func setDiscoveryAuth(req *http.Request, p discoveryProvider) {
	req.Header.Set("Accept", "application/json")
	switch p.Type {
	case "openai", "openrouter", "openai_compatible":
		req.Header.Set("Authorization", "Bearer "+p.Credential)
	case "anthropic":
		req.Header.Set("x-api-key", p.Credential)
		req.Header.Set("anthropic-version", "2023-06-01")
	case "gemini":
		// The Gemini API accepts the key in the query string. Keeping it out of
		// headers also matches the gateway's existing Gemini credential adapter.
	}
}

func safeDiscoveryError(err error, credential string) string {
	message := err.Error()
	for _, secret := range []string{credential, url.QueryEscape(credential)} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	if len(message) > 500 {
		message = message[:500] + "…"
	}
	return message
}

func parseDiscoveryModels(raw []byte, providerType string) ([]discoveredModel, string, error) {
	raw = []byte(strings.TrimSpace(string(raw)))
	var envelope discoveryEnvelope
	entriesRaw := json.RawMessage(raw)
	if len(raw) == 0 {
		return nil, "", errors.New("upstream returned an empty model list")
	}
	if raw[0] == '{' {
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return nil, "", fmt.Errorf("invalid model list JSON: %w", err)
		}
		switch {
		case len(envelope.Data) > 0 && string(envelope.Data) != "null":
			entriesRaw = envelope.Data
		case len(envelope.Models) > 0 && string(envelope.Models) != "null":
			entriesRaw = envelope.Models
		default:
			return nil, envelope.NextPageToken, errors.New("upstream response does not contain data or models")
		}
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(entriesRaw, &entries); err != nil {
		return nil, envelope.NextPageToken, fmt.Errorf("invalid model list entries: %w", err)
	}
	out := make([]discoveredModel, 0, len(entries))
	seen := map[string]bool{}
	for _, rawEntry := range entries {
		var id string
		var displayName string
		var methods []string
		var stringEntry string
		if json.Unmarshal(rawEntry, &stringEntry) == nil {
			id = stringEntry
		} else {
			var entry discoveryModelEntry
			if err := json.Unmarshal(rawEntry, &entry); err != nil {
				continue
			}
			id = entry.ID
			if id == "" {
				id = entry.Name
			}
			if id == "" {
				id = entry.Model
			}
			displayName = entry.DisplayName
			if displayName == "" {
				displayName = entry.DisplayNameCamel
			}
			methods = entry.SupportedGenerationMethods
		}
		id = strings.TrimSpace(strings.TrimPrefix(id, "models/"))
		if id == "" || seen[id] {
			continue
		}
		capabilities, importable := discoveredCapabilities(id, providerType, methods)
		if !importable {
			capabilities = "unsupported"
		}
		seen[id] = true
		out = append(out, discoveredModel{ID: id, DisplayName: displayName, Capabilities: capabilities, SupportedGenerationAPIs: methods})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, envelope.NextPageToken, nil
}

func discoveredCapabilities(id, providerType string, methods []string) (string, bool) {
	if providerType == "gemini" && len(methods) > 0 {
		generates := false
		for _, method := range methods {
			if method == "generateContent" || method == "streamGenerateContent" {
				generates = true
				break
			}
		}
		if !generates {
			return "", false
		}
	}
	lower := strings.ToLower(id)
	for _, marker := range []string{"embedding", "embed-", "moderation", "rerank"} {
		if strings.Contains(lower, marker) {
			return "", false
		}
	}
	if strings.Contains(lower, "dall-e") || strings.Contains(lower, "image") && !strings.Contains(lower, "vision") {
		return "image", true
	}
	return "chat,stream", true
}

func (a *App) fetchDiscoveredModels(ctx context.Context, p discoveryProvider) ([]discoveredModel, error) {
	urls, err := discoveryURLs(p)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for i, endpoint := range urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		setDiscoveryAuth(req, p)
		resp, err := a.client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("model discovery request failed: %s", safeDiscoveryError(err, p.Credential))
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxModelDiscoveryBody+1))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read model list: %w", readErr)
		}
		if len(body) > maxModelDiscoveryBody {
			return nil, errors.New("upstream model list is too large")
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("upstream model endpoint returned HTTP %d", resp.StatusCode)
			if resp.StatusCode == http.StatusNotFound && i+1 < len(urls) {
				continue
			}
			return nil, lastErr
		}
		models, _, err := parseDiscoveryModels(body, p.Type)
		if err != nil {
			lastErr = err
			if i+1 < len(urls) {
				continue
			}
			return nil, err
		}
		if len(models) == 0 {
			return nil, errors.New("upstream returned no models")
		}
		return models, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no model discovery endpoint is available")
	}
	return nil, lastErr
}

func (a *App) discoverAndImportModels(parent context.Context, providerID int64) (modelDiscoveryResult, error) {
	p, err := a.loadDiscoveryProvider(parent, providerID)
	if err != nil {
		return modelDiscoveryResult{}, err
	}
	timeout := 20 * time.Second
	if p.RequestTimeoutMS > 0 && time.Duration(p.RequestTimeoutMS)*time.Millisecond < timeout {
		timeout = time.Duration(p.RequestTimeoutMS) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	models, err := a.fetchDiscoveredModels(ctx, p)
	if err != nil {
		return modelDiscoveryResult{}, err
	}
	result := modelDiscoveryResult{Discovered: len(models), Models: models}
	tx, err := a.db.BeginTx(parent, nil)
	if err != nil {
		return modelDiscoveryResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	stamp := now()
	for _, model := range models {
		if model.Capabilities == "unsupported" {
			result.Skipped++
			continue
		}
		res, err := tx.ExecContext(parent, `INSERT OR IGNORE INTO model_routes(public_name,provider_id,upstream_model,capabilities,enabled,priority,input_price_micros,output_price_micros,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, model.ID, providerID, model.ID, model.Capabilities, 1, 100, 0, 0, stamp, stamp)
		if err != nil {
			return modelDiscoveryResult{}, err
		}
		rows, _ := res.RowsAffected()
		if rows == 1 {
			result.Added++
		} else {
			result.Existing++
		}
	}
	if err := tx.Commit(); err != nil {
		return modelDiscoveryResult{}, err
	}
	return result, nil
}

func discoveryErrorStatus(err error) int {
	if errors.Is(err, sql.ErrNoRows) {
		return http.StatusNotFound
	}
	if strings.Contains(err.Error(), "does not support model discovery") {
		return http.StatusUnprocessableEntity
	}
	return http.StatusBadGateway
}
