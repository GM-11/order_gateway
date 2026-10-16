package auth

import (
	"errors"
	"net/http"
	"order_gateway/core"
	"strings"
)

type Credentials struct {
	KeyID  string
	Secret string
	Bearer string
}

type Method uint8

const (
	MethodAPIKey Method = iota + 1
	MethodBrowser
)

type Principal struct {
	Account core.AccountID
	Method  Method
	KeyID   string
}

type Verifier interface {
	Verify(c Credentials) (Principal, error)
}

var (
	ErrNoCredentials  = errors.New("auth: no credentials")
	ErrBadCredentials = errors.New("auth: invalid credentials")
	ErrNotReady       = errors.New("auth: not ready")
)

func FromRequest(r *http.Request) Credentials {
	KeyID := r.Header.Get("API_KEY_ID")
	Secret := r.Header.Get("API_SECRET")
	h := r.Header.Get("Authorization")
	bearer := ""
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		bearer = h[7:]
	}
	return Credentials{KeyID: KeyID, Secret: Secret, Bearer: bearer}
}
