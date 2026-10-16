package auth

// Chain is the front door.
// API key credentials (KeyID or Secret) are routed to APIKeys, which decides
// their validity; Bearer is ignored in that case. Otherwise a bearer is routed
// to JWT. If JWT is nil, bearer tokens are rejected with ErrBadCredentials.
// With no credentials, Verify returns ErrNoCredentials. Ready is true only
// when APIKeys is ready and, if configured, JWT is ready.
type Chain struct {
	APIKeys *APIKeys
	JWT     *JWTVerifier
}

func (c Chain) Verify(cr Credentials) (Principal, error) {
	if cr.KeyID != "" || cr.Secret != "" {
		if c.APIKeys == nil {
			return Principal{}, ErrBadCredentials
		}
		return c.APIKeys.Verify(cr)
	}
	if cr.Bearer != "" {
		if c.JWT == nil {
			return Principal{}, ErrBadCredentials
		}
		return c.JWT.Verify(cr)
	}
	return Principal{}, ErrNoCredentials
}

func (c Chain) Ready() bool {
	return c.APIKeys != nil && c.APIKeys.Ready() && (c.JWT == nil || c.JWT.Ready())
}

var _ Verifier = Chain{}
