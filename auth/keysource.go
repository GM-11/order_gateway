package auth

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
)

var ErrSnapshotTooBig = errors.New("snapshot too big")

type KeyEntry struct {
	KeyID        string `json:"key_id"`
	AccountID    uint64 `json:"account_id"`
	SecretSHA256 string `json:"secret_sha256"` // 64 hex chars
	Disabled     bool   `json:"disabled"`
	Label        string `json:"label"`
}

type KeySnapshot struct {
	Version uint64     `json:"version"`
	Keys    []KeyEntry `json:"keys"`
}

type KeySource interface {
	Fetch(ctx context.Context, haveVersion uint64) (snap *KeySnapshot, changed bool, err error)
}

func ParseKeySnapshot(data []byte) (*KeySnapshot, error) {
	var snap KeySnapshot
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	if len(data) > 1<<20 {
		return nil, ErrSnapshotTooBig
	}
	if err := decoder.Decode(&snap); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return nil, ErrSnapshotTooBig
	} else if !errors.Is(err, io.EOF) {
		return nil, err
	}

	if snap.Version < 1 {
		return nil, errors.New("unsupported snapshot version")
	}

	seen := make(map[string]bool)
	for i, key := range snap.Keys {
		if key.KeyID == "" {
			return nil, fmt.Errorf("keys[%d] (%s): key_id must not be empty", i, key.KeyID)
		}
		if key.AccountID == 0 {
			return nil, fmt.Errorf("keys[%d] (%s): account_id must not be 0", i, key.KeyID)
		}
		if len(key.SecretSHA256) != 64 {
			return nil, fmt.Errorf("keys[%d] (%s): secret_sha256 must be 64 hex characters", i, key.KeyID)
		}
		if _, err := hex.DecodeString(key.SecretSHA256); err != nil {
			return nil, fmt.Errorf("keys[%d] (%s): secret_sha256 must be valid hex", i, key.KeyID)
		}
		if seen[key.KeyID] {
			return nil, fmt.Errorf("keys[%d] (%s): duplicate key_id", i, key.KeyID)
		}
		seen[key.KeyID] = true
	}
	return &snap, nil
}

// FileKeySource reads a local JSON file. Bump "version" in the file on every edit.
type FileKeySource struct{ Path string }

func (s FileKeySource) Fetch(ctx context.Context, have uint64) (*KeySnapshot, bool, error) {
	file, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, false, err
	}
	snap, err := ParseKeySnapshot(file)
	if err != nil {
		return nil, false, err
	}
	if snap.Version <= have {
		return nil, false, nil
	}
	return snap, true, nil
}

// HTTPKeySource polls Identity's internal endpoint (contract C).
type HTTPKeySource struct {
	URL    string       // e.g. http://identity:8081/internal/v1/api-keys
	Client *http.Client // with Timeout set, e.g. 5 * time.Second
}

func (s HTTPKeySource) Fetch(ctx context.Context, have uint64) (*KeySnapshot, bool, error) {
	parsedURL, err := url.Parse(s.URL)
	if err != nil {
		return nil, false, err
	}
	query := parsedURL.Query()
	query.Set("if_newer_than", strconv.FormatUint(have, 10))
	parsedURL.RawQuery = query.Encode()
	requestURL := parsedURL.String()

	req, err := http.NewRequestWithContext(ctx, "GET", requestURL, nil)
	if err != nil {
		return nil, false, err
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified { // 304 Not Modified
		return nil, false, nil
	}

	if resp.StatusCode != http.StatusOK { // 200 OK
		return nil, false, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, false, err
	}
	snap, err := ParseKeySnapshot(body)
	if err != nil {
		return nil, false, err
	}
	if snap.Version <= have {
		return nil, false, nil
	}
	return snap, true, nil
}
