package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"order_gateway/core"
)

type fakeKeySource struct {
	mu       sync.Mutex
	snapshot *KeySnapshot
	changed  bool
	err      error
}

func (s *fakeKeySource) Fetch(context.Context, uint64) (*KeySnapshot, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot, s.changed, s.err
}

func (s *fakeKeySource) set(snapshot *KeySnapshot, err error) {
	s.mu.Lock()
	s.snapshot = snapshot
	s.changed = snapshot != nil
	s.err = err
	s.mu.Unlock()
}

func testKey(keyID, secret string, account uint64, disabled bool) KeyEntry {
	hash := sha256.Sum256([]byte(secret))
	return KeyEntry{KeyID: keyID, AccountID: account, SecretSHA256: hex.EncodeToString(hash[:]), Disabled: disabled}
}

func testAPIKeys(source KeySource) *APIKeys {
	return NewAPIKeys(source, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestAPIKeysVerify(t *testing.T) {
	ctx := context.Background()
	source := &fakeKeySource{}
	keys := testAPIKeys(source)
	if keys.Ready() {
		t.Fatal("Ready() = true before Refresh, want false")
	}
	if _, err := keys.Verify(Credentials{KeyID: "key-a", Secret: "secret-a"}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("Verify before Refresh error = %v, want ErrNotReady", err)
	}

	source.set(&KeySnapshot{Version: 1, Keys: []KeyEntry{
		testKey("key-a", "secret-a", 7, false),
		testKey("key-disabled", "secret-disabled", 8, true),
	}}, nil)
	if err := keys.Refresh(ctx); err != nil {
		t.Fatalf("Refresh() error: %v", err)
	}
	if !keys.Ready() {
		t.Fatal("Ready() = false after successful Refresh, want true")
	}

	tests := []struct {
		name    string
		cred    Credentials
		want    Principal
		wantErr error
	}{
		{name: "correct key", cred: Credentials{KeyID: "key-a", Secret: "secret-a"}, want: Principal{Account: core.AccountID(7), Method: MethodAPIKey, KeyID: "key-a"}},
		{name: "wrong secret", cred: Credentials{KeyID: "key-a", Secret: "wrong"}, wantErr: ErrBadCredentials},
		{name: "unknown key id", cred: Credentials{KeyID: "missing", Secret: "secret-a"}, wantErr: ErrBadCredentials},
		{name: "disabled key", cred: Credentials{KeyID: "key-disabled", Secret: "secret-disabled"}, wantErr: ErrBadCredentials},
		{name: "empty credentials", wantErr: ErrNoCredentials},
		{name: "key id only", cred: Credentials{KeyID: "key-a"}, wantErr: ErrBadCredentials},
		{name: "secret only", cred: Credentials{Secret: "secret-a"}, wantErr: ErrBadCredentials},
		{name: "oversized secret", cred: Credentials{KeyID: "key-a", Secret: string(make([]byte, 10000))}, wantErr: ErrBadCredentials},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := keys.Verify(tt.cred)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Verify() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Verify() error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("Verify() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestAPIKeysRefreshSwapsSnapshot(t *testing.T) {
	source := &fakeKeySource{}
	keys := testAPIKeys(source)
	source.set(&KeySnapshot{Version: 1, Keys: []KeyEntry{testKey("key-a", "secret-a", 7, false)}}, nil)
	if err := keys.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	cred := Credentials{KeyID: "key-a", Secret: "secret-a"}
	if _, err := keys.Verify(cred); err != nil {
		t.Fatalf("key A before swap: %v", err)
	}

	source.set(&KeySnapshot{Version: 2, Keys: []KeyEntry{testKey("key-a", "secret-a", 7, true)}}, nil)
	if err := keys.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Verify(cred); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("key A after disable error = %v, want ErrBadCredentials", err)
	}
}

func TestAPIKeysFailedRefreshKeepsOldSnapshot(t *testing.T) {
	source := &fakeKeySource{}
	keys := testAPIKeys(source)
	source.set(&KeySnapshot{Version: 1, Keys: []KeyEntry{testKey("key-a", "secret-a", 7, false)}}, nil)
	if err := keys.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	source.set(nil, errors.New("source unavailable"))
	if err := keys.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh() succeeded despite source error")
	}
	if _, err := keys.Verify(Credentials{KeyID: "key-a", Secret: "secret-a"}); err != nil {
		t.Fatalf("old key stopped working after failed refresh: %v", err)
	}
}

func TestAPIKeysConcurrentVerifyAndRefresh(t *testing.T) {
	source := &fakeKeySource{}
	keys := testAPIKeys(source)
	makeSnapshot := func(version uint64) *KeySnapshot {
		return &KeySnapshot{Version: version, Keys: []KeyEntry{testKey("key-a", "secret-a", 7, false)}}
	}
	source.set(makeSnapshot(1), nil)
	if err := keys.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	const workers = 50
	var wg sync.WaitGroup
	start := make(chan struct{})
	cred := Credentials{KeyID: "key-a", Secret: "secret-a"}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 200; j++ {
				if _, err := keys.Verify(cred); err != nil {
					t.Errorf("Verify() error: %v", err)
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for version := uint64(2); version < 52; version++ {
			source.set(makeSnapshot(version), nil)
			if err := keys.Refresh(context.Background()); err != nil {
				t.Errorf("Refresh() error: %v", err)
				return
			}
		}
	}()
	close(start)
	wg.Wait()
}
