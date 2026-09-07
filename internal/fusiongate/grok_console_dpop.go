package fusiongate

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

const (
	consoleBaseURL       = "https://console.x.ai"
	consoleDPoPTokenPath = "/dpop/token"
	dpopCacheMaxEntries  = 4096
)

type dpopSession struct {
	accessToken string
	privateKey  *ecdsa.PrivateKey
	expiresAt   time.Time
	skewSeconds int64
}

type dpopSessionCache struct {
	mu       sync.Mutex
	sessions map[string]*dpopSession
	order    []string
}

func newDPoPSessionCache() *dpopSessionCache {
	return &dpopSessionCache{sessions: make(map[string]*dpopSession)}
}

func (c *dpopSessionCache) get(key string) (*dpopSession, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.sessions[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(s.expiresAt) {
		delete(c.sessions, key)
		return nil, false
	}
	return s, true
}

func (c *dpopSessionCache) put(key string, s *dpopSession) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.sessions[key]; !exists {
		c.order = append(c.order, key)
	}
	c.sessions[key] = s
	for len(c.sessions) > dpopCacheMaxEntries && len(c.order) > 0 {
		evict := c.order[0]
		c.order = c.order[1:]
		delete(c.sessions, evict)
	}
}

func (c *dpopSessionCache) invalidate(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.sessions, key)
}

func generateDPoPKeyPair() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

func dpopJWKThumbprint(pub *ecdsa.PublicKey) string {
	x := base64.RawURLEncoding.EncodeToString(pub.X.Bytes())
	y := base64.RawURLEncoding.EncodeToString(pub.Y.Bytes())
	canonical := fmt.Sprintf(`{"crv":"P-256","kty":"EC","x":"%s","y":"%s"}`, x, y)
	hash := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(hash[:])
}

func dpopJWK(pub *ecdsa.PublicKey) map[string]string {
	xBytes := pub.X.Bytes()
	yBytes := pub.Y.Bytes()
	coordLen := 32
	if len(xBytes) < coordLen {
		padded := make([]byte, coordLen)
		copy(padded[coordLen-len(xBytes):], xBytes)
		xBytes = padded
	}
	if len(yBytes) < coordLen {
		padded := make([]byte, coordLen)
		copy(padded[coordLen-len(yBytes):], yBytes)
		yBytes = padded
	}
	return map[string]string{
		"kty": "EC",
		"crv": "P-256",
		"x":   base64.RawURLEncoding.EncodeToString(xBytes),
		"y":   base64.RawURLEncoding.EncodeToString(yBytes),
	}
}

func signDPoPJWT(key *ecdsa.PrivateKey, claims map[string]any) (string, error) {
	header := map[string]any{
		"typ": "dpop+jwt",
		"alg": "ES256",
		"jwk": dpopJWK(&key.PublicKey),
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	headerB64 := base64.RawURLEncoding.EncodeToString(headerJSON)
	claimsB64 := base64.RawURLEncoding.EncodeToString(claimsJSON)
	signingInput := headerB64 + "." + claimsB64

	hash := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, key, hash[:])
	if err != nil {
		return "", err
	}
	rBytes := r.Bytes()
	sBytes := s.Bytes()
	sigLen := 32
	sig := make([]byte, sigLen*2)
	copy(sig[sigLen-len(rBytes):sigLen], rBytes)
	copy(sig[sigLen*2-len(sBytes):], sBytes)
	sigB64 := base64.RawURLEncoding.EncodeToString(sig)

	return signingInput + "." + sigB64, nil
}

func dpopProofForTokenExchange(key *ecdsa.PrivateKey, httpMethod, httpURI string) (string, error) {
	jtiBytes := make([]byte, 16)
	if _, err := rand.Read(jtiBytes); err != nil {
		return "", err
	}
	claims := map[string]any{
		"jti": base64.RawURLEncoding.EncodeToString(jtiBytes),
		"htm": httpMethod,
		"htu": httpURI,
		"iat": time.Now().Unix(),
	}
	return signDPoPJWT(key, claims)
}

func dpopProofForResource(key *ecdsa.PrivateKey, accessToken string, httpMethod, httpURI string, skewSeconds int64) (string, error) {
	jtiBytes := make([]byte, 16)
	if _, err := rand.Read(jtiBytes); err != nil {
		return "", err
	}
	ath := sha256.Sum256([]byte(accessToken))
	claims := map[string]any{
		"jti": base64.RawURLEncoding.EncodeToString(jtiBytes),
		"htm": httpMethod,
		"htu": httpURI,
		"iat": time.Now().Unix() + skewSeconds,
		"ath": base64.RawURLEncoding.EncodeToString(ath[:]),
	}
	return signDPoPJWT(key, claims)
}

type dpopTokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
}

func (a *App) exchangeDPoPToken(ctx context.Context, ssoToken string, key *ecdsa.PrivateKey, nodeID *int64) (*dpopSession, error) {
	tokenURL := consoleBaseURL + consoleDPoPTokenPath
	proof, err := dpopProofForTokenExchange(key, "POST", tokenURL)
	if err != nil {
		return nil, fmt.Errorf("dpop proof generation failed: %w", err)
	}

	body, _ := json.Marshal(map[string]string{"token": ssoToken})
	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("DPoP", proof)
	req.Header.Set("Accept", "application/json")

	resp, err := a.doProviderRequest(req, nodeID)
	if err != nil {
		return nil, fmt.Errorf("dpop token exchange failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("dpop token exchange read failed: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("dpop token exchange returned %d: %s", resp.StatusCode, string(respBody))
	}

	var skewSeconds int64
	if serverDate := resp.Header.Get("Date"); serverDate != "" {
		if parsed, parseErr := http.ParseTime(serverDate); parseErr == nil {
			skewSeconds = int64(parsed.Sub(time.Now()).Seconds())
		}
	}

	var tokenResp dpopTokenResponse
	if err := json.Unmarshal(respBody, &tokenResp); err != nil {
		return nil, fmt.Errorf("dpop token response invalid: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return nil, errors.New("dpop token exchange returned empty access token")
	}

	expiresIn := tokenResp.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}

	return &dpopSession{
		accessToken: tokenResp.AccessToken,
		privateKey:  key,
		expiresAt:   time.Now().Add(time.Duration(expiresIn-60) * time.Second),
		skewSeconds: skewSeconds,
	}, nil
}

func (a *App) getDPoPSession(ctx context.Context, ssoToken string, nodeID *int64) (*dpopSession, error) {
	cacheKey := fmt.Sprintf("%x", sha256.Sum256([]byte(ssoToken)))
	if session, ok := a.dpopCache.get(cacheKey); ok {
		return session, nil
	}

	key, err := generateDPoPKeyPair()
	if err != nil {
		return nil, fmt.Errorf("dpop key generation failed: %w", err)
	}

	session, err := a.exchangeDPoPToken(ctx, ssoToken, key, nodeID)
	if err != nil {
		return nil, err
	}

	a.dpopCache.put(cacheKey, session)
	return session, nil
}

func (a *App) invalidateDPoPSession(ssoToken string) {
	cacheKey := fmt.Sprintf("%x", sha256.Sum256([]byte(ssoToken)))
	a.dpopCache.invalidate(cacheKey)
}

func setConsoleDPoPAuth(req *http.Request, session *dpopSession) error {
	proof, err := dpopProofForResource(
		session.privateKey,
		session.accessToken,
		req.Method,
		req.URL.String(),
		session.skewSeconds,
	)
	if err != nil {
		return fmt.Errorf("dpop resource proof failed: %w", err)
	}
	req.Header.Set("Authorization", "DPoP "+session.accessToken)
	req.Header.Set("DPoP", proof)
	return nil
}

func isDPoPProofRequired(body []byte) bool {
	return bytes.Contains(body, []byte("use_dpop_nonce"))
}

func parseDPoPNonce(body []byte) string {
	var result struct {
		DPoPNonce string `json:"dpop_nonce"`
	}
	if json.Unmarshal(body, &result) == nil {
		return result.DPoPNonce
	}
	return ""
}

