package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"order_gateway/core"
)

type chainTestSource struct{ snapshot *KeySnapshot }

func (s chainTestSource) Fetch(context.Context, uint64) (*KeySnapshot, bool, error) {
	return s.snapshot, true, nil
}

func readyChainAPIKeys(t *testing.T) *APIKeys {
	t.Helper()
	hash := sha256.Sum256([]byte("secret"))
	keys := NewAPIKeys(chainTestSource{snapshot: &KeySnapshot{Version: 1, Keys: []KeyEntry{{
		KeyID: "key-a", AccountID: 7, SecretSHA256: hex.EncodeToString(hash[:]),
	}}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := keys.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return keys
}

func readyChainJWT(t *testing.T, now time.Time) (*JWTVerifier, string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cache := NewJWKSCache("", nil, time.Minute, func() time.Time { return now }, nil)
	cache.keys.Store(&map[string]ed25519.PublicKey{"jwt-kid": public})
	claims := jwt.RegisteredClaims{
		Issuer: "protrader-identity", Audience: jwt.ClaimStrings{"order-gateway"}, Subject: "9",
		ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)), NotBefore: jwt.NewNumericDate(now.Add(-time.Minute)),
		IssuedAt: jwt.NewNumericDate(now.Add(-time.Minute)),
	}
	return NewJWTVerifier(cache, "protrader-identity", "order-gateway", 30*time.Second, func() time.Time { return now }), mint(t, private, "jwt-kid", claims)
}

func TestChainVerifyRouting(t *testing.T) {
	apiKeys := readyChainAPIKeys(t)
	now := time.Now().UTC().Truncate(time.Second)
	jwtVerifier, validBearer := readyChainJWT(t, now)
	chain := Chain{APIKeys: apiKeys, JWT: jwtVerifier}

	principal, err := chain.Verify(Credentials{KeyID: "key-a", Secret: "secret"})
	if err != nil || principal.Account != core.AccountID(7) || principal.Method != MethodAPIKey {
		t.Fatalf("API key route = (%+v, %v), want account 7 API key", principal, err)
	}
	principal, err = chain.Verify(Credentials{Bearer: validBearer})
	if err != nil || principal.Account != core.AccountID(9) || principal.Method != MethodBrowser {
		t.Fatalf("Bearer route = (%+v, %v), want account 9 browser", principal, err)
	}
	principal, err = chain.Verify(Credentials{KeyID: "key-a", Secret: "secret", Bearer: "invalid"})
	if err != nil || principal.Method != MethodAPIKey {
		t.Fatalf("valid API key plus invalid bearer = (%+v, %v), want API key success", principal, err)
	}
	if _, err := chain.Verify(Credentials{KeyID: "wrong", Secret: "wrong", Bearer: validBearer}); err != ErrBadCredentials {
		t.Fatalf("invalid API key plus valid bearer error = %v, want ErrBadCredentials", err)
	}
	if _, err := chain.Verify(Credentials{}); err != ErrNoCredentials {
		t.Fatalf("empty credentials error = %v, want ErrNoCredentials", err)
	}
	if _, err := (Chain{APIKeys: apiKeys}).Verify(Credentials{Bearer: validBearer}); err != ErrBadCredentials {
		t.Fatalf("Bearer with nil JWT error = %v, want ErrBadCredentials", err)
	}
}

func TestChainReady(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	jwtReady, _ := readyChainJWT(t, now)
	apiReady := readyChainAPIKeys(t)
	apiNotReady := NewAPIKeys(chainTestSource{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	jwtNotReady := NewJWTVerifier(NewJWKSCache("", nil, time.Minute, time.Now, nil), "iss", "aud", 0, time.Now)
	tests := []struct {
		name string
		c    Chain
		want bool
	}{
		{name: "API keys not ready", c: Chain{APIKeys: apiNotReady}, want: false},
		{name: "API keys ready without JWT", c: Chain{APIKeys: apiReady}, want: true},
		{name: "JWT not ready", c: Chain{APIKeys: apiReady, JWT: jwtNotReady}, want: false},
		{name: "both ready", c: Chain{APIKeys: apiReady, JWT: jwtReady}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.c.Ready(); got != tt.want {
				t.Errorf("Ready() = %v, want %v", got, tt.want)
			}
		})
	}
}
