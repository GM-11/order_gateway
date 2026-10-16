package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"io"
	"net/url"

	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"order_gateway/auth"
)

func TestKeygenMintAndVerify(t *testing.T) {
	dir := t.TempDir()
	if err := generateKey("dev-test", dir); err != nil {
		t.Fatalf("generateKey(): %v", err)
	}
	privatePath := filepath.Join(dir, "dev-test.key")
	privateText, err := os.ReadFile(privatePath)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(string(privateText))
	if err != nil || len(decoded) != ed25519.PrivateKeySize {
		t.Fatalf("generated private key invalid: len=%d err=%v", len(decoded), err)
	}
	if info, err := os.Stat(privatePath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("private key mode = %v, err=%v; want 0600", info.Mode().Perm(), err)
	}

	now := time.Now().Truncate(time.Second)
	valid, err := mintToken(ed25519.PrivateKey(decoded), "dev-test", 7, 15*time.Minute, "protrader-identity", "order-gateway", now)
	if err != nil {
		t.Fatalf("mintToken(): %v", err)
	}
	jwksPath := filepath.Join(dir, "jwks.json")
	cache := auth.NewJWKSCache((&url.URL{Scheme: "file", Path: jwksPath}).String(), nil, time.Minute, func() time.Time { return now }, nil)
	if err := cache.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh JWKS: %v", err)
	}
	verifier := auth.NewJWTVerifier(cache, "protrader-identity", "order-gateway", 30*time.Second, func() time.Time { return now })
	principal, err := verifier.Verify(auth.Credentials{Bearer: valid})
	if err != nil {
		t.Fatalf("Verify valid token: %v", err)
	}
	if principal.Account != 7 || principal.Method != auth.MethodBrowser {
		t.Fatalf("principal = %+v, want account 7 and browser method", principal)
	}

	expired, err := mintToken(ed25519.PrivateKey(decoded), "dev-test", 7, -time.Minute, "protrader-identity", "order-gateway", now)
	if err != nil {
		t.Fatalf("mint expired token: %v", err)
	}
	if _, err := verifier.Verify(auth.Credentials{Bearer: expired}); err != auth.ErrBadCredentials {
		t.Fatalf("expired token error = %v, want ErrBadCredentials", err)
	}
}

func TestRunMintAndJWKS(t *testing.T) {
	dir := t.TempDir()
	if err := generateKey("kid", dir); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := run([]string{"jwks", "--key", filepath.Join(dir, "kid.key"), "--kid", "kid"}, &out, io.Discard); err != nil {
		t.Fatalf("run jwks: %v", err)
	}
	if !strings.Contains(out.String(), `"kid": "kid"`) {
		t.Fatalf("JWKS output missing kid: %s", out.String())
	}
	out.Reset()
	if err := run([]string{"mint", "--key", filepath.Join(dir, "kid.key"), "--kid", "kid", "--account", "9"}, &out, io.Discard); err != nil {
		t.Fatalf("run mint: %v", err)
	}
	if out.Len() == 0 {
		t.Fatal("mint produced no token")
	}

}
