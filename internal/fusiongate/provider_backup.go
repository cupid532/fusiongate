package fusiongate

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	providerBackupFormat      = "fusiongate-provider-backup"
	providerBackupVersion     = 1
	providerBackupMaxChannels = 200
	providerBackupMaxKeys     = 500
	providerBackupMaxRoutes   = 5000
)

type providerBackupFile struct {
	Format          string                   `json:"format"`
	Version         int                      `json:"version"`
	ExportedAt      string                   `json:"exported_at"`
	ContainsSecrets bool                     `json:"contains_secrets"`
	Providers       []providerBackupProvider `json:"providers"`
}

type providerBackupProvider struct {
	Name               string                `json:"name"`
	Type               string                `json:"type"`
	BaseURL            string                `json:"base_url"`
	Notes              string                `json:"notes,omitempty"`
	Enabled            bool                  `json:"enabled"`
	Priority           int                   `json:"priority"`
	Weight             int                   `json:"weight"`
	PassthroughMode    string                `json:"passthrough_mode"`
	ClientPolicy       string                `json:"client_policy"`
	MaxConcurrency     int                   `json:"max_concurrency"`
	RequestTimeoutMS   int                   `json:"request_timeout_ms"`
	FailureThreshold   int                   `json:"failure_threshold"`
	CooldownSeconds    int                   `json:"cooldown_seconds"`
	HealthCheckEnabled *bool                 `json:"health_check_enabled,omitempty"`
	DefaultModel       string                `json:"default_model,omitempty"`
	GroupName          string                `json:"group_name,omitempty"`
	GroupSortOrder     int                   `json:"group_sort_order,omitempty"`
	IPPoolNodeName     string                `json:"ip_pool_node_name,omitempty"`
	Keys               []providerBackupKey   `json:"keys"`
	Routes             []providerBackupRoute `json:"routes,omitempty"`
}

type providerBackupKey struct {
	Name           string                   `json:"name,omitempty"`
	APIKey         string                   `json:"api_key"`
	Model          string                   `json:"model,omitempty"`
	EgressMode     string                   `json:"egress_mode"`
	IPPoolNodeName string                   `json:"ip_pool_node_name,omitempty"`
	Enabled        bool                     `json:"enabled"`
	SortOrder      int                      `json:"sort_order"`
	Models         []providerBackupKeyModel `json:"models,omitempty"`
}

type providerBackupKeyModel struct {
	Model        string `json:"model"`
	DisplayName  string `json:"display_name,omitempty"`
	Capabilities string `json:"capabilities,omitempty"`
}

type providerBackupRoute struct {
	PublicName            string `json:"public_name"`
	UpstreamModel         string `json:"upstream_model"`
	Capabilities          string `json:"capabilities"`
	Enabled               bool   `json:"enabled"`
	Priority              int    `json:"priority"`
	SortOrder             int    `json:"sort_order"`
	InputPriceMicros      int64  `json:"input_price_micros"`
	CachedPriceMicros     int64  `json:"cached_price_micros"`
	OutputPriceMicros     int64  `json:"output_price_micros"`
	LongContextThreshold  int64  `json:"long_context_threshold"`
	LongInputPriceMicros  int64  `json:"long_input_price_micros"`
	LongCachedPriceMicros int64  `json:"long_cached_price_micros"`
	LongOutputPriceMicros int64  `json:"long_output_price_micros"`
	PricingSource         string `json:"pricing_source,omitempty"`
}

type providerBackupImportResult struct {
	ProvidersCreated int      `json:"providers_created"`
	ProvidersUpdated int      `json:"providers_updated"`
	KeysCreated      int      `json:"keys_created"`
	KeysUpdated      int      `json:"keys_updated"`
	RoutesCreated    int      `json:"routes_created"`
	RoutesUpdated    int      `json:"routes_updated"`
	Warnings         []string `json:"warnings,omitempty"`
}

type providerBackupPending struct {
	ID          int64
	Provider    providerBackupProvider
	Credential  []byte
	Initialized int
}

func (a *App) providerBackupExport(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	rows, err := a.db.Query(`
SELECT p.id,p.name,p.type,p.base_url,p.notes,p.enabled,p.priority,p.weight,p.passthrough_mode,p.client_policy,
       p.max_concurrency,p.request_timeout_ms,p.failure_threshold,p.cooldown_seconds,p.health_check_enabled,p.default_model,p.group_sort_order,
       COALESCE(g.name,''),COALESCE(n.name,''),p.credential,p.multi_key_initialized
FROM providers p
LEFT JOIN provider_groups g ON g.id=p.group_id
LEFT JOIN ip_pool_nodes n ON n.id=p.ip_pool_node_id
WHERE p.auth_kind='api_key'
ORDER BY p.priority DESC,p.id`)
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	pending := make([]providerBackupPending, 0)
	for rows.Next() {
		var item providerBackupPending
		var enabled, healthCheckEnabled int
		if err := rows.Scan(&item.ID, &item.Provider.Name, &item.Provider.Type, &item.Provider.BaseURL, &item.Provider.Notes,
			&enabled, &item.Provider.Priority, &item.Provider.Weight, &item.Provider.PassthroughMode, &item.Provider.ClientPolicy,
			&item.Provider.MaxConcurrency, &item.Provider.RequestTimeoutMS, &item.Provider.FailureThreshold, &item.Provider.CooldownSeconds, &healthCheckEnabled,
			&item.Provider.DefaultModel, &item.Provider.GroupSortOrder, &item.Provider.GroupName, &item.Provider.IPPoolNodeName, &item.Credential, &item.Initialized); err != nil {
			rows.Close()
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		item.Provider.HealthCheckEnabled = boolPtr(strBool(healthCheckEnabled))
		item.Provider.Enabled = strBool(enabled)
		pending = append(pending, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	if err := rows.Close(); err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}

	backup := providerBackupFile{Format: providerBackupFormat, Version: providerBackupVersion, ExportedAt: now(), ContainsSecrets: true, Providers: make([]providerBackupProvider, 0, len(pending))}
	for _, item := range pending {
		provider := item.Provider
		keyRows, err := a.db.Query(`
SELECT k.id,k.credential,k.name,k.model,k.egress_mode,COALESCE(n.name,''),k.enabled,k.sort_order
FROM provider_api_keys k LEFT JOIN ip_pool_nodes n ON n.id=k.ip_pool_node_id
WHERE k.provider_id=? ORDER BY k.sort_order,k.id`, item.ID)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		type pendingKey struct {
			ID         int64
			Credential []byte
			Backup     providerBackupKey
		}
		keys := make([]pendingKey, 0)
		for keyRows.Next() {
			var key pendingKey
			var enabled int
			if err := keyRows.Scan(&key.ID, &key.Credential, &key.Backup.Name, &key.Backup.Model, &key.Backup.EgressMode, &key.Backup.IPPoolNodeName, &enabled, &key.Backup.SortOrder); err != nil {
				keyRows.Close()
				fail(w, http.StatusInternalServerError, "database_error", err.Error())
				return
			}
			key.Backup.Enabled = strBool(enabled)
			keys = append(keys, key)
		}
		if err := keyRows.Err(); err != nil {
			keyRows.Close()
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		if err := keyRows.Close(); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		for _, pendingKey := range keys {
			key := pendingKey.Backup
			key.APIKey, err = a.decrypt(pendingKey.Credential)
			if err != nil {
				fail(w, http.StatusInternalServerError, "credential_error", "could not decrypt a provider API key")
				return
			}
			modelRows, queryErr := a.db.Query(`SELECT model,display_name,capabilities FROM provider_api_key_models WHERE provider_key_id=? ORDER BY model`, pendingKey.ID)
			if queryErr != nil {
				fail(w, http.StatusInternalServerError, "database_error", queryErr.Error())
				return
			}
			for modelRows.Next() {
				var model providerBackupKeyModel
				if err := modelRows.Scan(&model.Model, &model.DisplayName, &model.Capabilities); err != nil {
					modelRows.Close()
					fail(w, http.StatusInternalServerError, "database_error", err.Error())
					return
				}
				key.Models = append(key.Models, model)
			}
			if err := modelRows.Err(); err != nil {
				modelRows.Close()
				fail(w, http.StatusInternalServerError, "database_error", err.Error())
				return
			}
			if err := modelRows.Close(); err != nil {
				fail(w, http.StatusInternalServerError, "database_error", err.Error())
				return
			}
			provider.Keys = append(provider.Keys, key)
		}
		if len(provider.Keys) == 0 {
			if strBool(item.Initialized) {
				continue
			}
			legacy, decryptErr := a.decrypt(item.Credential)
			if decryptErr != nil {
				fail(w, http.StatusInternalServerError, "credential_error", "could not decrypt a provider credential")
				return
			}
			provider.Keys = append(provider.Keys, providerBackupKey{Name: "默认 Key", APIKey: legacy, EgressMode: providerKeyEgressInherit, Enabled: true})
		}

		routeRows, err := a.db.Query(`
SELECT public_name,upstream_model,capabilities,enabled,priority,sort_order,input_price_micros,cached_price_micros,output_price_micros,
       long_context_threshold,long_input_price_micros,long_cached_price_micros,long_output_price_micros,pricing_source
FROM model_routes WHERE provider_id=? ORDER BY sort_order,id`, item.ID)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		for routeRows.Next() {
			var route providerBackupRoute
			var enabled int
			if err := routeRows.Scan(&route.PublicName, &route.UpstreamModel, &route.Capabilities, &enabled, &route.Priority, &route.SortOrder,
				&route.InputPriceMicros, &route.CachedPriceMicros, &route.OutputPriceMicros, &route.LongContextThreshold,
				&route.LongInputPriceMicros, &route.LongCachedPriceMicros, &route.LongOutputPriceMicros, &route.PricingSource); err != nil {
				routeRows.Close()
				fail(w, http.StatusInternalServerError, "database_error", err.Error())
				return
			}
			route.Enabled = strBool(enabled)
			provider.Routes = append(provider.Routes, route)
		}
		if err := routeRows.Err(); err != nil {
			routeRows.Close()
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		if err := routeRows.Close(); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		backup.Providers = append(backup.Providers, provider)
	}

	stamp := time.Now().UTC().Format("20060102-150405")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="fusiongate-providers-`+stamp+`.json"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(backup)
}

func validateProviderBackup(backup *providerBackupFile, cfg Config) error {
	if backup.Format != providerBackupFormat || backup.Version != providerBackupVersion {
		return fmt.Errorf("unsupported backup format or version")
	}
	if len(backup.Providers) == 0 || len(backup.Providers) > providerBackupMaxChannels {
		return fmt.Errorf("backup must contain between 1 and %d providers", providerBackupMaxChannels)
	}
	seenNames := map[string]struct{}{}
	for i := range backup.Providers {
		provider := &backup.Providers[i]
		provider.Name = strings.TrimSpace(provider.Name)
		provider.Type = strings.TrimSpace(provider.Type)
		provider.BaseURL = strings.TrimRight(strings.TrimSpace(provider.BaseURL), "/")
		provider.DefaultModel = normalizeProviderKeyModel(provider.DefaultModel)
		provider.GroupName = strings.TrimSpace(provider.GroupName)
		provider.IPPoolNodeName = strings.TrimSpace(provider.IPPoolNodeName)
		if provider.Name == "" || !validEditableProviderType(provider.Type) || provider.BaseURL == "" {
			return fmt.Errorf("provider %d has an invalid name, type, or base URL", i+1)
		}
		if _, exists := seenNames[provider.Name]; exists {
			return fmt.Errorf("duplicate provider name %q", provider.Name)
		}
		seenNames[provider.Name] = struct{}{}
		if err := validateUpstream(provider.BaseURL, cfg); err != nil {
			return fmt.Errorf("provider %q: %w", provider.Name, err)
		}
		if provider.Weight == 0 {
			provider.Weight = 100
		}
		if provider.PassthroughMode == "" {
			provider.PassthroughMode = "normalized"
		}
		if provider.ClientPolicy == "" {
			provider.ClientPolicy = "any"
		}
		if provider.RequestTimeoutMS == 0 {
			provider.RequestTimeoutMS = 120000
		}
		if provider.FailureThreshold == 0 {
			provider.FailureThreshold = 3
		}
		if provider.CooldownSeconds == 0 {
			provider.CooldownSeconds = 30
		}
		if provider.Priority < 0 || provider.Weight < 1 || provider.MaxConcurrency < 0 || provider.RequestTimeoutMS < 1000 || provider.FailureThreshold < 1 || provider.CooldownSeconds < 1 || !validPassthroughMode(provider.PassthroughMode) || !validClientPolicy(provider.ClientPolicy) {
			return fmt.Errorf("provider %q has invalid scheduling settings", provider.Name)
		}
		if len(provider.Keys) == 0 || len(provider.Keys) > providerBackupMaxKeys {
			return fmt.Errorf("provider %q must contain between 1 and %d keys", provider.Name, providerBackupMaxKeys)
		}
		if len(provider.Routes) > providerBackupMaxRoutes {
			return fmt.Errorf("provider %q contains too many routes", provider.Name)
		}
		seenKeys := map[string]struct{}{}
		for keyIndex := range provider.Keys {
			key := &provider.Keys[keyIndex]
			key.Name = strings.TrimSpace(key.Name)
			key.APIKey = strings.TrimSpace(key.APIKey)
			key.Model = normalizeProviderKeyModel(key.Model)
			key.IPPoolNodeName = strings.TrimSpace(key.IPPoolNodeName)
			if key.APIKey == "" {
				return fmt.Errorf("provider %q contains an empty API key", provider.Name)
			}
			if key.EgressMode == "" {
				key.EgressMode = providerKeyEgressInherit
			}
			if !validProviderKeyEgressMode(key.EgressMode) {
				return fmt.Errorf("provider %q contains an invalid key egress mode", provider.Name)
			}
			if _, exists := seenKeys[key.APIKey]; exists {
				return fmt.Errorf("provider %q contains a duplicate API key", provider.Name)
			}
			seenKeys[key.APIKey] = struct{}{}
			for modelIndex := range key.Models {
				model := &key.Models[modelIndex]
				model.Model = strings.TrimSpace(model.Model)
				model.DisplayName = strings.TrimSpace(model.DisplayName)
				model.Capabilities = strings.TrimSpace(model.Capabilities)
				if model.Model == "" {
					return fmt.Errorf("provider %q contains an empty discovered model", provider.Name)
				}
				if model.Capabilities == "" {
					model.Capabilities = "chat,stream"
				}
			}
		}
		for routeIndex := range provider.Routes {
			route := &provider.Routes[routeIndex]
			route.PublicName = strings.TrimSpace(route.PublicName)
			route.UpstreamModel = strings.TrimSpace(route.UpstreamModel)
			route.Capabilities = strings.TrimSpace(route.Capabilities)
			route.PricingSource = strings.TrimSpace(route.PricingSource)
			if route.PublicName == "" || route.UpstreamModel == "" {
				return fmt.Errorf("provider %q contains an invalid route", provider.Name)
			}
			if route.Capabilities == "" {
				route.Capabilities = "chat,stream"
			}
			if route.Priority < 0 || route.SortOrder < 0 || route.InputPriceMicros < 0 || route.CachedPriceMicros < 0 || route.OutputPriceMicros < 0 || route.LongContextThreshold < 0 || route.LongInputPriceMicros < 0 || route.LongCachedPriceMicros < 0 || route.LongOutputPriceMicros < 0 {
				return fmt.Errorf("provider %q contains invalid route settings", provider.Name)
			}
		}
	}
	return nil
}

func (a *App) providerBackupImport(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	var backup providerBackupFile
	if err := readJSON(r, &backup); err != nil {
		fail(w, http.StatusBadRequest, "invalid_backup", err.Error())
		return
	}
	if err := validateProviderBackup(&backup, a.cfg); err != nil {
		fail(w, http.StatusBadRequest, "invalid_backup", err.Error())
		return
	}

	result := providerBackupImportResult{}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	defer tx.Rollback()
	changedProviderIDs := make([]int64, 0, len(backup.Providers))
	for _, provider := range backup.Providers {
		var groupID any
		if provider.GroupName != "" {
			var id int64
			err := tx.QueryRow(`SELECT id FROM provider_groups WHERE name=?`, provider.GroupName).Scan(&id)
			if errors.Is(err, sql.ErrNoRows) {
				res, insertErr := tx.Exec(`INSERT INTO provider_groups(name,collapsed,sort_order,created_at,updated_at) VALUES(?,0,0,?,?)`, provider.GroupName, now(), now())
				if insertErr != nil {
					fail(w, http.StatusInternalServerError, "database_error", insertErr.Error())
					return
				}
				id, _ = res.LastInsertId()
			} else if err != nil {
				fail(w, http.StatusInternalServerError, "database_error", err.Error())
				return
			}
			groupID = id
		}
		providerNodeID, nodeErr := backupNodeID(tx, provider.IPPoolNodeName)
		if nodeErr != nil {
			fail(w, http.StatusInternalServerError, "database_error", nodeErr.Error())
			return
		}
		if provider.IPPoolNodeName != "" && providerNodeID == nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("渠道 %s 的出口节点 %s 不存在，已改为本机直连", provider.Name, provider.IPPoolNodeName))
		}
		firstEncrypted, encryptErr := a.encrypt(provider.Keys[0].APIKey)
		if encryptErr != nil {
			fail(w, http.StatusInternalServerError, "credential_error", encryptErr.Error())
			return
		}
		var providerID int64
		var authKind string
		findErr := tx.QueryRow(`SELECT id,auth_kind FROM providers WHERE name=?`, provider.Name).Scan(&providerID, &authKind)
		if errors.Is(findErr, sql.ErrNoRows) {
			res, insertErr := tx.Exec(`INSERT INTO providers(name,type,base_url,credential,enabled,priority,sort_order,weight,status,notes,passthrough_mode,client_policy,max_concurrency,request_timeout_ms,failure_threshold,cooldown_seconds,auth_kind,auth_source,auth_status,group_id,group_sort_order,ip_pool_node_id,default_model,multi_key_initialized,created_at,updated_at) VALUES(?,?,?,?,?,?,(SELECT COALESCE(MAX(sort_order),-1)+1 FROM providers),?,'unknown',?,?,?,?,?,?,?,?,?,'ready',?,?,?,?,1,?,?)`, provider.Name, provider.Type, provider.BaseURL, firstEncrypted, boolInt(provider.Enabled), provider.Priority, provider.Weight, provider.Notes, provider.PassthroughMode, provider.ClientPolicy, provider.MaxConcurrency, provider.RequestTimeoutMS, provider.FailureThreshold, provider.CooldownSeconds, "api_key", "manual", groupID, provider.GroupSortOrder, providerNodeID, provider.DefaultModel, now(), now())
			if insertErr != nil {
				fail(w, http.StatusInternalServerError, "database_error", insertErr.Error())
				return
			}
			providerID, _ = res.LastInsertId()
			result.ProvidersCreated++
		} else if findErr != nil {
			fail(w, http.StatusInternalServerError, "database_error", findErr.Error())
			return
		} else {
			if authKind != "api_key" {
				fail(w, http.StatusConflict, "provider_conflict", fmt.Sprintf("provider %q is an OAuth provider and cannot be overwritten", provider.Name))
				return
			}
			if _, updateErr := tx.Exec(`UPDATE providers SET type=?,base_url=?,credential=?,enabled=?,priority=?,weight=?,status='unknown',notes=?,passthrough_mode=?,client_policy=?,max_concurrency=?,request_timeout_ms=?,failure_threshold=?,cooldown_seconds=?,consecutive_failures=0,circuit_open_until=NULL,last_error='',group_id=?,group_sort_order=?,ip_pool_node_id=?,default_model=?,multi_key_initialized=1,updated_at=? WHERE id=?`, provider.Type, provider.BaseURL, firstEncrypted, boolInt(provider.Enabled), provider.Priority, provider.Weight, provider.Notes, provider.PassthroughMode, provider.ClientPolicy, provider.MaxConcurrency, provider.RequestTimeoutMS, provider.FailureThreshold, provider.CooldownSeconds, groupID, provider.GroupSortOrder, providerNodeID, provider.DefaultModel, now(), providerID); updateErr != nil {
				fail(w, http.StatusInternalServerError, "database_error", updateErr.Error())
				return
			}
			result.ProvidersUpdated++
		}
		healthCheckEnabled := true
		if provider.HealthCheckEnabled != nil {
			healthCheckEnabled = *provider.HealthCheckEnabled
		}
		if _, updateErr := tx.Exec(`UPDATE providers SET health_check_enabled=?,updated_at=? WHERE id=?`, boolInt(healthCheckEnabled), now(), providerID); updateErr != nil {
			fail(w, http.StatusInternalServerError, "database_error", updateErr.Error())
			return
		}
		changedProviderIDs = append(changedProviderIDs, providerID)

		for _, key := range provider.Keys {
			keyNodeID, nodeErr := backupNodeID(tx, key.IPPoolNodeName)
			if nodeErr != nil {
				fail(w, http.StatusInternalServerError, "database_error", nodeErr.Error())
				return
			}
			egressMode := key.EgressMode
			if egressMode == providerKeyEgressNode && keyNodeID == nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("渠道 %s 的 Key %s 所用节点 %s 不存在，已改为继承渠道出口", provider.Name, firstNonEmpty(key.Name, providerKeyHint(key.APIKey)), key.IPPoolNodeName))
				egressMode = providerKeyEgressInherit
			}
			encrypted, encryptErr := a.encrypt(key.APIKey)
			if encryptErr != nil {
				fail(w, http.StatusInternalServerError, "credential_error", encryptErr.Error())
				return
			}
			fingerprint := a.providerKeyFingerprint(key.APIKey)
			var keyID int64
			findKeyErr := tx.QueryRow(`SELECT id FROM provider_api_keys WHERE provider_id=? AND fingerprint=?`, providerID, fingerprint).Scan(&keyID)
			if errors.Is(findKeyErr, sql.ErrNoRows) {
				res, insertErr := tx.Exec(`INSERT INTO provider_api_keys(provider_id,credential,fingerprint,key_hint,name,model,egress_mode,ip_pool_node_id,enabled,sort_order,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,'untested',?,?)`, providerID, encrypted, fingerprint, providerKeyHint(key.APIKey), key.Name, key.Model, egressMode, keyNodeID, boolInt(key.Enabled), key.SortOrder, now(), now())
				if insertErr != nil {
					fail(w, http.StatusInternalServerError, "database_error", insertErr.Error())
					return
				}
				keyID, _ = res.LastInsertId()
				result.KeysCreated++
			} else if findKeyErr != nil {
				fail(w, http.StatusInternalServerError, "database_error", findKeyErr.Error())
				return
			} else {
				if _, updateErr := tx.Exec(`UPDATE provider_api_keys SET credential=?,key_hint=?,name=?,model=?,egress_mode=?,ip_pool_node_id=?,enabled=?,sort_order=?,status='untested',last_error='',updated_at=? WHERE id=?`, encrypted, providerKeyHint(key.APIKey), key.Name, key.Model, egressMode, keyNodeID, boolInt(key.Enabled), key.SortOrder, now(), keyID); updateErr != nil {
					fail(w, http.StatusInternalServerError, "database_error", updateErr.Error())
					return
				}
				result.KeysUpdated++
			}
			if key.Models != nil {
				if _, deleteErr := tx.Exec(`DELETE FROM provider_api_key_models WHERE provider_key_id=?`, keyID); deleteErr != nil {
					fail(w, http.StatusInternalServerError, "database_error", deleteErr.Error())
					return
				}
				for _, model := range key.Models {
					if _, insertErr := tx.Exec(`INSERT INTO provider_api_key_models(provider_key_id,model,display_name,capabilities,discovered_at) VALUES(?,?,?,?,?)`, keyID, model.Model, model.DisplayName, model.Capabilities, now()); insertErr != nil {
						fail(w, http.StatusInternalServerError, "database_error", insertErr.Error())
						return
					}
				}
			}
		}

		for _, route := range provider.Routes {
			var routeID int64
			findRouteErr := tx.QueryRow(`SELECT id FROM model_routes WHERE public_name=? AND provider_id=? AND upstream_model=?`, route.PublicName, providerID, route.UpstreamModel).Scan(&routeID)
			if errors.Is(findRouteErr, sql.ErrNoRows) {
				_, insertErr := tx.Exec(`INSERT INTO model_routes(public_name,provider_id,upstream_model,capabilities,enabled,priority,sort_order,input_price_micros,cached_price_micros,output_price_micros,long_context_threshold,long_input_price_micros,long_cached_price_micros,long_output_price_micros,pricing_source,pricing_updated_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, route.PublicName, providerID, route.UpstreamModel, route.Capabilities, boolInt(route.Enabled), route.Priority, route.SortOrder, route.InputPriceMicros, route.CachedPriceMicros, route.OutputPriceMicros, route.LongContextThreshold, route.LongInputPriceMicros, route.LongCachedPriceMicros, route.LongOutputPriceMicros, route.PricingSource, now(), now(), now())
				if insertErr != nil {
					fail(w, http.StatusInternalServerError, "database_error", insertErr.Error())
					return
				}
				result.RoutesCreated++
			} else if findRouteErr != nil {
				fail(w, http.StatusInternalServerError, "database_error", findRouteErr.Error())
				return
			} else {
				if _, updateErr := tx.Exec(`UPDATE model_routes SET capabilities=?,enabled=?,priority=?,sort_order=?,input_price_micros=?,cached_price_micros=?,output_price_micros=?,long_context_threshold=?,long_input_price_micros=?,long_cached_price_micros=?,long_output_price_micros=?,pricing_source=?,pricing_updated_at=?,updated_at=? WHERE id=?`, route.Capabilities, boolInt(route.Enabled), route.Priority, route.SortOrder, route.InputPriceMicros, route.CachedPriceMicros, route.OutputPriceMicros, route.LongContextThreshold, route.LongInputPriceMicros, route.LongCachedPriceMicros, route.LongOutputPriceMicros, route.PricingSource, now(), now(), routeID); updateErr != nil {
					fail(w, http.StatusInternalServerError, "database_error", updateErr.Error())
					return
				}
				result.RoutesUpdated++
			}
		}
	}
	if err := tx.Commit(); err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	a.routeMu.Lock()
	for _, providerID := range changedProviderIDs {
		delete(a.providerStates, providerID)
	}
	a.routeMu.Unlock()
	writeJSON(w, http.StatusOK, result)
}

func backupNodeID(tx *sql.Tx, name string) (any, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil
	}
	var id int64
	err := tx.QueryRow(`SELECT id FROM ip_pool_nodes WHERE name=?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return id, nil
}

func boolPtr(value bool) *bool {
	return &value
}
