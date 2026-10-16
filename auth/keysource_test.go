package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validSnapshot = `{"version":1,"keys":[{"key_id":"key-a","account_id":7,"secret_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","disabled":false,"label":"test"}]}`

func TestParseKeySnapshot(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "valid", input: validSnapshot},
		{name: "unknown field", input: `{"version":1,"keys":[],"extra":true}`, wantErr: true},
		{name: "trailing data", input: validSnapshot + ` {}`, wantErr: true},
		{name: "zero version", input: `{"version":0,"keys":[]}`, wantErr: true},
		{name: "bad key id", input: `{"version":1,"keys":[{"key_id":"","account_id":7,"secret_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`, wantErr: true},
		{name: "zero account", input: `{"version":1,"keys":[{"key_id":"key-a","account_id":0,"secret_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`, wantErr: true},
		{name: "short hash", input: `{"version":1,"keys":[{"key_id":"key-a","account_id":7,"secret_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`, wantErr: true},
		{name: "non-hex hash", input: `{"version":1,"keys":[{"key_id":"key-a","account_id":7,"secret_sha256":"gggggggggggggggggggggggggggggggggggggggggggggggggggggggggggggggg"}]}`, wantErr: true},
		{name: "duplicate key id", input: `{"version":1,"keys":[{"key_id":"key-a","account_id":7,"secret_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},{"key_id":"key-a","account_id":8,"secret_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseKeySnapshot([]byte(tt.input))
			if tt.wantErr && err == nil {
				t.Fatal("ParseKeySnapshot() succeeded, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ParseKeySnapshot() error: %v", err)
			}
		})
	}
}

func TestFileKeySourceVersions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	if err := os.WriteFile(path, []byte(validSnapshot), 0o600); err != nil {
		t.Fatal(err)
	}
	source := FileKeySource{Path: path}
	if snap, changed, err := source.Fetch(context.Background(), 1); err != nil || changed || snap != nil {
		t.Fatalf("same version Fetch() = (%v, %v, %v), want (nil, false, nil)", snap, changed, err)
	}
	if snap, changed, err := source.Fetch(context.Background(), 0); err != nil || !changed || snap == nil || snap.Version != 1 {
		t.Fatalf("higher version Fetch() = (%v, %v, %v), want version 1 and changed", snap, changed, err)
	}
}

func TestHTTPKeySource(t *testing.T) {
	const newer = `{"version":2,"keys":[]}`
	const older = `{"version":4,"keys":[]}`
	tests := []struct {
		name        string
		status      int
		body        string
		have        uint64
		wantChanged bool
		wantErr     bool
	}{
		{name: "not modified", status: http.StatusNotModified, have: 1},
		{name: "new version", status: http.StatusOK, body: newer, have: 1, wantChanged: true},
		{name: "server error", status: http.StatusInternalServerError, wantErr: true},
		{name: "huge body", status: http.StatusOK, body: strings.Repeat("x", (1<<20)+1), wantErr: true},
		{name: "older version", status: http.StatusOK, body: older, have: 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotHave string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotHave = r.URL.Query().Get("if_newer_than")
				w.WriteHeader(tt.status)
				if tt.body != "" {
					_, _ = w.Write([]byte(tt.body))
				}
			}))
			defer server.Close()

			source := HTTPKeySource{URL: server.URL, Client: server.Client()}
			snap, changed, err := source.Fetch(context.Background(), tt.have)
			if gotHave != fmt.Sprint(tt.have) {
				t.Errorf("if_newer_than query = %q, want %q", gotHave, fmt.Sprint(tt.have))
			}
			if tt.wantErr {
				if err == nil {
					t.Fatal("Fetch() succeeded, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Fetch() error: %v", err)
			}
			if changed != tt.wantChanged {
				t.Errorf("Fetch() changed = %v, want %v", changed, tt.wantChanged)
			}
			if tt.wantChanged && (snap == nil || snap.Version != 2) {
				t.Errorf("Fetch() snapshot = %+v, want version 2", snap)
			}
			if !tt.wantChanged && snap != nil {
				t.Errorf("Fetch() snapshot = %+v, want nil", snap)
			}
		})
	}
}

func TestHTTPKeySourceQueryEscapesHave(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := url.ParseQuery(r.URL.RawQuery); err != nil {
			t.Errorf("invalid query: %v", err)
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()
	if _, _, err := (HTTPKeySource{URL: server.URL, Client: server.Client()}).Fetch(context.Background(), 123); err != nil {
		t.Fatal(err)
	}
}
