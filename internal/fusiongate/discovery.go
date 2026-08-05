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

type providerKeyDiscoveryResult struct {
	KeyID            int64  `json:"key_id"`
	KeyName          string `json:"key_name,omitempty"`
	KeyHint          string `json:"key_hint,omitempty"`
	Discovered       int    `json:"discovered"`
	LatencyMS        int64  `json:"latency_ms"`
	LastDiscoveredAt string `json:"last_discovered_at,omitempty"`
	Error            string `json:"error,omitempty"`
}

type modelDiscoveryResult struct {
	Discovered int                          `json:"discovered"`
	Skipped    int                          `json:"skipped"`
	Models     []discoveredModel            `json:"models"`
	Keys       []providerKeyDiscoveryResult `json:"keys,omitempty"`
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
	IPPoolNodeID     *int64
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
	SupportedReasoningEfforts  []string `json:"supported_reasoning_efforts"`
	SupportedReasoningLevels   []struct {
		Effort string `json:"effort"`
	} `json:"supported_reasoning_levels"`
	DefaultReasoningEffort string   `json:"default_reasoning_effort"`
	DefaultReasoningLevel  string   `json:"default_reasoning_level"`
	InputModalities        []string `json:"input_modalities"`
}

type discoveryCandidate struct {
	provider discoveryProvider
	key      selectedProviderKey
}

// loadDiscoveryCandidates reads and closes every DB row before decrypting or
// making an upstream request. FusionGate intentionally uses one SQLite
// connection, so discovery must never hold a row iterator while doing nested
// work.
func (a *App) loadDiscoveryCandidates(ctx context.Context, id int64) ([]discoveryCandidate, error) {
	var base discoveryProvider
	var encrypted []byte
	var authKind string
	var providerNodeID sql.NullInt64
	var initialized int
	if err := a.db.QueryRowContext(ctx, `SELECT id,name,type,base_url,credential,auth_kind,request_timeout_ms,ip_pool_node_id,multi_key_initialized FROM providers WHERE id=?`, id).Scan(&base.ID, &base.Name, &base.Type, &base.BaseURL, &encrypted, &authKind, &base.RequestTimeoutMS, &providerNodeID, &initialized); err != nil {
		return nil, err
	}
	if providerNodeID.Valid {
		value := providerNodeID.Int64
		base.IPPoolNodeID = &value
	}
	if authKind != "api_key" {
		p, err := a.loadDiscoveryProvider(ctx, id)
		if err != nil {
			return nil, err
		}
		return []discoveryCandidate{{provider: p}}, nil
	}
	if !strBool(initialized) {
		raw, err := a.decrypt(encrypted)
		if err != nil {
			return nil, err
		}
		base.Credential = raw
		return []discoveryCandidate{{provider: base}}, nil
	}

	type storedKey struct {
		selected   selectedProviderKey
		encrypted  []byte
		egressMode string
		node       sql.NullInt64
	}
	rows, err := a.db.QueryContext(ctx, `SELECT id,credential,name,key_hint,model,egress_mode,ip_pool_node_id FROM provider_api_keys WHERE provider_id=? AND enabled=1 ORDER BY sort_order,id`, id)
	if err != nil {
		return nil, err
	}
	stored := make([]storedKey, 0)
	for rows.Next() {
		var item storedKey
		if err := rows.Scan(&item.selected.ID, &item.encrypted, &item.selected.Name, &item.selected.Hint, &item.selected.Model, &item.egressMode, &item.node); err != nil {
			_ = rows.Close()
			return nil, err
		}
		stored = append(stored, item)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(stored) == 0 {
		return nil, errors.New("provider has no enabled API key")
	}
	candidates := make([]discoveryCandidate, 0, len(stored))
	var inheritedNode sql.NullInt64
	if providerNodeID.Valid {
		inheritedNode = providerNodeID
	}
	for _, item := range stored {
		raw, err := a.decrypt(item.encrypted)
		if err != nil {
			return nil, err
		}
		item.selected.Credential = raw
		item.selected.IPPoolNodeID, _ = effectiveProviderKeyNode(item.egressMode, item.node, inheritedNode)
		candidate := base
		candidate.Credential = raw
		candidate.IPPoolNodeID = item.selected.IPPoolNodeID
		candidates = append(candidates, discoveryCandidate{provider: candidate, key: item.selected})
	}
	return candidates, nil
}

func discoveryTimeout(p discoveryProvider) time.Duration {
	timeout := 20 * time.Second
	if p.RequestTimeoutMS > 0 && time.Duration(p.RequestTimeoutMS)*time.Millisecond < timeout {
		timeout = time.Duration(p.RequestTimeoutMS) * time.Millisecond
	}
	return timeout
}

func (a *App) fetchDiscoveryCandidate(parent context.Context, p discoveryProvider) ([]discoveredModel, int64, error) {
	ctx, cancel := context.WithTimeout(parent, discoveryTimeout(p))
	defer cancel()
	started := time.Now()
	models, err := a.fetchDiscoveredModels(ctx, p)
	if err != nil && p.AuthCredential != nil && isDiscoveryAuthenticationError(err) {
		z := resolvedRoute{Provider: Provider{ID: p.ID, Type: p.Type, IPPoolNodeID: p.IPPoolNodeID}, Credential: p.Credential, AuthCredential: p.AuthCredential}
		if refreshErr := a.refreshProviderCredential(ctx, &z, true); refreshErr != nil {
			return nil, time.Since(started).Milliseconds(), refreshErr
		}
		p.Credential, p.AuthCredential = z.Credential, z.AuthCredential
		models, err = a.fetchDiscoveredModels(ctx, p)
	}
	return models, time.Since(started).Milliseconds(), err
}

func providerInventoryModel(model discoveredModel) string {
	value := normalizedModelID(model.UpstreamID)
	if value == "" {
		value = normalizedModelID(model.ID)
	}
	return value
}

func (a *App) persistProviderKeyDiscovery(ctx context.Context, keyID int64, models []discoveredModel, latency int64, discoveryErr error) error {
	if keyID < 1 {
		return nil
	}
	stamp := now()
	if discoveryErr != nil {
		_, err := a.db.ExecContext(ctx, `UPDATE provider_api_keys SET status='failed',last_error=?,last_tested_at=?,last_test_latency_ms=?,updated_at=? WHERE id=?`, sanitizeError(discoveryErr.Error()), stamp, latency, stamp, keyID)
		return err
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	previous := map[string]int{}
	rows, err := tx.QueryContext(ctx, `SELECT model,enabled FROM provider_api_key_models WHERE provider_key_id=?`, keyID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var model string
		var enabled int
		if rows.Scan(&model, &enabled) == nil {
			previous[strings.ToLower(model)] = enabled
		}
	}
	_ = rows.Close()
	if _, err := tx.ExecContext(ctx, `DELETE FROM provider_api_key_models WHERE provider_key_id=?`, keyID); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, model := range models {
		id := providerInventoryModel(model)
		if id == "" || model.Capabilities == "unsupported" || seen[id] {
			continue
		}
		seen[id] = true
		enabled, existed := previous[strings.ToLower(id)]
		if !existed {
			enabled = 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO provider_api_key_models(provider_key_id,model,display_name,capabilities,discovered_at,enabled) VALUES(?,?,?,?,?,?)`, keyID, id, model.DisplayName, model.Capabilities, stamp, enabled); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE provider_api_keys SET status='healthy',last_error='',last_tested_at=?,last_test_latency_ms=?,updated_at=? WHERE id=?`, stamp, latency, stamp, keyID); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *App) loadDiscoveryProvider(ctx context.Context, id int64) (discoveryProvider, error) {
	var p discoveryProvider
	var encrypted []byte
	var authKind string
	var ipPoolNodeID sql.NullInt64
	var multiKeyInitialized int
	err := a.db.QueryRowContext(ctx, `SELECT id,name,type,base_url,credential,auth_kind,request_timeout_ms,ip_pool_node_id,multi_key_initialized FROM providers WHERE id=?`, id).Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &encrypted, &authKind, &p.RequestTimeoutMS, &ipPoolNodeID, &multiKeyInitialized)
	if err != nil {
		return p, err
	}
	if ipPoolNodeID.Valid {
		value := ipPoolNodeID.Int64
		p.IPPoolNodeID = &value
	}
	if authKind == "api_key" {
		selected, err := a.loadDiscoveryProviderKey(ctx, p.ID, p.IPPoolNodeID, encrypted, strBool(multiKeyInitialized))
		if err != nil {
			return p, err
		}
		p.Credential = selected.Credential
		p.IPPoolNodeID = selected.IPPoolNodeID
		return p, nil
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
		var reasoningEfforts []string
		var defaultReasoningEffort string
		var inputModalities []string
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
			reasoningEfforts = append(reasoningEfforts, entry.SupportedReasoningEfforts...)
			for _, level := range entry.SupportedReasoningLevels {
				reasoningEfforts = append(reasoningEfforts, level.Effort)
			}
			defaultReasoningEffort = firstNonEmpty(entry.DefaultReasoningEffort, entry.DefaultReasoningLevel)
			inputModalities = entry.InputModalities
		}
		upstreamID = strings.TrimSpace(strings.TrimPrefix(upstreamID, "models/"))
		publicID := strings.ToLower(upstreamID)
		if publicID == "" || seen[publicID] {
			continue
		}
		capabilities, importable := discoveredCapabilities(upstreamID, providerType, methods)
		if !importable {
			capabilities = "unsupported"
		} else {
			capabilities = discoveredModelCapabilities(capabilities, reasoningEfforts, defaultReasoningEffort, inputModalities)
		}
		seen[publicID] = true
		out = append(out, discoveredModel{ID: publicID, UpstreamID: upstreamID, DisplayName: strings.TrimSpace(displayName), Capabilities: capabilities, SupportedGenerationAPIs: methods})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, envelope.NextPageToken, nil
}

func discoveredModelCapabilities(base string, reasoningEfforts []string, defaultReasoningEffort string, inputModalities []string) string {
	capabilities := strings.Split(base, ",")
	seen := map[string]bool{}
	for _, capability := range capabilities {
		seen[capability] = true
	}
	for _, modality := range inputModalities {
		if strings.EqualFold(strings.TrimSpace(modality), "image") && !seen["image_input"] {
			capabilities = append(capabilities, "image_input")
			seen["image_input"] = true
		}
	}
	for _, effort := range reasoningEfforts {
		effort = strings.ToLower(strings.TrimSpace(effort))
		if effort == "" {
			continue
		}
		capability := "reasoning:" + effort
		if !seen[capability] {
			capabilities = append(capabilities, capability)
			seen[capability] = true
		}
	}
	if effort := strings.ToLower(strings.TrimSpace(defaultReasoningEffort)); effort != "" {
		capabilities = append(capabilities, "reasoning_default:"+effort)
	}
	return strings.Join(capabilities, ",")
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
		resp, err := a.doProviderRequest(req, p.IPPoolNodeID)
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
	candidates, err := a.loadDiscoveryCandidates(parent, providerID)
	if err != nil {
		return modelDiscoveryResult{}, err
	}
	byID := map[string]discoveredModel{}
	keyResults := make([]providerKeyDiscoveryResult, 0, len(candidates))
	successes := 0
	failures := make([]string, 0)
	for _, candidate := range candidates {
		models, latency, fetchErr := a.fetchDiscoveryCandidate(parent, candidate.provider)
		if persistErr := a.persistProviderKeyDiscovery(parent, candidate.key.ID, models, latency, fetchErr); persistErr != nil {
			fetchErr = persistErr
		}
		keyResult := providerKeyDiscoveryResult{KeyID: candidate.key.ID, KeyName: candidate.key.Name, KeyHint: candidate.key.Hint, LatencyMS: latency}
		if fetchErr != nil {
			keyResult.Error = sanitizeError(fetchErr.Error())
			if candidate.key.ID > 0 {
				keyResults = append(keyResults, keyResult)
			}
			failures = append(failures, firstNonEmpty(candidate.key.Name, candidate.key.Hint, "provider")+": "+keyResult.Error)
			continue
		}
		successes++
		keyResult.LastDiscoveredAt = now()
		seenForKey := map[string]bool{}
		for _, model := range models {
			model.ID = normalizedModelID(model.ID)
			if model.ID == "" {
				continue
			}
			if model.Capabilities != "unsupported" && !seenForKey[model.ID] {
				seenForKey[model.ID] = true
				keyResult.Discovered++
			}
			if existing, ok := byID[model.ID]; ok {
				if existing.DisplayName == "" {
					existing.DisplayName = model.DisplayName
				}
				byID[model.ID] = existing
			} else {
				byID[model.ID] = model
			}
		}
		if candidate.key.ID > 0 {
			keyResults = append(keyResults, keyResult)
		}
	}
	if successes == 0 {
		return modelDiscoveryResult{Keys: keyResults}, errors.New(strings.Join(failures, "; "))
	}
	allModels := make([]discoveredModel, 0, len(byID))
	for _, model := range byID {
		allModels = append(allModels, model)
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
	result := modelDiscoveryResult{Models: make([]discoveredModel, 0, len(allModels)+len(routes)), Keys: keyResults}
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
	if result.Added > 0 {
		a.triggerPricingSync()
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
	if result.Added > 0 {
		a.triggerPricingSync()
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
