package auth

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// jwk is one key in the JWKS JSON (contract B).
type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	X   string `json:"x"`
}

type jwkSet struct {
	Keys []jwk `json:"keys"`
}

func ParseJWKS(data []byte) (map[string]ed25519.PublicKey, error) {
	var set jwkSet
	if err := json.Unmarshal(data, &set); err != nil {
		return nil, err
	}

	keys := make(map[string]ed25519.PublicKey)
	for _, key := range set.Keys {
		if key.Kty != "OKP" || key.Crv != "Ed25519" {
			continue
		}
		if key.Use != "" && key.Use != "sig" {
			continue
		}
		if key.Alg != "" && key.Alg != "EdDSA" {
			continue
		}
		if key.Kid == "" {
			return nil, fmt.Errorf("JWKS key has empty kid")
		}
		if _, exists := keys[key.Kid]; exists {
			return nil, fmt.Errorf("duplicate JWKS kid %q", key.Kid)
		}
		raw, err := base64.RawURLEncoding.DecodeString(key.X)
		if err != nil {
			return nil, fmt.Errorf("decode JWKS key %q: %w", key.Kid, err)
		}
		if len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("JWKS key %q has invalid public key length %d", key.Kid, len(raw))
		}
		keys[key.Kid] = ed25519.PublicKey(raw)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("JWKS contains no usable Ed25519 keys")
	}
	return keys, nil
}

type JWKSCache struct {
	url    string
	client *http.Client
	minGap time.Duration // never re-download more often than this on "unknown kid"
	keys   atomic.Pointer[map[string]ed25519.PublicKey]
	wake   chan struct{} // buffered(1): "please refresh soon"
	now    func() time.Time
	log    *slog.Logger

	mu          sync.Mutex // guards lastFetched only
	lastFetched time.Time
}

func NewJWKSCache(url string, client *http.Client, minGap time.Duration, now func() time.Time, log *slog.Logger) *JWKSCache {
	return &JWKSCache{
		url:    url,
		client: client,
		minGap: minGap,
		wake:   make(chan struct{}, 1),
		now:    now,
		log:    log,
	}
}
func (c *JWKSCache) Refresh(ctx context.Context) error {
	data, err := fetchURL(ctx, c.client, c.url, 1<<20)
	if err != nil {
		return fmt.Errorf("fetch JWKS: %w", err)
	}
	keys, err := ParseJWKS(data)
	if err != nil {
		return fmt.Errorf("parse JWKS: %w", err)
	}
	c.keys.Store(&keys)

	c.mu.Lock()
	c.lastFetched = c.now()
	c.mu.Unlock()

	if c.log != nil {
		kids := make([]string, 0, len(keys))
		for kid := range keys {
			kids = append(kids, kid)
		}
		c.log.Info("refreshed JWKS", "key_count", len(keys), "kids", kids)
	}
	return nil

}
func (c *JWKSCache) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.refreshAndLog(ctx)
		case <-c.wake:
			c.mu.Lock()
			tooSoon := c.now().Sub(c.lastFetched) < c.minGap
			c.mu.Unlock()
			if !tooSoon {
				c.refreshAndLog(ctx)
			}
		}
	}
}

func (c *JWKSCache) refreshAndLog(ctx context.Context) {
	err := c.Refresh(ctx)
	if err != nil {
		if c.log != nil {
			c.log.Error("refresh JWKS", "error", err)
		}
	}
}
func (c *JWKSCache) Ready() bool {
	return c.keys.Load() != nil
}
func (c *JWKSCache) key(kid string) (ed25519.PublicKey, bool) {
	p := c.keys.Load()
	if p == nil {
		return nil, false
	}
	key, ok := (*p)[kid]
	if !ok {
		select {
		case c.wake <- struct{}{}:
		default:
		}
		return nil, false
	}
	return key, ok
}
