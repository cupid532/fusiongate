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

const (
	maxModelDiscoveryBody   = 8 << 20
	maxModelImportSelection = 5000
)

var errSelectedModelsUnavailable = errors.New("one or more selected models are no longer available")

// discoveryHTTPError retains only a safe HTTP status so callers can decide
// whether an OAuth credential refresh is warranted without exposing upstream
// response bodies or credentials.
type discoveryHTTPError struct {
	Status int
}

func (e *discoveryHTTPError) Error() string {
	return fmt.Sprintf("upstream model endpoint returned HTTP %d", e.Status)
}

func isDiscoveryAuthenticationError(err error) bool {
	var httpErr *discoveryHTTPError
	return errors.As(err, &httpErr) && (httpErr.Status == http.StatusUnauthorized || httpErr.Status == http.StatusForbidden)
}

type discoveredModel struct {
	ID                      string   `json:"id"`
	UpstreamID              string   `json:"-"`
	DisplayName             string   `json:"display_name,omitempty"`
	Capabilities            string   `json:"capabilities"`
	PublicNames             []string `json:"public_names,omitempty"`
	Existing                bool     `json:"existing,omitempty"`
	Excluded                bool     `json:"excluded,omitempty"`
	Unavailable             bool     `json:"unavailable,omitempty"`
	SupportedGenerationAPIs []string `json:"-"`
}

type modelDiscoveryResult struct {
	Discovered int               `json:"discovered"`
	Skipped    int               `json:"skipped"`
	Models     []discoveredModel `json:"models"`
}

type modelImportResult struct {
	Selected int `json:"selected"`
	Added    int `json:"added"`
	Existing int `json:"existing"`
	Excluded int `json:"excluded,omitempty"`
	Missing  int `json:"missing"`
}

type modelSelectionResult struct {
	Selected int `json:"selected"`
	Added    int `json:"added"`
	Existing int `json:"existing"`
	Removed  int `json:"removed"`
	Missing  int `json:"missing,omitempty"`
}

type discoveryProvider struct {
	ID               int64
	Name             string
	Type             string
	BaseURL          string
	Credential       string
	AuthCredential   *ProviderCredential
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
	Slug                       string   `json:"slug"`
	DisplayName                string   `json:"display_name"`
	DisplayNameCamel           string   `json:"displayName"`
	SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
}

func (a *App) loadDiscoveryProvider(ctx context.Context, id int64) (discoveryProvider, error) {
	var p discoveryProvider
	var encrypted []byte
	var authKind string
	err := a.db.QueryRowContext(ctx, `SELECT id,name,type,base_url,credential,auth_kind,request_timeout_ms FROM providers WHERE id=?`, id).Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &encrypted, &authKind, &p.RequestTimeoutMS)
	if err != nil {
		return p, err
	}
	plaintext, err := a.decrypt(encrypted)
	if err != nil {
		return p, err
	}
	authCredential, token, err := decodeStoredCredential(authKind, plaintext)
	if err != nil {
		return p, err
	}
	p.Credential = token
	if authKind == "oauth" {
		// Discovery first uses the stored access token. Some OAuth providers
		// issue tokens that remain accepted after locally recorded expiry
		// metadata, while their refresh endpoint may no longer be available.
		// We only refresh after the model endpoint explicitly rejects it.
		p.AuthCredential = &authCredential
	}
	return p, nil
}

func discoveryURLs(p discoveryProvider) ([]string, error) {
	u, err := url.Parse(p.BaseURL)
	if err != nil {
		return nil, err
	}
	basePath := strings.TrimRight(u.Path, "/")
	var paths []string
	switch p.Type {
	case "codex_oauth":
		// The ChatGPT Codex backend does not expose the OpenAI-compatible
		// /v1/models endpoint. Its CLI endpoint requires the client version
		// query parameter and returns the list in a top-level models field.
		paths = []string{basePath + "/models"}
	case "openai", "grok", "openrouter", "openai_compatible", "anthropic":
		paths = compatibleDiscoveryPaths(basePath)
	case "claude_oauth", "grok_oauth":
		if strings.HasSuffix(basePath, "/v1") {
			paths = []string{basePath + "/models"}
		} else {
			paths = []string{basePath + "/v1/models"}
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
		} else if p.Type == "codex_oauth" {
			q.Set("client_version", codexCLIVersion())
		} else if p.Type == "anthropic" || p.Type == "claude_oauth" {
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

func compatibleDiscoveryPaths(basePath string) []string {
	basePath = strings.TrimRight(basePath, "/")
	paths := make([]string, 0, 5)
	add := func(value string) {
		if value == "" {
			value = "/models"
		}
		if !strings.HasPrefix(value, "/") {
			value = "/" + value
		}
		for _, existing := range paths {
			if existing == value {
				return
			}
		}
		paths = append(paths, value)
	}
	if strings.HasSuffix(basePath, "/models") {
		add(basePath)
	} else {
		add(basePath + "/models")
	}
	trimmed := basePath
	for _, version := range []string{"/v1", "/v1beta", "/api/v1"} {
		if strings.HasSuffix(trimmed, version) {
			trimmed = strings.TrimSuffix(trimmed, version)
			break
		}
	}
	add(trimmed + "/models")
	add("/v1/models")
	add("/api/v1/models")
	add("/models")
	return paths
}

func setDiscoveryAuth(req *http.Request, p discoveryProvider) {
	req.Header.Set("Accept", "application/json")
	switch p.Type {
	case "openai", "grok", "openrouter", "openai_compatible":
		req.Header.Set("Authorization", "Bearer "+p.Credential)
	case "codex_oauth":
		req.Header.Set("Authorization", "Bearer "+p.Credential)
		if p.AuthCredential != nil && p.AuthCredential.AccountID != "" {
			req.Header.Set("ChatGPT-Account-ID", p.AuthCredential.AccountID)
		}
		setCodexClientHeaders(req.Header)
	case "grok_oauth":
		req.Header.Set("Authorization", "Bearer "+p.Credential)
		setGrokClientHeaders(req.Header)
	case "anthropic":
		req.Header.Set("x-api-key", p.Credential)
		req.Header.Set("anthropic-version", "2023-06-01")
	case "claude_oauth":
		req.Header.Set("Authorization", "Bearer "+p.Credential)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("anthropic-beta", "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14")
		req.Header.Set("x-app", "cli")
	case "gemini":
		// Gemini accepts the key in the query string.
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
		var upstreamID string
		var displayName string
		var methods []string
		var stringEntry string
		if json.Unmarshal(rawEntry, &stringEntry) == nil {
			upstreamID = stringEntry
		} else {
			var entry discoveryModelEntry
			if err := json.Unmarshal(rawEntry, &entry); err != nil {
				continue
			}
			upstreamID = entry.ID
			if upstreamID == "" {
				upstreamID = entry.Name
			}
			if upstreamID == "" {
				upstreamID = entry.Model
			}
			if upstreamID == "" {
				upstreamID = entry.Slug
			}
			displayName = entry.DisplayName
			if displayName == "" {
				displayName = entry.DisplayNameCamel
			}
			methods = entry.SupportedGenerationMethods
		}
		upstreamID = strings.TrimSpace(strings.TrimPrefix(upstreamID, "models/"))
		publicID := strings.ToLower(upstreamID)
		if publicID == "" || seen[publicID] {
			continue
		}
		capabilities, importable := discoveredCapabilities(upstreamID, providerType, methods)
		if !importable {
			capabilities = "unsupported"
		}
		seen[publicID] = true
		out = append(out, discoveredModel{ID: publicID, UpstreamID: upstreamID, DisplayName: strings.TrimSpace(displayName), Capabilities: capabilities, SupportedGenerationAPIs: methods})
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

func addCodexImageModel(models []discoveredModel) []discoveredModel {
	host := ""
	for _, model := range models {
		if model.ID == "gpt-5.5" && model.Capabilities != "unsupported" {
			host = model.UpstreamID
			break
		}
	}
	if host == "" {
		return models
	}
	existing := make(map[string]bool, len(models))
	for _, model := range models {
		existing[model.ID] = true
	}
	for _, alias := range []struct {
		id, displayName string
	}{
		{"gpt-image-1", "GPT Image 1 (ChatGPT Plus compatibility)"},
		{"gpt-image-2", "GPT Image 2 (ChatGPT Plus)"},
	} {
		if !existing[alias.id] {
			models = append(models, discoveredModel{
				ID: alias.id, UpstreamID: host, DisplayName: alias.displayName, Capabilities: "image",
			})
		}
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models
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
			lastErr = &discoveryHTTPError{Status: resp.StatusCode}
			if p.AuthCredential != nil && isDiscoveryAuthenticationError(lastErr) {
				return nil, lastErr
			}
			if i+1 < len(urls) {
				continue
			}
			break
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
			lastErr = errors.New("upstream returned no models")
			if i+1 < len(urls) {
				continue
			}
			break
		}
		if p.Type == "codex_oauth" {
			models = addCodexImageModel(models)
		}
		return models, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no model discovery endpoint is available")
	}
	return nil, lastErr
}

func normalizedModelID(value string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimPrefix(value, "models/")))
}

type providerModelRoute struct {
	ID            int64
	PublicName    string
	UpstreamModel string
	Capabilities  string
}

func routeDiscoveryModelID(route providerModelRoute, discovered map[string]discoveredModel) string {
	upstream := normalizedModelID(route.UpstreamModel)
	publicName := normalizedModelID(route.PublicName)
	// A synthetic compatibility model can deliberately expose a public ID that
	// differs from the upstream host model. Match that exact pair first so one
	// route does not make both the host and alias appear selected.
	if publicName != upstream {
		if model, ok := discovered[publicName]; ok && normalizedModelID(model.UpstreamID) == upstream {
			return publicName
		}
	}
	if _, ok := discovered[upstream]; ok {
		return upstream
	}
	return upstream
}

func appendUniqueModelName(values []string, value string) []string {
	value = normalizedModelID(value)
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func (a *App) discoverProviderModels(parent context.Context, providerID int64) (modelDiscoveryResult, error) {
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
	allModels, err := a.fetchDiscoveredModels(ctx, p)
	if err != nil && p.AuthCredential != nil && isDiscoveryAuthenticationError(err) {
		// A 401/403 is the only discovery failure that can justify refreshing.
		// Force a single refresh because the provider may reject a token even
		// when its locally recorded expiry has not yet elapsed.
		z := resolvedRoute{Provider: Provider{ID: p.ID, Type: p.Type}, Credential: p.Credential, AuthCredential: p.AuthCredential}
		if refreshErr := a.refreshProviderCredential(ctx, &z, true); refreshErr != nil {
			return modelDiscoveryResult{}, refreshErr
		}
		p.Credential, p.AuthCredential = z.Credential, z.AuthCredential
		allModels, err = a.fetchDiscoveredModels(ctx, p)
	}
	if err != nil {
		return modelDiscoveryResult{}, err
	}

	discovered := make(map[string]discoveredModel, len(allModels))
	for _, model := range allModels {
		model.ID = normalizedModelID(model.ID)
		if model.ID == "" || model.Capabilities == "unsupported" {
			continue
		}
		discovered[model.ID] = model
	}

	routes := make([]providerModelRoute, 0)
	rows, err := a.db.QueryContext(parent, `SELECT id,public_name,upstream_model,capabilities FROM model_routes WHERE provider_id=?`, providerID)
	if err != nil {
		return modelDiscoveryResult{}, err
	}
	for rows.Next() {
		var route providerModelRoute
		if err := rows.Scan(&route.ID, &route.PublicName, &route.UpstreamModel, &route.Capabilities); err != nil {
			_ = rows.Close()
			return modelDiscoveryResult{}, err
		}
		routes = append(routes, route)
	}
	if err := rows.Close(); err != nil {
		return modelDiscoveryResult{}, err
	}

	excluded := map[string]bool{}
	rows, err = a.db.QueryContext(parent, `SELECT public_name,upstream_model FROM model_route_exclusions WHERE provider_id=?`, providerID)
	if err != nil {
		return modelDiscoveryResult{}, err
	}
	for rows.Next() {
		var publicName, upstreamModel string
		if err := rows.Scan(&publicName, &upstreamModel); err != nil {
			_ = rows.Close()
			return modelDiscoveryResult{}, err
		}
		if name := normalizedModelID(upstreamModel); name != "" {
			excluded[name] = true
		}
		if name := normalizedModelID(publicName); name != "" {
			excluded[name] = true
		}
	}
	if err := rows.Close(); err != nil {
		return modelDiscoveryResult{}, err
	}

	matchedRoutes := make(map[int64]bool, len(routes))
	result := modelDiscoveryResult{Models: make([]discoveredModel, 0, len(allModels)+len(routes))}
	for _, model := range allModels {
		if model.Capabilities == "unsupported" {
			result.Skipped++
			continue
		}
		model.ID = normalizedModelID(model.ID)
		if model.ID == "" {
			continue
		}
		upstreamID := normalizedModelID(model.UpstreamID)
		for _, route := range routes {
			if routeDiscoveryModelID(route, discovered) == model.ID {
				matchedRoutes[route.ID] = true
				model.Existing = true
				model.PublicNames = appendUniqueModelName(model.PublicNames, route.PublicName)
			}
		}
		sort.Strings(model.PublicNames)
		model.Excluded = !model.Existing && (excluded[model.ID] || excluded[upstreamID])
		result.Models = append(result.Models, model)
		result.Discovered++
	}

	// Keep configured upstream models visible even when an upstream temporarily
	// omits them. The picker identity remains the real upstream model so changing
	// a public alias never makes the same route look like a new model.
	unavailable := map[string]*discoveredModel{}
	for _, route := range routes {
		if matchedRoutes[route.ID] {
			continue
		}
		id := normalizedModelID(route.UpstreamModel)
		if id == "" {
			id = normalizedModelID(route.PublicName)
		}
		model := unavailable[id]
		if model == nil {
			model = &discoveredModel{ID: id, UpstreamID: route.UpstreamModel, Capabilities: route.Capabilities, Existing: true, Unavailable: true}
			unavailable[id] = model
		}
		model.PublicNames = appendUniqueModelName(model.PublicNames, route.PublicName)
	}
	for _, model := range unavailable {
		sort.Strings(model.PublicNames)
		result.Models = append(result.Models, *model)
	}
	sort.Slice(result.Models, func(i, j int) bool { return result.Models[i].ID < result.Models[j].ID })
	return result, nil
}

func normalizeSelectedModels(selected []string) ([]string, error) {
	if len(selected) == 0 {
		return nil, errors.New("select at least one model")
	}
	if len(selected) > maxModelImportSelection {
		return nil, fmt.Errorf("too many selected models; maximum is %d", maxModelImportSelection)
	}
	out := make([]string, 0, len(selected))
	seen := map[string]bool{}
	for _, value := range selected {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	if len(out) == 0 {
		return nil, errors.New("select at least one model")
	}
	sort.Strings(out)
	return out, nil
}

func (a *App) importSelectedModels(parent context.Context, providerID int64, selected []string) (modelImportResult, error) {
	normalized, err := normalizeSelectedModels(selected)
	if err != nil {
		return modelImportResult{}, err
	}
	discovery, err := a.discoverProviderModels(parent, providerID)
	if err != nil {
		return modelImportResult{}, err
	}
	available := make(map[string]discoveredModel, len(discovery.Models))
	for _, model := range discovery.Models {
		available[model.ID] = model
	}
	missing := make([]string, 0)
	for _, id := range normalized {
		if _, ok := available[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return modelImportResult{Selected: len(normalized), Missing: len(missing)}, fmt.Errorf("%w: %s", errSelectedModelsUnavailable, strings.Join(missing, ", "))
	}

	models := make([]discoveredModel, 0, len(normalized))
	for _, id := range normalized {
		models = append(models, available[id])
	}
	return a.importDiscoveredModels(parent, providerID, models, true)
}

func normalizeModelSelection(selected []string) ([]string, error) {
	if len(selected) > maxModelImportSelection {
		return nil, fmt.Errorf("too many selected models; maximum is %d", maxModelImportSelection)
	}
	out := make([]string, 0, len(selected))
	seen := map[string]bool{}
	for _, value := range selected {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out, nil
}

// applySelectedModels treats selected as the provider's complete desired model
// set. One transaction restores newly checked models and excludes/removes models
// that were unchecked, so the picker behaves like a simple on/off control.
func (a *App) applySelectedModels(parent context.Context, providerID int64, selected []string) (modelSelectionResult, error) {
	normalized, err := normalizeModelSelection(selected)
	if err != nil {
		return modelSelectionResult{}, err
	}
	discovery, err := a.discoverProviderModels(parent, providerID)
	if err != nil {
		return modelSelectionResult{}, err
	}
	available := make(map[string]discoveredModel, len(discovery.Models))
	for _, model := range discovery.Models {
		available[model.ID] = model
	}
	missing := make([]string, 0)
	for _, id := range normalized {
		if _, ok := available[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return modelSelectionResult{Selected: len(normalized), Missing: len(missing)}, fmt.Errorf("%w: %s", errSelectedModelsUnavailable, strings.Join(missing, ", "))
	}

	selectedSet := make(map[string]bool, len(normalized))
	for _, id := range normalized {
		selectedSet[id] = true
	}
	result := modelSelectionResult{Selected: len(normalized)}
	tx, err := a.db.BeginTx(parent, nil)
	if err != nil {
		return modelSelectionResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	current := make([]providerModelRoute, 0)
	rows, err := tx.QueryContext(parent, `SELECT id,public_name,upstream_model,capabilities FROM model_routes WHERE provider_id=?`, providerID)
	if err != nil {
		return modelSelectionResult{}, err
	}
	for rows.Next() {
		var route providerModelRoute
		if err := rows.Scan(&route.ID, &route.PublicName, &route.UpstreamModel, &route.Capabilities); err != nil {
			_ = rows.Close()
			return modelSelectionResult{}, err
		}
		current = append(current, route)
	}
	if err := rows.Close(); err != nil {
		return modelSelectionResult{}, err
	}

	stamp := now()
	currentModels := map[string]bool{}
	removedModels := map[string]bool{}
	oldPublicNames := map[string]bool{}
	for _, route := range current {
		modelID := routeDiscoveryModelID(route, available)
		if selectedSet[modelID] {
			currentModels[modelID] = true
			continue
		}
		upstream := normalizedModelID(route.UpstreamModel)
		if upstream == "" {
			upstream = modelID
		}
		if _, err := tx.ExecContext(parent, `INSERT OR REPLACE INTO model_route_exclusions(provider_id,public_name,upstream_model,created_at) VALUES(?,?,?,?)`, providerID, upstream, upstream, stamp); err != nil {
			return modelSelectionResult{}, err
		}
		if _, err := tx.ExecContext(parent, `DELETE FROM model_routes WHERE id=?`, route.ID); err != nil {
			return modelSelectionResult{}, err
		}
		removedModels[modelID] = true
		oldPublicNames[normalizedModelID(route.PublicName)] = true
	}
	for publicName := range oldPublicNames {
		if _, err := tx.ExecContext(parent, `DELETE FROM route_policies WHERE LOWER(public_name)=? AND NOT EXISTS(SELECT 1 FROM model_routes WHERE LOWER(public_name)=?)`, publicName, publicName); err != nil {
			return modelSelectionResult{}, err
		}
	}

	for _, id := range normalized {
		model := available[id]
		upstream := normalizedModelID(model.UpstreamID)
		if upstream == "" {
			upstream = id
		}
		if _, err := tx.ExecContext(parent, `DELETE FROM model_route_exclusions WHERE provider_id=? AND (LOWER(public_name) IN (?,?) OR LOWER(upstream_model) IN (?,?))`, providerID, id, upstream, id, upstream); err != nil {
			return modelSelectionResult{}, err
		}
		if currentModels[id] {
			result.Existing++
			continue
		}
		res, err := tx.ExecContext(parent, `INSERT INTO model_routes(public_name,provider_id,upstream_model,capabilities,enabled,priority,sort_order,input_price_micros,output_price_micros,created_at,updated_at)
SELECT ?,?,?,?,?,?,(SELECT COALESCE(MAX(sort_order),-1)+1 FROM model_routes WHERE public_name=?),?,?,?,?`, id, providerID, upstream, model.Capabilities, 1, 0, id, 0, 0, stamp, stamp)
		if err != nil {
			return modelSelectionResult{}, err
		}
		added, _ := res.RowsAffected()
		result.Added += int(added)
		if _, err := tx.ExecContext(parent, `INSERT INTO route_policies(public_name,strategy,updated_at) VALUES(?,?,?) ON CONFLICT(public_name) DO NOTHING`, id, StrategyPriorityFailover, stamp); err != nil {
			return modelSelectionResult{}, err
		}
	}
	result.Removed = len(removedModels)
	if err := tx.Commit(); err != nil {
		return modelSelectionResult{}, err
	}
	return result, nil
}

// discoverAndImportAllModels performs one upstream discovery request and imports
// every supported model returned by it. Keeping discovery and insertion separate
// avoids querying OAuth providers twice during automatic initialization.
func (a *App) discoverAndImportAllModels(parent context.Context, providerID int64) (modelDiscoveryResult, modelImportResult, error) {
	discovery, err := a.discoverProviderModels(parent, providerID)
	if err != nil {
		return modelDiscoveryResult{}, modelImportResult{}, err
	}
	result, err := a.importDiscoveredModels(parent, providerID, discovery.Models, false)
	return discovery, result, err
}

func (a *App) importDiscoveredModels(parent context.Context, providerID int64, models []discoveredModel, restoreExcluded bool) (modelImportResult, error) {
	if len(models) == 0 {
		return modelImportResult{}, errors.New("upstream returned no supported models")
	}
	if len(models) > maxModelImportSelection {
		return modelImportResult{}, fmt.Errorf("too many discovered models; maximum is %d", maxModelImportSelection)
	}
	result := modelImportResult{Selected: len(models)}
	tx, err := a.db.BeginTx(parent, nil)
	if err != nil {
		return modelImportResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	stamp := now()
	excluded := map[string]bool{}
	rows, err := tx.QueryContext(parent, `SELECT public_name,upstream_model FROM model_route_exclusions WHERE provider_id=?`, providerID)
	if err != nil {
		return modelImportResult{}, err
	}
	for rows.Next() {
		var publicName, upstreamModel string
		if err := rows.Scan(&publicName, &upstreamModel); err != nil {
			_ = rows.Close()
			return modelImportResult{}, err
		}
		excluded[normalizedModelID(publicName)] = true
		excluded[normalizedModelID(upstreamModel)] = true
	}
	if err := rows.Close(); err != nil {
		return modelImportResult{}, err
	}
	for _, model := range models {
		model.ID = normalizedModelID(model.ID)
		model.UpstreamID = normalizedModelID(model.UpstreamID)
		if model.ID == "" || model.UpstreamID == "" {
			continue
		}
		if excluded[model.ID] || excluded[model.UpstreamID] {
			if !restoreExcluded {
				result.Excluded++
				continue
			}
			if _, err := tx.ExecContext(parent, `DELETE FROM model_route_exclusions WHERE provider_id=? AND (LOWER(public_name) IN (?,?) OR LOWER(upstream_model) IN (?,?))`, providerID, model.ID, model.UpstreamID, model.ID, model.UpstreamID); err != nil {
				return modelImportResult{}, err
			}
		}
		var res sql.Result
		if model.ID == model.UpstreamID {
			res, err = tx.ExecContext(parent, `INSERT INTO model_routes(public_name,provider_id,upstream_model,capabilities,enabled,priority,sort_order,input_price_micros,output_price_micros,created_at,updated_at)
SELECT ?,?,?,?,?,?,(SELECT COALESCE(MAX(sort_order),-1)+1 FROM model_routes WHERE public_name=?),?,?,?,?
WHERE NOT EXISTS (SELECT 1 FROM model_routes WHERE provider_id=? AND LOWER(upstream_model)=?)`, model.ID, providerID, model.UpstreamID, model.Capabilities, 1, 0, model.ID, 0, 0, stamp, stamp, providerID, model.UpstreamID)
		} else {
			// Synthetic compatibility aliases (for example image aliases hosted by
			// another upstream model) must remain independently selectable.
			res, err = tx.ExecContext(parent, `INSERT INTO model_routes(public_name,provider_id,upstream_model,capabilities,enabled,priority,sort_order,input_price_micros,output_price_micros,created_at,updated_at)
SELECT ?,?,?,?,?,?,(SELECT COALESCE(MAX(sort_order),-1)+1 FROM model_routes WHERE public_name=?),?,?,?,?
WHERE NOT EXISTS (SELECT 1 FROM model_routes WHERE provider_id=? AND LOWER(public_name)=? AND LOWER(upstream_model)=?)`, model.ID, providerID, model.UpstreamID, model.Capabilities, 1, 0, model.ID, 0, 0, stamp, stamp, providerID, model.ID, model.UpstreamID)
		}
		if err != nil {
			return modelImportResult{}, err
		}
		rows, _ := res.RowsAffected()
		policyNames := append([]string(nil), model.PublicNames...)
		if rows == 1 {
			result.Added++
			policyNames = appendUniqueModelName(policyNames, model.ID)
		} else {
			result.Existing++
		}
		for _, publicName := range policyNames {
			if _, err := tx.ExecContext(parent, `INSERT INTO route_policies(public_name,strategy,updated_at) VALUES(?,?,?) ON CONFLICT(public_name) DO NOTHING`, publicName, StrategyPriorityFailover, stamp); err != nil {
				return modelImportResult{}, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return modelImportResult{}, err
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

func modelImportErrorStatus(err error) int {
	if errors.Is(err, sql.ErrNoRows) {
		return http.StatusNotFound
	}
	if errors.Is(err, errSelectedModelsUnavailable) {
		return http.StatusConflict
	}
	if strings.Contains(err.Error(), "select at least") || strings.Contains(err.Error(), "too many selected") {
		return http.StatusBadRequest
	}
	return discoveryErrorStatus(err)
}
