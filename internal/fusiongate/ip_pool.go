package fusiongate

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	xproxy "golang.org/x/net/proxy"
)

const (
	ipPoolPortStart = 22000
	ipPoolPortEnd   = 23999
	maxNodeLinkSize = 64 << 10
)

type IPPoolNode struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	Protocol       string `json:"protocol"`
	Server         string `json:"server"`
	Enabled        bool   `json:"enabled"`
	LocalPort      int    `json:"-"`
	Status         string `json:"status"`
	LastError      string `json:"last_error,omitempty"`
	LastCheckedAt  string `json:"last_checked_at,omitempty"`
	LastLatencyMS  int64  `json:"last_latency_ms"`
	ExitIP         string `json:"exit_ip,omitempty"`
	ProviderCount  int    `json:"provider_count"`
	LinkConfigured bool   `json:"link_configured"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
}

type storedIPPoolNode struct {
	IPPoolNode
	ShareLink string
}

type ipPoolManager struct {
	app     *App
	mu      sync.Mutex
	cmd     *exec.Cmd
	done    chan struct{}
	clients map[int64]*http.Client
	ports   map[int64]int
}

func newIPPoolManager(app *App) *ipPoolManager {
	return &ipPoolManager{app: app, clients: map[int64]*http.Client{}, ports: map[int64]int{}}
}

func (m *ipPoolManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked()
	for _, client := range m.clients {
		if transport, ok := client.Transport.(*http.Transport); ok {
			transport.CloseIdleConnections()
		}
	}
	m.clients = map[int64]*http.Client{}
	m.ports = map[int64]int{}
}

func (m *ipPoolManager) Client(nodeID int64) (*http.Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	client := m.clients[nodeID]
	if client == nil {
		return nil, fmt.Errorf("IP pool node %d is disabled or unavailable", nodeID)
	}
	return client, nil
}

func (m *ipPoolManager) Reconcile(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reconcileLocked(ctx)
}

func (m *ipPoolManager) reconcileLocked(ctx context.Context) error {
	nodes, err := m.app.loadEnabledIPPoolNodes(ctx)
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		m.stopLocked()
		for _, client := range m.clients {
			if transport, ok := client.Transport.(*http.Transport); ok {
				transport.CloseIdleConnections()
			}
		}
		m.clients = map[int64]*http.Client{}
		m.ports = map[int64]int{}
		return nil
	}
	binary := strings.TrimSpace(os.Getenv("FUSIONGATE_SING_BOX_PATH"))
	if binary == "" {
		binary = "sing-box"
	}
	if _, err := exec.LookPath(binary); err != nil {
		return fmt.Errorf("sing-box executable is required for IP pool nodes: %w", err)
	}
	config, validNodes, parseFailures, err := buildSingBoxConfig(nodes)
	if err != nil {
		return err
	}
	for id, parseErr := range parseFailures {
		m.app.setIPPoolRuntimeError(id, parseErr)
	}
	if len(validNodes) == 0 {
		return errors.New("no valid enabled IP pool nodes")
	}
	configDir, err := os.MkdirTemp("", "fusiongate-ip-pool-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(configDir)
	if err := os.Chmod(configDir, 0700); err != nil {
		return err
	}
	configPath := filepath.Join(configDir, "sing-box.json")
	if err := os.WriteFile(configPath, config, 0600); err != nil {
		return err
	}
	check := exec.CommandContext(ctx, binary, "check", "-c", configPath)
	if output, err := check.CombinedOutput(); err != nil {
		return fmt.Errorf("invalid generated sing-box configuration: %s", safeProcessError(output, err))
	}
	m.stopLocked()
	for _, client := range m.clients {
		if transport, ok := client.Transport.(*http.Transport); ok {
			transport.CloseIdleConnections()
		}
	}
	m.clients = map[int64]*http.Client{}
	m.ports = map[int64]int{}
	cmd := exec.Command(binary, "run", "-c", configPath)
	cmd.Stdout = &ipPoolLogWriter{app: m.app, level: "info"}
	cmd.Stderr = &ipPoolLogWriter{app: m.app, level: "error"}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start sing-box: %w", err)
	}
	m.cmd = cmd
	done := make(chan struct{})
	m.done = done
	go func(running *exec.Cmd, stopped chan struct{}) {
		err := running.Wait()
		close(stopped)
		if err != nil {
			m.app.log.Warn("IP pool sing-box process exited", "error", err)
		}
	}(cmd, done)
	if err := waitForIPPoolPorts(ctx, validNodes, done); err != nil {
		m.stopLocked()
		return err
	}
	for _, node := range validNodes {
		m.clients[node.ID] = newSOCKSHTTPClient(m.app.cfg, node.LocalPort)
		m.ports[node.ID] = node.LocalPort
		_, _ = m.app.db.ExecContext(ctx, `UPDATE ip_pool_nodes SET status='ready',last_error='',updated_at=? WHERE id=?`, now(), node.ID)
	}
	return nil
}

func waitForIPPoolPorts(ctx context.Context, nodes []storedIPPoolNode, processDone <-chan struct{}) error {
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		allReady := true
		for _, node := range nodes {
			conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(node.LocalPort)), 100*time.Millisecond)
			if err != nil {
				allReady = false
				break
			}
			_ = conn.Close()
		}
		if allReady {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-processDone:
			return errors.New("sing-box exited before IP pool listeners became ready")
		case <-deadline.C:
			return errors.New("sing-box did not open IP pool listeners within 3 seconds")
		case <-ticker.C:
		}
	}
}

func safeProcessError(output []byte, err error) string {
	message := strings.TrimSpace(string(output))
	if message == "" {
		message = err.Error()
	}
	if len(message) > 800 {
		message = message[:800] + "…"
	}
	return message
}

func (m *ipPoolManager) stopLocked() {
	if m.cmd == nil || m.cmd.Process == nil {
		m.cmd = nil
		m.done = nil
		return
	}
	process := m.cmd.Process
	done := m.done
	_ = process.Signal(syscall.SIGTERM)
	if done != nil {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = process.Kill()
			<-done
		}
	} else {
		_ = process.Kill()
	}
	m.cmd = nil
	m.done = nil
}

type ipPoolLogWriter struct {
	app   *App
	level string
}

func (w *ipPoolLogWriter) Write(p []byte) (int, error) {
	message := strings.TrimSpace(string(p))
	if message != "" {
		if w.level == "error" {
			w.app.log.Warn("sing-box", "message", message)
		} else {
			w.app.log.Debug("sing-box", "message", message)
		}
	}
	return len(p), nil
}

func newSOCKSHTTPClient(cfg Config, port int) *http.Client {
	transport := newUpstreamHTTPTransport(cfg)
	transport.Proxy = nil
	baseDialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	socksDialer, err := xproxy.SOCKS5("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), nil, baseDialer)
	if err != nil {
		return &http.Client{Transport: roundTripError{err: fmt.Errorf("create local IP pool dialer: %w", err)}}
	}
	contextDialer, ok := socksDialer.(xproxy.ContextDialer)
	if !ok {
		return &http.Client{Transport: roundTripError{err: errors.New("local IP pool dialer does not support context cancellation")}}
	}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, targetPort, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if cfg.AllowPrivateUpstreams {
			return contextDialer.DialContext(ctx, network, address)
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, ip := range ips {
			addr := ip.Unmap()
			if !addr.IsValid() || isPrivate(addr) {
				lastErr = fmt.Errorf("resolved upstream address %s is blocked by SSRF protection", ip.String())
				continue
			}
			conn, dialErr := contextDialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), targetPort))
			if dialErr == nil {
				return conn, nil
			}
			lastErr = dialErr
		}
		if lastErr == nil {
			lastErr = errors.New("upstream hostname resolved to no usable addresses")
		}
		return nil, lastErr
	}
	return &http.Client{Transport: transport, CheckRedirect: upstreamRedirectPolicy(cfg)}
}

type roundTripError struct{ err error }

func (r roundTripError) RoundTrip(*http.Request) (*http.Response, error) { return nil, r.err }

func (a *App) clientForNode(nodeID *int64) (*http.Client, error) {
	if nodeID == nil {
		return a.client, nil
	}
	if a.ipPool == nil {
		return nil, errors.New("IP pool is not initialized")
	}
	return a.ipPool.Client(*nodeID)
}

func (a *App) doProviderRequest(req *http.Request, nodeID *int64) (*http.Response, error) {
	client, err := a.clientForNode(nodeID)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

func (a *App) loadEnabledIPPoolNodes(ctx context.Context) ([]storedIPPoolNode, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT id,name,protocol,share_link,enabled,local_port,status,last_error,COALESCE(last_checked_at,''),last_latency_ms,exit_ip,created_at,updated_at FROM ip_pool_nodes WHERE enabled=1 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storedIPPoolNode
	for rows.Next() {
		var node storedIPPoolNode
		var encrypted []byte
		var enabled int
		if err := rows.Scan(&node.ID, &node.Name, &node.Protocol, &encrypted, &enabled, &node.LocalPort, &node.Status, &node.LastError, &node.LastCheckedAt, &node.LastLatencyMS, &node.ExitIP, &node.CreatedAt, &node.UpdatedAt); err != nil {
			return nil, err
		}
		plaintext, err := a.decrypt(encrypted)
		if err != nil {
			a.setIPPoolRuntimeError(node.ID, errors.New("cannot decrypt node link"))
			continue
		}
		node.ShareLink = plaintext
		node.Enabled = strBool(enabled)
		out = append(out, node)
	}
	return out, rows.Err()
}

func (a *App) setIPPoolRuntimeError(id int64, err error) {
	message := sanitizeError(err.Error())
	_, _ = a.db.Exec(`UPDATE ip_pool_nodes SET status='config_error',last_error=?,updated_at=? WHERE id=?`, message, now(), id)
}

func (a *App) validateIPPoolNode(id int64) error {
	var enabled int
	if err := a.db.QueryRow(`SELECT enabled FROM ip_pool_nodes WHERE id=?`, id).Scan(&enabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("selected IP pool node does not exist")
		}
		return err
	}
	if !strBool(enabled) {
		return errors.New("selected IP pool node is disabled")
	}
	return nil
}

func (a *App) reconcileIPPool(ctx context.Context) error {
	if a.ipPool == nil {
		return nil
	}
	if err := a.ipPool.Reconcile(ctx); err != nil {
		a.log.Warn("could not reconcile IP pool", "error", err)
		return err
	}
	return nil
}

func (a *App) ipPoolNodes(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	switch r.Method {
	case http.MethodGet:
		rows, err := a.db.Query(`SELECT n.id,n.name,n.protocol,n.server,n.enabled,n.status,n.last_error,COALESCE(n.last_checked_at,''),n.last_latency_ms,n.exit_ip,((SELECT COUNT(*) FROM providers p WHERE p.ip_pool_node_id=n.id)+(SELECT COUNT(*) FROM provider_api_keys k WHERE k.ip_pool_node_id=n.id)),n.created_at,n.updated_at FROM ip_pool_nodes n ORDER BY n.id`)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		defer rows.Close()
		out := []IPPoolNode{}
		for rows.Next() {
			var node IPPoolNode
			var enabled int
			if err := rows.Scan(&node.ID, &node.Name, &node.Protocol, &node.Server, &enabled, &node.Status, &node.LastError, &node.LastCheckedAt, &node.LastLatencyMS, &node.ExitIP, &node.ProviderCount, &node.CreatedAt, &node.UpdatedAt); err != nil {
				fail(w, http.StatusInternalServerError, "database_error", err.Error())
				return
			}
			node.Enabled = strBool(enabled)
			node.LinkConfigured = true
			out = append(out, node)
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var in struct {
			Name      string `json:"name"`
			ShareLink string `json:"share_link"`
			Enabled   *bool  `json:"enabled"`
		}
		if err := readJSON(r, &in); err != nil {
			fail(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		name := strings.TrimSpace(in.Name)
		link := strings.TrimSpace(in.ShareLink)
		if name == "" || link == "" || len(link) > maxNodeLinkSize {
			fail(w, http.StatusBadRequest, "invalid_request", "node name and a valid share link are required")
			return
		}
		outbound, protocol, server, err := parseProxyShareLink(link, "validate")
		if err != nil || outbound == nil {
			fail(w, http.StatusBadRequest, "unsupported_proxy_link", err.Error())
			return
		}
		encrypted, err := a.encrypt(link)
		if err != nil {
			fail(w, http.StatusInternalServerError, "credential_error", err.Error())
			return
		}
		enabled := true
		if in.Enabled != nil {
			enabled = *in.Enabled
		}
		tx, err := a.db.BeginTx(r.Context(), nil)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		defer tx.Rollback()
		port, err := allocateIPPoolPort(r.Context(), tx)
		if err != nil {
			fail(w, http.StatusConflict, "ip_pool_full", err.Error())
			return
		}
		res, err := tx.ExecContext(r.Context(), `INSERT INTO ip_pool_nodes(name,protocol,server,share_link,enabled,local_port,status,created_at,updated_at) VALUES(?,?,?,?,?,?, 'pending',?,?)`, name, protocol, server, encrypted, boolInt(enabled), port, now(), now())
		if err != nil {
			fail(w, http.StatusConflict, "node_conflict", err.Error())
			return
		}
		if err := tx.Commit(); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		id, _ := res.LastInsertId()
		reconcileErr := a.reconcileIPPool(r.Context())
		response := map[string]any{"id": id, "name": name, "protocol": protocol, "server": server}
		if reconcileErr != nil {
			response["runtime_warning"] = reconcileErr.Error()
		}
		writeJSON(w, http.StatusCreated, response)
	default:
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET or POST required")
	}
}

func allocateIPPoolPort(ctx context.Context, tx *sql.Tx) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT local_port FROM ip_pool_nodes`)
	if err != nil {
		return 0, err
	}
	used := map[int]bool{}
	for rows.Next() {
		var port int
		if rows.Scan(&port) == nil {
			used[port] = true
		}
	}
	_ = rows.Close()
	for port := ipPoolPortStart; port <= ipPoolPortEnd; port++ {
		if !used[port] {
			return port, nil
		}
	}
	return 0, errors.New("IP pool supports at most 2000 nodes")
}

func (a *App) ipPoolNodeByID(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	suffix := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/admin/ip-pool/"), "/")
	parts := strings.Split(suffix, "/")
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id < 1 {
		fail(w, http.StatusBadRequest, "invalid_id", "invalid node id")
		return
	}
	if len(parts) == 2 && parts[1] == "test" {
		a.testIPPoolNode(w, r, id)
		return
	}
	if len(parts) != 1 {
		fail(w, http.StatusNotFound, "not_found", "unknown IP pool action")
		return
	}
	switch r.Method {
	case http.MethodPatch:
		var in struct {
			Name      *string `json:"name"`
			ShareLink *string `json:"share_link"`
			Enabled   *bool   `json:"enabled"`
		}
		if err := readJSON(r, &in); err != nil {
			fail(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		var nameArg, protocolArg, serverArg any
		var encryptedArg any
		if in.Name != nil {
			name := strings.TrimSpace(*in.Name)
			if name == "" {
				fail(w, http.StatusBadRequest, "invalid_request", "node name cannot be empty")
				return
			}
			nameArg = name
		}
		if in.ShareLink != nil && strings.TrimSpace(*in.ShareLink) != "" {
			link := strings.TrimSpace(*in.ShareLink)
			if len(link) > maxNodeLinkSize {
				fail(w, http.StatusBadRequest, "invalid_request", "share link is too large")
				return
			}
			_, protocol, server, parseErr := parseProxyShareLink(link, "validate")
			if parseErr != nil {
				fail(w, http.StatusBadRequest, "unsupported_proxy_link", parseErr.Error())
				return
			}
			encrypted, encryptErr := a.encrypt(link)
			if encryptErr != nil {
				fail(w, http.StatusInternalServerError, "credential_error", encryptErr.Error())
				return
			}
			encryptedArg, protocolArg, serverArg = encrypted, protocol, server
		}
		res, err := a.db.Exec(`UPDATE ip_pool_nodes SET name=COALESCE(?,name),share_link=COALESCE(?,share_link),protocol=COALESCE(?,protocol),server=COALESCE(?,server),enabled=COALESCE(?,enabled),status='pending',last_error='',updated_at=? WHERE id=?`, nameArg, encryptedArg, protocolArg, serverArg, maybeBool(in.Enabled), now(), id)
		if err != nil {
			fail(w, http.StatusConflict, "node_conflict", err.Error())
			return
		}
		if affected, _ := res.RowsAffected(); affected == 0 {
			fail(w, http.StatusNotFound, "not_found", "IP pool node not found")
			return
		}
		reconcileErr := a.reconcileIPPool(r.Context())
		response := map[string]any{"id": id, "updated": true}
		if reconcileErr != nil {
			response["runtime_warning"] = reconcileErr.Error()
		}
		writeJSON(w, http.StatusOK, response)
	case http.MethodDelete:
		var providerCount int
		if err := a.db.QueryRow(`SELECT (SELECT COUNT(*) FROM providers WHERE ip_pool_node_id=?)+(SELECT COUNT(*) FROM provider_api_keys WHERE ip_pool_node_id=?)`, id, id).Scan(&providerCount); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		if providerCount > 0 {
			fail(w, http.StatusConflict, "node_in_use", fmt.Sprintf("node is still assigned to %d provider or API key configuration(s); change their network exit before deleting it", providerCount))
			return
		}
		res, err := a.db.Exec(`DELETE FROM ip_pool_nodes WHERE id=?`, id)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		if affected, _ := res.RowsAffected(); affected == 0 {
			fail(w, http.StatusNotFound, "not_found", "IP pool node not found")
			return
		}
		_ = a.reconcileIPPool(r.Context())
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
	default:
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "PATCH or DELETE required")
	}
}

func (a *App) testIPPoolNode(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	client, err := a.clientForNode(&id)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "node_unavailable", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.ipify.org?format=json", nil)
	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()
	status, message, exitIP := "healthy", "", ""
	if err == nil {
		defer resp.Body.Close()
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		if readErr != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
			err = fmt.Errorf("exit IP service returned HTTP %d", resp.StatusCode)
		} else {
			var payload struct {
				IP string `json:"ip"`
			}
			if json.Unmarshal(body, &payload) != nil || net.ParseIP(strings.TrimSpace(payload.IP)) == nil {
				err = errors.New("exit IP service returned an invalid address")
			} else {
				exitIP = strings.TrimSpace(payload.IP)
			}
		}
	}
	if err != nil {
		status, message = "unhealthy", sanitizeError(err.Error())
	}
	_, _ = a.db.Exec(`UPDATE ip_pool_nodes SET status=?,last_error=?,last_checked_at=?,last_latency_ms=?,exit_ip=?,updated_at=? WHERE id=?`, status, message, now(), latency, exitIP, now(), id)
	if err != nil {
		fail(w, http.StatusBadGateway, "node_test_failed", message)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": status, "exit_ip": exitIP, "latency_ms": latency})
}

func buildSingBoxConfig(nodes []storedIPPoolNode) ([]byte, []storedIPPoolNode, map[int64]error, error) {
	inbounds := make([]any, 0, len(nodes))
	outbounds := make([]any, 0, len(nodes))
	rules := make([]any, 0, len(nodes))
	valid := make([]storedIPPoolNode, 0, len(nodes))
	failures := map[int64]error{}
	for _, node := range nodes {
		tag := fmt.Sprintf("node-%d", node.ID)
		outbound, _, _, err := parseProxyShareLink(node.ShareLink, tag)
		if err != nil {
			failures[node.ID] = err
			continue
		}
		inTag := "in-" + tag
		inbounds = append(inbounds, map[string]any{"type": "socks", "tag": inTag, "listen": "127.0.0.1", "listen_port": node.LocalPort})
		outbounds = append(outbounds, outbound)
		rules = append(rules, map[string]any{"inbound": []string{inTag}, "action": "route", "outbound": tag})
		valid = append(valid, node)
	}
	config := map[string]any{
		"log":       map[string]any{"level": "warn", "timestamp": true},
		"inbounds":  inbounds,
		"outbounds": outbounds,
		"route":     map[string]any{"rules": rules},
	}
	encoded, err := json.MarshalIndent(config, "", "  ")
	return encoded, valid, failures, err
}

func parseProxyShareLink(raw, tag string) (map[string]any, string, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, "", "", errors.New("proxy share link is empty")
	}
	if strings.HasPrefix(raw, "{") {
		var outbound map[string]any
		if err := json.Unmarshal([]byte(raw), &outbound); err != nil {
			return nil, "", "", fmt.Errorf("invalid sing-box outbound JSON: %w", err)
		}
		protocol, _ := outbound["type"].(string)
		protocol = strings.ToLower(strings.TrimSpace(protocol))
		server, _ := outbound["server"].(string)
		if protocol == "" || server == "" {
			return nil, "", "", errors.New("sing-box outbound JSON requires type and server")
		}
		if !allowedCustomOutboundType(protocol) {
			return nil, "", "", fmt.Errorf("sing-box outbound type %q is not allowed for an IP pool node", protocol)
		}
		outbound["tag"] = tag
		return outbound, strings.ToLower(protocol), serverLabel(server, anyInt(outbound["server_port"])), nil
	}
	schemeEnd := strings.Index(raw, "://")
	if schemeEnd < 1 {
		return nil, "", "", errors.New("unsupported proxy link; paste a standard share URI or sing-box outbound JSON")
	}
	scheme := strings.ToLower(raw[:schemeEnd])
	switch scheme {
	case "socks", "socks5", "socks5h", "socks4", "socks4a":
		return parseSOCKSLink(raw, tag)
	case "http", "https":
		return parseHTTPProxyLink(raw, tag)
	case "ss":
		return parseShadowsocksLink(raw, tag)
	case "trojan":
		return parseTrojanLink(raw, tag)
	case "vless":
		return parseVLESSLink(raw, tag)
	case "vmess":
		return parseVMessLink(raw, tag)
	case "hysteria2", "hy2":
		return parseHysteria2Link(raw, tag)
	case "hysteria":
		return parseHysteriaLink(raw, tag)
	case "tuic":
		return parseTUICLink(raw, tag)
	case "anytls":
		return parseAnyTLSLink(raw, tag)
	default:
		return nil, "", "", fmt.Errorf("proxy protocol %q is not supported by share-link import; use sing-box outbound JSON for advanced protocols", scheme)
	}
}

func parseURL(raw string) (*url.URL, string, int, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, "", 0, err
	}
	host := strings.TrimSpace(u.Hostname())
	port, err := strconv.Atoi(u.Port())
	if host == "" || err != nil || port < 1 || port > 65535 {
		return nil, "", 0, errors.New("proxy link requires a valid host and port")
	}
	return u, host, port, nil
}

func allowedCustomOutboundType(protocol string) bool {
	switch protocol {
	case "socks", "http", "shadowsocks", "shadowtls", "vmess", "trojan", "wireguard", "hysteria", "hysteria2", "vless", "tuic", "ssh", "anytls", "naive", "tor":
		return true
	default:
		return false
	}
}

func parseSOCKSLink(raw, tag string) (map[string]any, string, string, error) {
	u, host, port, err := parseURL(raw)
	if err != nil {
		return nil, "", "", err
	}
	version := "5"
	if strings.HasPrefix(strings.ToLower(u.Scheme), "socks4") {
		version = "4"
	}
	out := map[string]any{"type": "socks", "tag": tag, "server": host, "server_port": port, "version": version}
	if u.User != nil {
		out["username"] = u.User.Username()
		if password, ok := u.User.Password(); ok {
			out["password"] = password
		}
	}
	return out, "socks" + version, serverLabel(host, port), nil
}

func parseHTTPProxyLink(raw, tag string) (map[string]any, string, string, error) {
	u, host, port, err := parseURL(raw)
	if err != nil {
		return nil, "", "", err
	}
	out := map[string]any{"type": "http", "tag": tag, "server": host, "server_port": port}
	if u.User != nil {
		out["username"] = u.User.Username()
		if password, ok := u.User.Password(); ok {
			out["password"] = password
		}
	}
	if strings.EqualFold(u.Scheme, "https") {
		out["tls"] = map[string]any{"enabled": true, "server_name": firstNonEmpty(u.Query().Get("sni"), host), "insecure": queryBool(u.Query(), "insecure", "allowInsecure")}
	}
	return out, strings.ToLower(u.Scheme), serverLabel(host, port), nil
}

func parseShadowsocksLink(raw, tag string) (map[string]any, string, string, error) {
	withoutScheme := strings.TrimPrefix(raw, "ss://")
	fragmentAt := strings.Index(withoutScheme, "#")
	if fragmentAt >= 0 {
		withoutScheme = withoutScheme[:fragmentAt]
	}
	queryRaw := ""
	if queryAt := strings.Index(withoutScheme, "?"); queryAt >= 0 {
		queryRaw, withoutScheme = withoutScheme[queryAt+1:], withoutScheme[:queryAt]
	}
	var userInfo, address string
	if at := strings.LastIndex(withoutScheme, "@"); at >= 0 {
		userInfo, address = withoutScheme[:at], withoutScheme[at+1:]
		decoded, err := decodeBase64URL(userInfo)
		if err == nil {
			userInfo = decoded
		}
	} else {
		decoded, err := decodeBase64URL(withoutScheme)
		if err != nil {
			return nil, "", "", errors.New("invalid Shadowsocks share link")
		}
		at := strings.LastIndex(decoded, "@")
		if at < 0 {
			return nil, "", "", errors.New("invalid Shadowsocks share link")
		}
		userInfo, address = decoded[:at], decoded[at+1:]
	}
	colon := strings.Index(userInfo, ":")
	if colon < 1 {
		return nil, "", "", errors.New("Shadowsocks link requires method and password")
	}
	host, portString, err := net.SplitHostPort(address)
	if err != nil {
		return nil, "", "", errors.New("Shadowsocks link requires a valid host and port")
	}
	port, err := strconv.Atoi(portString)
	if err != nil {
		return nil, "", "", errors.New("invalid Shadowsocks port")
	}
	out := map[string]any{"type": "shadowsocks", "tag": tag, "server": host, "server_port": port, "method": userInfo[:colon], "password": userInfo[colon+1:]}
	query, _ := url.ParseQuery(queryRaw)
	if plugin := query.Get("plugin"); plugin != "" {
		parts := strings.SplitN(plugin, ";", 2)
		out["plugin"] = parts[0]
		if len(parts) == 2 {
			out["plugin_opts"] = parts[1]
		}
	}
	return out, "shadowsocks", serverLabel(host, port), nil
}

func parseTrojanLink(raw, tag string) (map[string]any, string, string, error) {
	u, host, port, err := parseURL(raw)
	if err != nil {
		return nil, "", "", err
	}
	password := ""
	if u.User != nil {
		password = u.User.Username()
	}
	if password == "" {
		return nil, "", "", errors.New("Trojan link requires a password")
	}
	out := map[string]any{"type": "trojan", "tag": tag, "server": host, "server_port": port, "password": password}
	out["tls"] = clientTLSFromQuery(u.Query(), host, true)
	if transport := transportFromQuery(u.Query()); transport != nil {
		out["transport"] = transport
	}
	return out, "trojan", serverLabel(host, port), nil
}

func parseVLESSLink(raw, tag string) (map[string]any, string, string, error) {
	u, host, port, err := parseURL(raw)
	if err != nil {
		return nil, "", "", err
	}
	uuid := ""
	if u.User != nil {
		uuid = u.User.Username()
	}
	if uuid == "" {
		return nil, "", "", errors.New("VLESS link requires a UUID")
	}
	q := u.Query()
	out := map[string]any{"type": "vless", "tag": tag, "server": host, "server_port": port, "uuid": uuid}
	if flow := q.Get("flow"); flow != "" {
		out["flow"] = flow
	}
	if network := q.Get("network"); network != "" {
		out["network"] = network
	}
	security := strings.ToLower(q.Get("security"))
	if security == "tls" || security == "reality" {
		tls := clientTLSFromQuery(q, host, true)
		if security == "reality" {
			publicKey := firstNonEmpty(q.Get("pbk"), q.Get("publicKey"))
			if publicKey == "" {
				return nil, "", "", errors.New("VLESS Reality link requires pbk/publicKey")
			}
			tls["reality"] = map[string]any{"enabled": true, "public_key": publicKey, "short_id": firstNonEmpty(q.Get("sid"), q.Get("shortId"))}
		}
		out["tls"] = tls
	}
	if transport := transportFromQuery(q); transport != nil {
		out["transport"] = transport
	}
	if packet := firstNonEmpty(q.Get("packetEncoding"), q.Get("packet_encoding")); packet != "" {
		out["packet_encoding"] = packet
	}
	return out, map[bool]string{true: "vless-reality", false: "vless"}[security == "reality"], serverLabel(host, port), nil
}

func parseVMessLink(raw, tag string) (map[string]any, string, string, error) {
	encoded := strings.TrimPrefix(raw, "vmess://")
	if index := strings.IndexAny(encoded, "?#"); index >= 0 {
		encoded = encoded[:index]
	}
	decoded, err := decodeBase64URL(encoded)
	if err != nil {
		return nil, "", "", errors.New("invalid VMess share link")
	}
	var item map[string]any
	if err := json.Unmarshal([]byte(decoded), &item); err != nil {
		return nil, "", "", errors.New("VMess link must contain base64 JSON")
	}
	host := anyString(item["add"])
	port := anyInt(item["port"])
	uuid := anyString(item["id"])
	if host == "" || port < 1 || uuid == "" {
		return nil, "", "", errors.New("VMess link requires add, port and id")
	}
	out := map[string]any{"type": "vmess", "tag": tag, "server": host, "server_port": port, "uuid": uuid, "security": firstNonEmpty(anyString(item["scy"]), "auto")}
	if alterID := anyInt(item["aid"]); alterID > 0 {
		out["alter_id"] = alterID
	}
	q := url.Values{}
	q.Set("type", anyString(item["net"]))
	q.Set("host", anyString(item["host"]))
	q.Set("path", anyString(item["path"]))
	q.Set("serviceName", anyString(item["path"]))
	if transport := transportFromQuery(q); transport != nil {
		out["transport"] = transport
	}
	if tlsMode := strings.ToLower(anyString(item["tls"])); tlsMode == "tls" {
		fake := url.Values{}
		fake.Set("sni", firstNonEmpty(anyString(item["sni"]), host))
		fake.Set("fp", anyString(item["fp"]))
		out["tls"] = clientTLSFromQuery(fake, host, true)
	}
	return out, "vmess", serverLabel(host, port), nil
}

func parseHysteria2Link(raw, tag string) (map[string]any, string, string, error) {
	normalized := raw
	if strings.HasPrefix(raw, "hy2://") {
		normalized = "hysteria2://" + strings.TrimPrefix(raw, "hy2://")
	}
	u, host, port, err := parseURL(normalized)
	if err != nil {
		return nil, "", "", err
	}
	password := ""
	if u.User != nil {
		password = u.User.Username()
		if p, ok := u.User.Password(); ok && p != "" {
			password += ":" + p
		}
	}
	if password == "" {
		return nil, "", "", errors.New("Hysteria2 link requires a password")
	}
	q := u.Query()
	out := map[string]any{"type": "hysteria2", "tag": tag, "server": host, "server_port": port, "password": password, "tls": clientTLSFromQuery(q, host, true)}
	if obfs := q.Get("obfs"); obfs != "" {
		out["obfs"] = map[string]any{"type": obfs, "password": firstNonEmpty(q.Get("obfs-password"), q.Get("obfs_password"))}
	}
	return out, "hysteria2", serverLabel(host, port), nil
}

func parseHysteriaLink(raw, tag string) (map[string]any, string, string, error) {
	u, host, port, err := parseURL(raw)
	if err != nil {
		return nil, "", "", err
	}
	q := u.Query()
	auth := firstNonEmpty(q.Get("auth"), q.Get("auth_str"))
	if auth == "" && u.User != nil {
		auth = u.User.Username()
	}
	out := map[string]any{"type": "hysteria", "tag": tag, "server": host, "server_port": port, "tls": clientTLSFromQuery(q, host, true)}
	if auth != "" {
		out["auth_str"] = auth
	}
	if up := firstNonEmpty(q.Get("upmbps"), q.Get("up")); up != "" {
		out["up_mbps"], _ = strconv.Atoi(up)
	}
	if down := firstNonEmpty(q.Get("downmbps"), q.Get("down")); down != "" {
		out["down_mbps"], _ = strconv.Atoi(down)
	}
	if obfs := q.Get("obfs"); obfs != "" {
		out["obfs"] = obfs
	}
	return out, "hysteria", serverLabel(host, port), nil
}

func parseTUICLink(raw, tag string) (map[string]any, string, string, error) {
	u, host, port, err := parseURL(raw)
	if err != nil {
		return nil, "", "", err
	}
	uuid, password := "", ""
	if u.User != nil {
		uuid = u.User.Username()
		password, _ = u.User.Password()
	}
	if uuid == "" || password == "" {
		return nil, "", "", errors.New("TUIC link requires UUID and password")
	}
	q := u.Query()
	out := map[string]any{"type": "tuic", "tag": tag, "server": host, "server_port": port, "uuid": uuid, "password": password, "tls": clientTLSFromQuery(q, host, true)}
	if value := firstNonEmpty(q.Get("congestion_control"), q.Get("congestion-controller")); value != "" {
		out["congestion_control"] = value
	}
	if value := q.Get("udp_relay_mode"); value != "" {
		out["udp_relay_mode"] = value
	}
	return out, "tuic", serverLabel(host, port), nil
}

func parseAnyTLSLink(raw, tag string) (map[string]any, string, string, error) {
	u, host, port, err := parseURL(raw)
	if err != nil {
		return nil, "", "", err
	}
	password := ""
	if u.User != nil {
		password = u.User.Username()
		if p, ok := u.User.Password(); ok && p != "" {
			password += ":" + p
		}
	}
	if password == "" {
		return nil, "", "", errors.New("AnyTLS link requires a password")
	}
	out := map[string]any{"type": "anytls", "tag": tag, "server": host, "server_port": port, "password": password, "tls": clientTLSFromQuery(u.Query(), host, true)}
	return out, "anytls", serverLabel(host, port), nil
}

func clientTLSFromQuery(q url.Values, host string, enabled bool) map[string]any {
	tls := map[string]any{"enabled": enabled, "server_name": firstNonEmpty(q.Get("sni"), q.Get("serverName"), q.Get("peer"), host)}
	if queryBool(q, "insecure", "allowInsecure", "allow_insecure") {
		tls["insecure"] = true
	}
	if alpn := q.Get("alpn"); alpn != "" {
		tls["alpn"] = splitNonEmpty(alpn, ",")
	}
	if fingerprint := firstNonEmpty(q.Get("fp"), q.Get("fingerprint")); fingerprint != "" && fingerprint != "none" {
		tls["utls"] = map[string]any{"enabled": true, "fingerprint": fingerprint}
	}
	return tls
}

func transportFromQuery(q url.Values) map[string]any {
	typeName := strings.ToLower(firstNonEmpty(q.Get("type"), q.Get("net")))
	switch typeName {
	case "", "tcp", "raw":
		return nil
	case "ws", "websocket":
		transport := map[string]any{"type": "ws", "path": firstNonEmpty(q.Get("path"), "/")}
		if host := q.Get("host"); host != "" {
			transport["headers"] = map[string]any{"Host": host}
		}
		return transport
	case "httpupgrade":
		transport := map[string]any{"type": "httpupgrade", "path": firstNonEmpty(q.Get("path"), "/")}
		if host := q.Get("host"); host != "" {
			transport["host"] = host
		}
		return transport
	case "grpc":
		return map[string]any{"type": "grpc", "service_name": firstNonEmpty(q.Get("serviceName"), q.Get("service_name"))}
	case "http", "h2":
		transport := map[string]any{"type": "http", "path": firstNonEmpty(q.Get("path"), "/")}
		if host := q.Get("host"); host != "" {
			transport["host"] = splitNonEmpty(host, ",")
		}
		return transport
	case "quic":
		return map[string]any{"type": "quic"}
	default:
		return map[string]any{"type": typeName}
	}
}

func queryBool(q url.Values, keys ...string) bool {
	for _, key := range keys {
		switch strings.ToLower(strings.TrimSpace(q.Get(key))) {
		case "1", "true", "yes", "on":
			return true
		}
	}
	return false
}

func decodeBase64URL(value string) (string, error) {
	value = strings.TrimSpace(value)
	for _, encoding := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding} {
		if decoded, err := encoding.DecodeString(value); err == nil {
			return string(decoded), nil
		}
	}
	return "", errors.New("invalid base64")
}

func anyString(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return v.String()
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return ""
	}
}

func anyInt(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		n, _ := strconv.Atoi(v.String())
		return n
	case string:
		n, _ := strconv.Atoi(v)
		return n
	default:
		return 0
	}
}

func serverLabel(host string, port int) string {
	if port <= 0 {
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func splitNonEmpty(value, separator string) []string {
	var out []string
	for _, item := range strings.Split(value, separator) {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}
