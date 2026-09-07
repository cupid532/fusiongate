package fusiongate

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

var (
	cfBypassH2Client     *http.Client
	cfBypassH2ClientOnce sync.Once
)

func cloudflareBypassClient() *http.Client {
	cfBypassH2ClientOnce.Do(func() {
		cfBypassH2Client = &http.Client{
			Transport: &http2.Transport{
				DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
					return dialTLSChrome(ctx, network, addr)
				},
			},
			Timeout: 60 * time.Second,
		}
	})
	return cfBypassH2Client
}

func dialTLSChrome(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	conn, err := dialer.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}

	config := &utls.Config{
		ServerName:         host,
		InsecureSkipVerify: false,
	}

	uconn := utls.UClient(conn, config, utls.HelloChrome_Auto)
	if err := uconn.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, err
	}

	return uconn, nil
}
