package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"

	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func mint(t *testing.T, private ed25519.PrivateKey, kid string, claims jwt.RegisteredClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	if kid != "" {
		token.Header["kid"] = kid
	}
	encoded, err := token.SignedString(private)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func jwtTestClaims(now time.Time) jwt.RegisteredClaims {
	return jwt.RegisteredClaims{
		Issuer: "protrader-identity", Audience: jwt.ClaimStrings{"order-gateway"}, Subject: "7",
		ExpiresAt: jwt.NewNumericDate(now.Add(15 * time.Minute)), NotBefore: jwt.NewNumericDate(now.Add(-time.Minute)),
		IssuedAt: jwt.NewNumericDate(now.Add(-time.Minute)),
	}
}

func setupJWTTest(t *testing.T, now time.Time) (*JWTVerifier, ed25519.PrivateKey, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, otherPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "OKP", "crv": "Ed25519", "kid": "known", "use": "sig", "alg": "EdDSA",
		"x": base64.RawURLEncoding.EncodeToString(public),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "jwks.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cache := NewJWKSCache((&url.URL{Scheme: "file", Path: path}).String(), nil, time.Minute, func() time.Time { return now }, nil)
	if err := cache.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	verifier := NewJWTVerifier(cache, "protrader-identity", "order-gateway", 30*time.Second, func() time.Time { return now })
	return verifier, private, otherPrivate
}
func TestJWTVerifier(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	verifier, private, otherPrivate := setupJWTTest(t, now)
	claims := jwtTestClaims(now)
	valid := mint(t, private, "known", claims)
	claimsAud := claims
	claimsAud.Audience = jwt.ClaimStrings{"other", "order-gateway"}
	validAudList := mint(t, private, "known", claimsAud)

	missingKid := mint(t, private, "", claims)
	unknownKid := mint(t, private, "unknown", claims)
	wrongKey := mint(t, otherPrivate, "known", claims)
	withoutExp := claims
	withoutExp.ExpiresAt = nil
	noExp := mint(t, private, "known", withoutExp)
	withoutNbf := claims
	withoutNbf.NotBefore = nil
	noNbf := mint(t, private, "known", withoutNbf)
	withoutIat := claims
	withoutIat.IssuedAt = nil
	noIat := mint(t, private, "known", withoutIat)
	wrongIssuer := claims
	wrongIssuer.Issuer = "wrong"
	wrongIss := mint(t, private, "known", wrongIssuer)
	wrongAudience := claims
	wrongAudience.Audience = jwt.ClaimStrings{"different"}
	wrongAud := mint(t, private, "known", wrongAudience)
	wrongSubject := claims
	wrongSubject.Subject = "abc"
	abcSub := mint(t, private, "known", wrongSubject)
	zeroSubject := claims
	zeroSubject.Subject = "0"
	zeroSub := mint(t, private, "known", zeroSubject)
	missingSubject := claims
	missingSubject.Subject = ""
	noSub := mint(t, private, "known", missingSubject)
	longLifetime := claims
	longLifetime.ExpiresAt = jwt.NewNumericDate(claims.IssuedAt.Time.Add(10 * 365 * 24 * time.Hour))
	longLifetimeToken := mint(t, private, "known", longLifetime)
	futureIssuedAt := claims
	futureIssuedAt.IssuedAt = jwt.NewNumericDate(now.Add(24 * time.Hour))
	futureIssuedAtToken := mint(t, private, "known", futureIssuedAt)

	hmac := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	hmac.Header["kid"] = "known"
	hmacToken, err := hmac.SignedString([]byte(private.Public().(ed25519.PublicKey)))
	if err != nil {
		t.Fatal(err)
	}

	noneHeader, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT", "kid": "known"})
	claimsJSON, _ := json.Marshal(claims)
	noneToken := base64.RawURLEncoding.EncodeToString(noneHeader) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON) + "."

	tests := []struct {
		name      string
		token     string
		wantOK    bool
		wantError error
	}{
		{name: "valid token", token: valid, wantOK: true},
		{name: "expired one minute", token: mint(t, private, "known", func() jwt.RegisteredClaims {
			c := claims
			c.ExpiresAt = jwt.NewNumericDate(now.Add(-time.Minute))
			return c
		}())},
		{name: "expired within leeway", token: mint(t, private, "known", func() jwt.RegisteredClaims {
			c := claims
			c.ExpiresAt = jwt.NewNumericDate(now.Add(-10 * time.Second))
			return c
		}()), wantOK: true},
		{name: "future nbf", token: mint(t, private, "known", func() jwt.RegisteredClaims {
			c := claims
			c.NotBefore = jwt.NewNumericDate(now.Add(time.Minute))
			return c
		}())},
		{name: "no expiration", token: noExp},
		{name: "expiration ten years after issued at", token: longLifetimeToken},
		{name: "issued at one day in the future", token: futureIssuedAtToken},
		{name: "wrong issuer", token: wrongIss},
		{name: "wrong audience", token: wrongAud},
		{name: "audience list contains configured audience", token: validAudList, wantOK: true},
		{name: "different signing key", token: wrongKey},
		{name: "unknown kid", token: unknownKid},
		{name: "missing kid", token: missingKid},
		{name: "none algorithm", token: noneToken},
		{name: "HS256 algorithm", token: hmacToken},
		{name: "nonnumeric subject", token: abcSub},
		{name: "zero subject", token: zeroSub},
		{name: "missing subject", token: noSub},
		{name: "missing nbf", token: noNbf},
		{name: "missing iat", token: noIat},
		{name: "hello", token: "hello"},
		{name: "oversized bearer", token: string(make([]byte, 5000))},
		{name: "empty bearer", wantError: ErrNoCredentials},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			principal, err := verifier.Verify(Credentials{Bearer: tt.token})
			if tt.wantError != nil {
				if !errors.Is(err, tt.wantError) {
					t.Fatalf("Verify() error = %v, want %v", err, tt.wantError)
				}
				return
			}
			if tt.wantOK {
				if err != nil || principal.Account != 7 || principal.Method != MethodBrowser || principal.KeyID != "" {
					t.Fatalf("Verify() = (%+v, %v), want browser principal for account 7", principal, err)
				}
				return
			}
			if !errors.Is(err, ErrBadCredentials) {
				t.Fatalf("Verify() error = %v, want ErrBadCredentials", err)
			}
		})
	}

	if _, err := verifier.Verify(Credentials{Bearer: valid}); err != nil {
		t.Fatalf("sanity valid verify: %v", err)
	}
}

func TestJWTVerifierNotReadyAndConcurrent(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cache := NewJWKSCache("file:///nonexistent", nil, time.Minute, func() time.Time { return now }, nil)
	verifier := NewJWTVerifier(cache, "protrader-identity", "order-gateway", 30*time.Second, func() time.Time { return now })
	valid := mint(t, private, "known", jwtTestClaims(now))
	if _, err := verifier.Verify(Credentials{Bearer: valid}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("Verify() before JWKS load = %v, want ErrNotReady", err)
	}

	cache.keys.Store(&map[string]ed25519.PublicKey{"known": public})
	const workers = 50
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				if _, err := verifier.Verify(Credentials{Bearer: valid}); err != nil {
					t.Errorf("Verify() error: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
