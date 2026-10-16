package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
)

var ErrTooLarge = errors.New("auth: response too large")

func fetchURL(ctx context.Context, client *http.Client, rawURL string, max int64) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}

	switch u.Scheme {
	case "file":
		file, err := os.Open(u.Path)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, max+1))
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > max {
			return nil, ErrTooLarge
		}
		return data, nil
	case "http", "https":
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("unexpected HTTP status: %s", resp.Status)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > max {
			return nil, ErrTooLarge
		}
		return data, nil
	default:
		return nil, fmt.Errorf("unsupported URL scheme: %q", u.Scheme)
	}
}
