package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"order_gateway/auth"
)

func TestNewKey(t *testing.T) {
	keyID, secret, entry, err := newKey(7, "test key")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^PK[A-Z2-7]{16}$`).MatchString(keyID) {
		t.Errorf("key id %q does not match expected format", keyID)
	}
	if len(secret) != 43 {
		t.Errorf("secret length = %d, want 43", len(secret))
	}
	if entry.KeyID != keyID || entry.AccountID != 7 || entry.Label != "test key" || entry.Disabled {
		t.Fatalf("entry = %+v, inconsistent with generated key", entry)
	}

	snapshot, err := json.Marshal(auth.KeySnapshot{Version: 1, Keys: []auth.KeyEntry{entry}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "keys.json")
	if err := os.WriteFile(path, snapshot, 0o600); err != nil {
		t.Fatal(err)
	}
	keys := auth.NewAPIKeys(auth.FileKeySource{Path: path}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := keys.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh(): %v", err)
	}
	principal, err := keys.Verify(auth.Credentials{KeyID: keyID, Secret: secret})
	if err != nil {
		t.Fatalf("Verify(): %v", err)
	}
	if uint64(principal.Account) != 7 {
		t.Errorf("account = %d, want 7", principal.Account)
	}

	secondID, secondSecret, _, err := newKey(7, "test key")
	if err != nil {
		t.Fatal(err)
	}
	if secondID == keyID || secondSecret == secret {
		t.Error("two newKey calls returned duplicate credentials")
	}
}
