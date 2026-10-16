package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testJWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv,omitempty"`
	Kid string `json:"kid,omitempty"`
	Use string `json:"use,omitempty"`
	Alg string `json:"alg,omitempty"`
	X   string `json:"x,omitempty"`
	N   string `json:"n,omitempty"`
}

func generatedJWK(t *testing.T, kid string) (testJWK, ed25519.PublicKey) {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return testJWK{Kty: "OKP", Crv: "Ed25519", Kid: kid, Use: "sig", Alg: "EdDSA", X: base64.RawURLEncoding.EncodeToString(public)}, public
}

func marshalTestJWKS(t *testing.T, keys ...testJWK) []byte {
	t.Helper()
	data, err := json.Marshal(struct {
		Keys []testJWK `json:"keys"`
	}{Keys: keys})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestParseJWKS(t *testing.T) {
	good, publicA := generatedJWK(t, "key-a")
	second, publicB := generatedJWK(t, "key-b")
	rsa := testJWK{Kty: "RSA", Kid: "rsa", Use: "sig", N: "not-an-ed25519-key"}
	encryption := good
	encryption.Kid = "encryption"
	encryption.Use = "enc"
	nonEdDSA := good
	nonEdDSA.Kid = "non-eddsa"
	nonEdDSA.Alg = "RS256"

	tests := []struct {
		name     string
		keys     []testJWK
		mutate   func([]byte) []byte
		wantKids map[string]ed25519.PublicKey
		wantErr  bool
	}{
		{name: "one good key", keys: []testJWK{good}, wantKids: map[string]ed25519.PublicKey{"key-a": publicA}},
		{name: "several keys", keys: []testJWK{good, second}, wantKids: map[string]ed25519.PublicKey{"key-a": publicA, "key-b": publicB}},
		{name: "skips RSA key", keys: []testJWK{good, rsa}, wantKids: map[string]ed25519.PublicKey{"key-a": publicA}},
		{name: "skips encryption key", keys: []testJWK{good, encryption}, wantKids: map[string]ed25519.PublicKey{"key-a": publicA}},
		{name: "skips non-EdDSA key", keys: []testJWK{good, nonEdDSA}, wantKids: map[string]ed25519.PublicKey{"key-a": publicA}},
		{name: "bad x length", keys: []testJWK{{Kty: "OKP", Crv: "Ed25519", Kid: "short", X: base64.RawURLEncoding.EncodeToString([]byte{1})}}, wantErr: true},
		{name: "bad base64", keys: []testJWK{{Kty: "OKP", Crv: "Ed25519", Kid: "bad-base64", X: "%%%"}}, wantErr: true},
		{name: "empty kid", keys: []testJWK{{Kty: "OKP", Crv: "Ed25519", Kid: "", X: good.X}}, wantErr: true},
		{name: "duplicate kid", keys: []testJWK{good, good}, wantErr: true},
		{name: "no usable keys", keys: []testJWK{rsa}, wantErr: true},
		{name: "unknown fields are ignored", keys: []testJWK{good}, mutate: func(data []byte) []byte {
			return []byte(strings.Replace(string(data), `"x":`, `"extra":"allowed","x":`, 1))
		}, wantKids: map[string]ed25519.PublicKey{"key-a": publicA}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := marshalTestJWKS(t, tt.keys...)
			if tt.mutate != nil {
				data = tt.mutate(data)
			}
			got, err := ParseJWKS(data)
			if tt.wantErr {
				if err == nil {
					t.Fatal("ParseJWKS() succeeded, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseJWKS() error: %v", err)
			}
			if len(got) != len(tt.wantKids) {
				t.Fatalf("ParseJWKS() returned %d keys, want %d", len(got), len(tt.wantKids))
			}
			for kid, want := range tt.wantKids {
				if string(got[kid]) != string(want) {
					t.Errorf("key %q differs from generated public key", kid)
				}
			}
		})
	}
}

func TestJWKSCacheRefreshHTTPAndKeepsOldKeysOnError(t *testing.T) {
	jwkA, publicA := generatedJWK(t, "key-a")
	goodBody := marshalTestJWKS(t, jwkA)
	var status atomic.Int32
	status.Store(http.StatusOK)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status.Load() != http.StatusOK {
			http.Error(w, "unavailable", int(status.Load()))
			return
		}
		_, _ = w.Write(goodBody)
	}))
	defer server.Close()

	cache := NewJWKSCache(server.URL, server.Client(), time.Minute, time.Now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if cache.Ready() {
		t.Fatal("Ready() = true before first refresh, want false")
	}
	if err := cache.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh() error: %v", err)
	}
	if !cache.Ready() {
		t.Fatal("Ready() = false after successful refresh")
	}
	if got, ok := cache.key("key-a"); !ok || string(got) != string(publicA) {
		t.Fatalf("key(key-a) = (%v, %v), want generated key and true", got, ok)
	}

	status.Store(http.StatusInternalServerError)
	if err := cache.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh() succeeded with server status 500")
	}
	if got, ok := cache.key("key-a"); !ok || string(got) != string(publicA) {
		t.Fatalf("old key unavailable after failed refresh: ok=%v", ok)
	}
}

func TestJWKSCacheRefreshFileURL(t *testing.T) {
	jwkA, publicA := generatedJWK(t, "key-a")
	path := filepath.Join(t.TempDir(), "jwks.json")
	if err := os.WriteFile(path, marshalTestJWKS(t, jwkA), 0o600); err != nil {
		t.Fatal(err)
	}
	cache := NewJWKSCache((&url.URL{Scheme: "file", Path: path}).String(), http.DefaultClient, time.Minute, time.Now, nil)
	if err := cache.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh() from file URL error: %v", err)
	}
	if got, ok := cache.key("key-a"); !ok || string(got) != string(publicA) {
		t.Fatalf("key(key-a) = (%v, %v), want generated key and true", got, ok)
	}
}

func TestJWKSUnknownKidPokesRefresh(t *testing.T) {
	jwkA, _ := generatedJWK(t, "key-a")
	body := marshalTestJWKS(t, jwkA)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write(body)
	}))
	defer server.Close()

	cache := NewJWKSCache(server.URL, server.Client(), 0, time.Now, nil)
	if err := cache.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var run sync.WaitGroup
	run.Add(1)
	go func() {
		defer run.Done()
		cache.Run(ctx, time.Hour)
	}()
	if _, ok := cache.key("nope"); ok {
		t.Fatal(`key("nope") returned true, want false`)
	}
	waitForRequests(t, &requests, 2)
	cancel()
	run.Wait()
	if got := requests.Load(); got != 2 {
		t.Fatalf("request count = %d, want 2", got)
	}
}

func TestJWKSUnknownKidPokesAreRateLimited(t *testing.T) {
	jwkA, _ := generatedJWK(t, "key-a")
	body := marshalTestJWKS(t, jwkA)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write(body)
	}))
	defer server.Close()

	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	cache := NewJWKSCache(server.URL, server.Client(), time.Minute, now, nil)
	if err := cache.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("initial request count = %d, want 1", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var run sync.WaitGroup
	run.Add(1)
	go func() {
		defer run.Done()
		cache.Run(ctx, time.Hour)
	}()
	defer func() {
		cancel()
		run.Wait()
	}()

	for i := 0; i < 100; i++ {
		if _, ok := cache.key("nope"); ok {
			t.Fatal(`key("nope") returned true, want false`)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests before fake-clock advance = %d, want 1", got)
	}
	clock.Add(int64(2 * time.Minute))
	if _, ok := cache.key("nope"); ok {
		t.Fatal(`key("nope") returned true, want false`)
	}
	waitForRequests(t, &requests, 2)
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests after fake-clock advance = %d, want exactly 2", got)
	}
}

func TestJWKSRefreshRejectsLargeResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", (1<<20)+1))
	}))
	defer server.Close()
	cache := NewJWKSCache(server.URL, server.Client(), time.Minute, time.Now, nil)
	if err := cache.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh() succeeded with response larger than 1 MiB")
	}
}

func waitForRequests(t *testing.T, requests *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for requests.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("request count = %d, want at least %d", requests.Load(), want)
		}
		<-ticker.C
	}
}
