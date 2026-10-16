package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	X   string `json:"x"`
}

func buildJWKS(kid string, public ed25519.PublicKey) ([]byte, error) {
	if len(public) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid Ed25519 public key length %d", len(public))
	}
	return json.MarshalIndent(struct {
		Keys []jwk `json:"keys"`
	}{Keys: []jwk{{
		Kty: "OKP", Crv: "Ed25519", Kid: kid, Use: "sig", Alg: "EdDSA",
		X: base64.RawURLEncoding.EncodeToString(public),
	}}}, "", "  ")
}

func mintToken(private ed25519.PrivateKey, kid string, account uint64, ttl time.Duration, issuer, audience string, now time.Time) (string, error) {
	if len(private) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("invalid Ed25519 private key length %d", len(private))
	}
	if account == 0 {
		return "", fmt.Errorf("account must be non-zero")
	}
	claims := jwt.RegisteredClaims{
		Issuer:    issuer,
		Audience:  jwt.ClaimStrings{audience},
		Subject:   strconv.FormatUint(account, 10),
		ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		NotBefore: jwt.NewNumericDate(now),
		IssuedAt:  jwt.NewNumericDate(now),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	token.Header["kid"] = kid
	return token.SignedString(private)
}

func readPrivate(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	decoded, err := base64.StdEncoding.DecodeString(string(data))
	if err != nil {
		return nil, fmt.Errorf("decode private key: %w", err)
	}
	if len(decoded) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid Ed25519 private key length %d", len(decoded))
	}
	return ed25519.PrivateKey(decoded), nil
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: devtoken <keygen|mint|jwks> [flags]")
	}
	flags := flag.NewFlagSet("devtoken "+args[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	switch args[0] {
	case "keygen":
		kid := flags.String("kid", "", "key id (required)")
		dir := flags.String("out", "", "output directory (required)")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || *kid == "" || *dir == "" {
			return fmt.Errorf("keygen requires --kid and --out")
		}
		return generateKey(*kid, *dir)
	case "mint":
		keyPath := flags.String("key", "", "base64 private-key file (required)")
		kid := flags.String("kid", "", "key id (required)")
		account := flags.Uint64("account", 0, "account id (required)")
		ttl := flags.Duration("ttl", 15*time.Minute, "token lifetime")
		issuer := flags.String("iss", "protrader-identity", "token issuer")
		audience := flags.String("aud", "order-gateway", "token audience")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || *keyPath == "" || *kid == "" || *account == 0 {
			return fmt.Errorf("mint requires --key, --kid, and non-zero --account")
		}
		private, err := readPrivate(*keyPath)
		if err != nil {
			return err
		}
		token, err := mintToken(private, *kid, *account, *ttl, *issuer, *audience, time.Now())
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, token)
		return err
	case "jwks":
		keyPath := flags.String("key", "", "base64 private-key file (required)")
		kid := flags.String("kid", "", "key id (required)")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || *keyPath == "" || *kid == "" {
			return fmt.Errorf("jwks requires --key and --kid")
		}
		private, err := readPrivate(*keyPath)
		if err != nil {
			return err
		}
		data, err := buildJWKS(*kid, private.Public().(ed25519.PublicKey))
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, string(data))
		return err
	default:
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func generateKey(kid, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, kid+".key"), []byte(base64.StdEncoding.EncodeToString(private)), 0o600); err != nil {
		return err
	}
	jwks, err := buildJWKS(kid, public)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "jwks.json"), jwks, 0o600)
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}
