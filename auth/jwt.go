package auth

import (
	"crypto/ed25519"
	"strconv"
	"time"

	"order_gateway/core"

	"github.com/golang-jwt/jwt/v5"
)

type JWTVerifier struct {
	keys     *JWKSCache
	issuer   string
	audience string
	leeway   time.Duration    // allowed clock difference, e.g. 30 * time.Second
	now      func() time.Time // injectable for tests
	parser   *jwt.Parser      // built once in the constructor
}

const maxTokenLifetime = time.Hour

func NewJWTVerifier(keys *JWKSCache, issuer, audience string, leeway time.Duration, now func() time.Time) *JWTVerifier {
	if now == nil {
		now = time.Now
	}
	return &JWTVerifier{
		keys:     keys,
		issuer:   issuer,
		audience: audience,
		leeway:   leeway,
		now:      now,
		parser: jwt.NewParser(
			jwt.WithValidMethods([]string{"EdDSA"}),
			jwt.WithIssuer(issuer),
			jwt.WithAudience(audience),
			jwt.WithExpirationRequired(),
			jwt.WithLeeway(leeway),
			jwt.WithTimeFunc(now),
			jwt.WithIssuedAt(),
		),
	}
}

func (v *JWTVerifier) Ready() bool { return v.keys != nil && v.keys.Ready() }

func (v *JWTVerifier) Verify(c Credentials) (Principal, error) {
	if c.Bearer == "" {
		return Principal{}, ErrNoCredentials
	}
	if len(c.Bearer) > 4096 {
		return Principal{}, ErrBadCredentials
	}
	if !v.Ready() {
		return Principal{}, ErrNotReady
	}

	var claims jwt.RegisteredClaims
	_, err := v.parser.ParseWithClaims(c.Bearer, &claims, func(token *jwt.Token) (any, error) {
		kid, ok := token.Header["kid"].(string)
		if !ok || kid == "" {
			return nil, jwt.ErrTokenMalformed
		}
		key, ok := v.keys.key(kid)
		if !ok {
			return nil, jwt.ErrTokenUnverifiable
		}
		return ed25519.PublicKey(key), nil
	})
	if err != nil || claims.IssuedAt == nil || claims.NotBefore == nil {
		return Principal{}, ErrBadCredentials
	}
	if claims.ExpiresAt.Sub(claims.IssuedAt.Time) > maxTokenLifetime {
		return Principal{}, ErrBadCredentials
	}

	account, err := strconv.ParseUint(claims.Subject, 10, 64)
	if err != nil || account == 0 {
		return Principal{}, ErrBadCredentials
	}
	return Principal{Account: core.AccountID(account), Method: MethodBrowser}, nil
}
