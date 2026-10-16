package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"order_gateway/auth"
)

func newKey(account uint64, label string) (keyID, secret string, entry auth.KeyEntry, err error) {
	if account == 0 {
		return "", "", auth.KeyEntry{}, fmt.Errorf("account must be non-zero")
	}
	var idBytes [10]byte
	if _, err := io.ReadFull(rand.Reader, idBytes[:]); err != nil {
		return "", "", auth.KeyEntry{}, err
	}
	var secretBytes [32]byte
	if _, err := io.ReadFull(rand.Reader, secretBytes[:]); err != nil {
		return "", "", auth.KeyEntry{}, err
	}
	keyID = "PK" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(idBytes[:])
	secret = base64.RawURLEncoding.EncodeToString(secretBytes[:])
	hash := sha256.Sum256([]byte(secret))
	entry = auth.KeyEntry{
		KeyID:        keyID,
		AccountID:    account,
		SecretSHA256: hex.EncodeToString(hash[:]),
		Label:        label,
	}
	return keyID, secret, entry, nil
}

func run(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("newkey", flag.ContinueOnError)
	flags.SetOutput(stderr)
	account := flags.Uint64("account", 0, "account id (required)")
	label := flags.String("label", "", "human-readable key label")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *account == 0 {
		return fmt.Errorf("--account must be provided and non-zero")
	}
	keyID, secret, entry, err := newKey(*account, *label)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(stdout, "WARNING: the secret is shown once; store it securely."); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "key_id: %s\nsecret: %s\n%s\n", keyID, secret, encoded); err != nil {
		return err
	}
	return nil
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}
