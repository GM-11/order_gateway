package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"log/slog"
	"order_gateway/core"
	"sync/atomic"
	"time"
)

type apiKey struct {
	account  core.AccountID
	hash     [32]byte // SHA-256 of the secret
	disabled bool
}

type keyTable struct {
	version uint64
	byID    map[string]apiKey
}

type APIKeys struct {
	source  KeySource
	current atomic.Pointer[keyTable] // nil until the first successful Refresh
	dummy   [32]byte                 // compared against when the key id is unknown
	log     *slog.Logger
}

func NewAPIKeys(source KeySource, log *slog.Logger) *APIKeys {
	k := &APIKeys{
		source: source,
		log:    log,
	}
	rand.Read(k.dummy[:])
	return k
}
func (k *APIKeys) Refresh(ctx context.Context) error {
	have := uint64(0)
	if t := k.current.Load(); t != nil {
		have = t.version
	}

	snap, changed, err := k.source.Fetch(ctx, have)

	if err != nil {
		return err
	}
	if !changed {
		return nil
	}

	newTable := &keyTable{
		version: snap.Version,
		byID:    make(map[string]apiKey, len(snap.Keys)),
	}

	for _, k := range snap.Keys {
		var hash [32]byte
		decoded, _ := hex.DecodeString(k.SecretSHA256)
		copy(hash[:], decoded)
		newTable.byID[k.KeyID] = apiKey{
			account:  core.AccountID(k.AccountID),
			hash:     hash,
			disabled: k.Disabled,
		}
	}

	k.current.Store(newTable)
	k.log.Info("api keys loaded", "version", snap.Version, "count", len(snap.Keys))

	return nil
}
func (k *APIKeys) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := k.Refresh(ctx); err != nil {
				k.log.Warn("api key refresh failed; keeping previous list", "err", err)
			}
		}
	}
}
func (k *APIKeys) Ready() bool {
	return k.current.Load() != nil
}
func (k *APIKeys) Verify(c Credentials) (Principal, error) {
	if c.KeyID == "" && c.Secret == "" {
		return Principal{}, ErrNoCredentials
	}
	if c.KeyID == "" || c.Secret == "" {
		return Principal{}, ErrBadCredentials
	}

	if len(c.KeyID) > 32 || len(c.Secret) > 128 {
		return Principal{}, ErrBadCredentials
	}

	if !k.Ready() {
		return Principal{}, ErrNotReady
	}

	got := sha256.Sum256([]byte(c.Secret))

	e, ok := k.current.Load().byID[c.KeyID]
	want := e.hash
	if !ok {
		want = k.dummy
	}
	same := subtle.ConstantTimeCompare(got[:], want[:]) == 1

	if !ok || !same || e.disabled {
		return Principal{}, ErrBadCredentials
	}

	return Principal{
		Account: e.account,
		Method:  MethodAPIKey,
		KeyID:   c.KeyID,
	}, nil
}
