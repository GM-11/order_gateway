package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"order_gateway/auth"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadConfigDefaults(t *testing.T) {
	c, err := loadConfig(env(map[string]string{
		"GATEWAY_ID":      "gw-1",
		"API_KEYS_SOURCE": "keys.json",
	}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if c.HTTPAddr != "127.0.0.1:8080" || c.EngineFeedAddr != "127.0.0.1:50061" ||
		c.CashOwnerAddr != "127.0.0.1:50071" || c.OrderTimeout != 5*time.Second {
		t.Errorf("unexpected defaults: %+v", c)
	}
}

func TestLoadConfigReportsEveryProblemAtOnce(t *testing.T) {
	_, err := loadConfig(env(map[string]string{
		"JWKS_URL":      "file:///tmp/jwks.json",
		"ORDER_TIMEOUT": "-1s",
	}))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"GATEWAY_ID", "API_KEYS_SOURCE", "JWT_ISSUER", "ORDER_TIMEOUT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

func TestLoadConfigRejectsBadTimeout(t *testing.T) {
	for _, raw := range []string{"5", "abc", "0s"} {
		_, err := loadConfig(env(map[string]string{
			"GATEWAY_ID": "gw-1", "API_KEYS_SOURCE": "keys.json", "ORDER_TIMEOUT": raw,
		}))
		if err == nil || !strings.Contains(err.Error(), "ORDER_TIMEOUT") {
			t.Errorf("ORDER_TIMEOUT=%q: got %v, want an ORDER_TIMEOUT error", raw, err)
		}
	}
}

func TestKeySourcePicksByScheme(t *testing.T) {
	if _, ok := keySource("http://identity:8081/keys").(auth.HTTPKeySource); !ok {
		t.Error("http URL should use HTTPKeySource")
	}
	if _, ok := keySource("https://identity/keys").(auth.HTTPKeySource); !ok {
		t.Error("https URL should use HTTPKeySource")
	}
	if _, ok := keySource("./keys.json").(auth.FileKeySource); !ok {
		t.Error("a path should use FileKeySource")
	}
}

func TestReadyHandler(t *testing.T) {
	up := func() bool { return true }
	down := func() bool { return false }

	w := httptest.NewRecorder()
	readyHandler(map[string]func() bool{"auth": up, "engine_feed": up})(w, httptest.NewRequest("GET", "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Errorf("all up: got %d, want 200", w.Code)
	}

	w = httptest.NewRecorder()
	readyHandler(map[string]func() bool{"auth": down, "engine_feed": down, "x": up})(w, httptest.NewRequest("GET", "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("some down: got %d, want 503", w.Code)
	}
	if got := w.Body.String(); !strings.Contains(got, "auth, engine_feed") {
		t.Errorf("body = %q, want the failing checks named in order", got)
	}
}
