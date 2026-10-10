package configstore

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"filippo.io/age"
)

// The private key lives in nixcfg, age-encrypted with a passphrase (scrypt),
// and is decrypted in memory for one signing: the zbox has no age identity
// of its own (0008), and a passphrase works on any host (0022).

// EncryptKey writes the key's seed, base64, encrypted with the passphrase.
func EncryptKey(key ed25519.PrivateKey, passphrase string) ([]byte, error) {
	r, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	w, err := age.Encrypt(&out, r)
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(w, base64.StdEncoding.EncodeToString(key.Seed())+"\n"); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// ErrPassphrase is a key the passphrase does not open.
var ErrPassphrase = errors.New("the passphrase does not open the config key")

// DecryptKey opens what EncryptKey wrote.
func DecryptKey(encrypted []byte, passphrase string) (ed25519.PrivateKey, error) {
	id, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return nil, err
	}
	r, err := age.Decrypt(bytes.NewReader(encrypted), id)
	if err != nil {
		var wrong *age.NoIdentityMatchError
		if errors.As(err, &wrong) || errors.Is(err, age.ErrIncorrectIdentity) {
			return nil, ErrPassphrase
		}
		return nil, err
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("the config key file does not hold an ed25519 seed")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// PublicKeyString is the key as Nix lists it.
func PublicKeyString(key ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(key)
}
