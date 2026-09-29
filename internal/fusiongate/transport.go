package fusiongate

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

func newUpstreamHTTPTransport(cfg Config) *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		// Provider request contexts enforce the configured timeout. A fixed
		// response-header limit breaks long-running Responses-to-Chat bridges.
		ResponseHeaderTimeout: 0,
	}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if cfg.AllowPrivateUpstreams {
			return dialer.DialContext(ctx, network, address)
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
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("upstream hostname resolved to no usable addresses")
		}
		return nil, lastErr
	}
	return transport
}

func upstreamRedirectPolicy(cfg Config) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return fmt.Errorf("too many upstream redirects")
		}
		if err := validateUpstream(req.URL.String(), cfg); err != nil {
			return err
		}
		if len(via) > 0 && !strings.EqualFold(req.URL.Hostname(), via[0].URL.Hostname()) {
			return fmt.Errorf("cross-host upstream redirect is blocked")
		}
		return nil
	}
}

func newUpstreamHTTPClient(cfg Config) *http.Client {
	return &http.Client{Transport: newUpstreamHTTPTransport(cfg), CheckRedirect: upstreamRedirectPolicy(cfg)}
}

// Each egress owns two pools: raw transfer preserves compression bytes, while
// conversion uses the original transport's automatic decompression.
func (a *App) nativeTransport(base *http.Transport) *http.Transport {
	a.transportMu.Lock()
	defer a.transportMu.Unlock()
	if a.nativeTransports == nil {
		a.nativeTransports = make(map[*http.Transport]*http.Transport)
	}
	if t := a.nativeTransports[base]; t != nil {
		return t
	}
	t := base.Clone()
	t.DisableCompression = true
	a.nativeTransports[base] = t
	return t
}
func (a *App) retireTransport(base *http.Transport) {
	a.transportMu.Lock()
	defer a.transportMu.Unlock()
	base.CloseIdleConnections()
	if t := a.nativeTransports[base]; t != nil {
		t.CloseIdleConnections()
		delete(a.nativeTransports, base)
	}
}
func (a *App) closeInferenceTransports() {
	a.transportMu.Lock()
	defer a.transportMu.Unlock()
	for base, t := range a.nativeTransports {
		base.CloseIdleConnections()
		t.CloseIdleConnections()
	}
	a.nativeTransports = nil
	if a.client != nil {
		a.client.CloseIdleConnections()
	}
	if a.pricingClient != nil {
		a.pricingClient.CloseIdleConnections()
	}
}
func (a *App) streamTimeouts(p Provider) (start, idle time.Duration) {
	start = time.Duration(p.RequestTimeoutMS) * time.Millisecond
	if start <= 0 {
		start = DefaultStreamStartTimeout
	}
	if a.cfg.StreamStartTimeout > 0 {
		start = a.cfg.StreamStartTimeout
	}
	if p.StreamStartTimeoutMS > 0 {
		start = time.Duration(p.StreamStartTimeoutMS) * time.Millisecond
	}
	idle = 5 * time.Minute
	if a.cfg.StreamIdleTimeout > 0 {
		idle = a.cfg.StreamIdleTimeout
	}
	if p.StreamIdleTimeoutMS > 0 {
		idle = time.Duration(p.StreamIdleTimeoutMS) * time.Millisecond
	}
	return
}
