package muxui

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/warden"
	"gopkg.in/yaml.v3"
)

// The keys (0006, 4). keys.yaml, the warden's keys_file, holds each key's
// SHA-256 and is what the daemons read. The plaintext goes to KeySecrets, a
// sops file only the user can open, so a key can be read again. A key is
// shown when it is made and on request, never listed.

// KeysState is the keys page: the hashes' file, the sops file, and the keys
// without their plaintext.
type KeysState struct {
	File    string                `json:"file"`
	Secrets string                `json:"secrets"`
	Keys    map[string]warden.Key `json:"keys"`
}

// ErrNoKeysFile is a warden file without keys_file: there is nowhere to put a
// key, and the daemon asks for none.
var ErrNoKeysFile = errors.New("the warden file has no keys_file")

func (s *Store) keysPath() (string, error) {
	cfg, err := s.Warden()
	if err != nil {
		return "", err
	}
	if cfg.KeysPath == "" {
		return "", ErrNoKeysFile
	}
	return cfg.KeysPath, nil
}

// readKeys is keys.yaml, or no keys when it does not exist yet.
func (s *Store) readKeys() (string, warden.KeyFile, error) {
	path, err := s.keysPath()
	if err != nil {
		return "", warden.KeyFile{}, err
	}
	kf, err := warden.LoadKeys(path)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if kf.Keys == nil {
		kf.Keys = map[string]warden.Key{}
	}
	return path, kf, err
}

func (s *Store) Keys() (KeysState, error) {
	path, kf, err := s.readKeys()
	return KeysState{File: path, Secrets: s.KeySecrets, Keys: kf.Keys}, err
}

// CreateKey makes a key and returns it, the one time the UI shows it unasked.
// The plaintext is stored before the hash, so a key the daemons accept
// always has one the user can read.
func (s *Store) CreateKey(name string, k warden.Key) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, kf, err := s.readKeys()
	if err != nil {
		return "", err
	}
	if _, taken := kf.Keys[name]; taken {
		return "", fmt.Errorf("a key named %s already exists", name)
	}
	random := make([]byte, 32)
	rand.Read(random)
	key := "imx-" + hex.EncodeToString(random)
	k.SHA256 = warden.HashKey(key)
	kf.Keys[name] = k
	if err := kf.Check(); err != nil {
		return "", err
	}
	if err := s.storeSecret(name, key); err != nil {
		return "", err
	}
	return key, writeKeys(path, kf)
}

// UpdateKey changes a key's class and allow list. The key stays the same.
func (s *Store) UpdateKey(name string, k warden.Key) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, kf, err := s.readKeys()
	if err != nil {
		return err
	}
	old, ok := kf.Keys[name]
	if !ok {
		return fmt.Errorf("%w: key %s", ErrNotFound, name)
	}
	k.SHA256 = old.SHA256
	kf.Keys[name] = k
	if err := kf.Check(); err != nil {
		return err
	}
	return writeKeys(path, kf)
}

// DeleteKey revokes a key: its hash goes first, which is what the daemons
// read, then its plaintext.
func (s *Store) DeleteKey(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, kf, err := s.readKeys()
	if err != nil {
		return err
	}
	if _, ok := kf.Keys[name]; !ok {
		return fmt.Errorf("%w: key %s", ErrNotFound, name)
	}
	delete(kf.Keys, name)
	if err := writeKeys(path, kf); err != nil {
		return err
	}
	if s.KeySecrets == "" {
		return nil
	}
	if _, err := os.Stat(s.KeySecrets); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if _, err := s.sops(nil, "unset", s.KeySecrets, index(name)); err != nil && !strings.Contains(err.Error(), "not found") {
		return fmt.Errorf("revoked, but its plaintext is still in %s: %w", s.KeySecrets, err)
	}
	return nil
}

// RevealKey reads a key back out of the sops file.
func (s *Store) RevealKey(name string) (string, error) {
	if s.KeySecrets == "" {
		return "", fmt.Errorf("infermux-ui has no -key-secrets file")
	}
	out, err := s.sops(nil, "decrypt", "--extract", index(name), s.KeySecrets)
	return strings.TrimSpace(string(out)), err
}

func (s *Store) storeSecret(name, key string) error {
	if s.KeySecrets == "" {
		return fmt.Errorf("infermux-ui has no -key-secrets file, so a new key could never be read again")
	}
	if _, err := os.Stat(s.KeySecrets); errors.Is(err, os.ErrNotExist) {
		// A new file takes its recipients from .sops.yaml's rule for its name.
		doc, _ := yaml.Marshal(map[string]string{name: key})
		_, err := s.sops(doc, "encrypt", "--filename-override", s.KeySecrets,
			"--input-type", "yaml", "--output-type", "yaml", "--output", s.KeySecrets, "/dev/stdin")
		return err
	}
	value, _ := json.Marshal(key)
	_, err := s.sops(value, "set", "--value-stdin", s.KeySecrets, index(name))
	return err
}

// index is sops' path to a top-level entry. A key's name passed Check, so it
// needs no escaping.
func index(name string) string { return `["` + name + `"]` }

// sops runs the sops CLI in the secrets file's directory, where it looks for
// .sops.yaml. The key goes in on stdin, never as an argument.
func (s *Store) sops(stdin []byte, args ...string) ([]byte, error) {
	bin := s.Sops
	if bin == "" {
		bin = "sops"
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = filepath.Dir(s.KeySecrets)
	cmd.Stdin = bytes.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("sops %s: %s", args[0], strings.TrimSpace(errOut.String()+" "+err.Error()))
	}
	return out.Bytes(), nil
}

// writeKeys writes keys.yaml whole, keeping its leading comment.
func writeKeys(path string, kf warden.KeyFile) error {
	var body yaml.Node
	if err := body.Encode(kf); err != nil {
		return err
	}
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{&body}}
	if raw, err := os.ReadFile(path); err == nil {
		var old yaml.Node
		if yaml.Unmarshal(raw, &old) == nil {
			doc.HeadComment = leadingComment(&old)
		}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return err
	}
	enc.Close()
	return writeAtomic(path, buf.Bytes())
}
