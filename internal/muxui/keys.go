package muxui

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"filippo.io/age"
	"github.com/mostlygeek/llama-swap/internal/warden"
	"gopkg.in/yaml.v3"
)

// The keys (0006, 4). keys.yaml, the warden's keys_file, holds each key's
// SHA-256 and is what the daemons read. The plaintext goes to KeySecrets, a
// sops file the user can open, so a key can be read again; it is the hosts'
// file, beside the secrets their services read. A key is shown when it is
// made and on request, never listed.
//
// The user's age identity is protected by a passphrase, so the UI can open
// the sops file only while a request carries it (0006, 4). The passphrase
// unlocks the identity in this process's memory for that one call to sops,
// and is never stored or passed on a command line.

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

// ErrPassphrase is an operation that has to open the sops file, asked
// without the passphrase.
var ErrPassphrase = errors.New("this needs the passphrase of the age identity")

// CreateKey makes a key and returns it, the one time the UI shows it unasked.
// The plaintext is stored before the hash, so a key the daemons accept
// always has one the user can read. The first key creates the sops file,
// which needs only the recipients; every later one adds to it, which needs
// the passphrase.
func (s *Store) CreateKey(name string, k warden.Key, passphrase string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, kf, err := s.readKeys()
	if err != nil {
		return "", err
	}
	if _, taken := kf.Keys[name]; taken {
		return "", fmt.Errorf("a key named %s already exists", name)
	}
	if taken, err := s.secretNamed(name); err != nil || taken {
		if err == nil {
			err = fmt.Errorf("%s already holds a secret named %s, which a key would overwrite", s.KeySecrets, name)
		}
		return "", err
	}
	random := make([]byte, 32)
	rand.Read(random)
	key := "imx-" + hex.EncodeToString(random)
	k.SHA256 = warden.HashKey(key)
	kf.Keys[name] = k
	if err := kf.Check(); err != nil {
		return "", err
	}
	if err := s.storeSecret(name, key, passphrase); err != nil {
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
// read, then its plaintext. The passphrase is checked before either, so a
// revoke never stops halfway for want of it.
func (s *Store) DeleteKey(name, passphrase string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, kf, err := s.readKeys()
	if err != nil {
		return err
	}
	if _, ok := kf.Keys[name]; !ok {
		return fmt.Errorf("%w: key %s", ErrNotFound, name)
	}
	identity := ""
	if s.secretsExist() {
		if identity, err = s.unlock(passphrase); err != nil {
			return err
		}
	}
	delete(kf.Keys, name)
	if err := writeKeys(path, kf); err != nil {
		return err
	}
	if !s.secretsExist() {
		return nil
	}
	if _, err := s.sops(identity, nil, "unset", s.KeySecrets, index(name)); err != nil && !strings.Contains(err.Error(), "not found") {
		return fmt.Errorf("revoked, but its plaintext is still in %s: %w", s.KeySecrets, err)
	}
	return nil
}

// RevealKey reads a key back out of the sops file.
func (s *Store) RevealKey(name, passphrase string) (string, error) {
	if s.KeySecrets == "" {
		return "", fmt.Errorf("infermux-ui has no -key-secrets file")
	}
	identity, err := s.unlock(passphrase)
	if err != nil {
		return "", err
	}
	out, err := s.sops(identity, nil, "decrypt", "--extract", index(name), s.KeySecrets)
	return strings.TrimSpace(string(out)), err
}

func (s *Store) secretsExist() bool {
	_, err := os.Stat(s.KeySecrets)
	return s.KeySecrets != "" && err == nil
}

// secretNamed is whether the sops file has a top-level entry by that name.
// The file is shared with the hosts' other secrets, such as OpenRouter's
// key, and sops set replaces an entry without a word. sops leaves the names
// in the clear, so this needs no passphrase.
func (s *Store) secretNamed(name string) (bool, error) {
	raw, err := os.ReadFile(s.KeySecrets)
	if s.KeySecrets == "" || errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var entries map[string]any
	if err := yaml.Unmarshal(raw, &entries); err != nil {
		return false, fmt.Errorf("%s: %w", s.KeySecrets, err)
	}
	_, ok := entries[name]
	return ok, nil
}

func (s *Store) storeSecret(name, key, passphrase string) error {
	if s.KeySecrets == "" {
		return fmt.Errorf("infermux-ui has no -key-secrets file, so a new key could never be read again")
	}
	if !s.secretsExist() {
		// A new file takes its recipients from .sops.yaml's rule for its name.
		doc, _ := yaml.Marshal(map[string]string{name: key})
		_, err := s.sops("", doc, "encrypt", "--filename-override", s.KeySecrets,
			"--input-type", "yaml", "--output-type", "yaml", "--output", s.KeySecrets, "/dev/stdin")
		return err
	}
	identity, err := s.unlock(passphrase)
	if err != nil {
		return err
	}
	value, _ := json.Marshal(key)
	_, err = s.sops(identity, value, "set", "--value-stdin", s.KeySecrets, index(name))
	return err
}

// unlock opens the passphrase-protected age identity, as `age -p` wrote it,
// and returns its text for SOPS_AGE_KEY.
func (s *Store) unlock(passphrase string) (string, error) {
	if s.AgeIdentity == "" {
		return "", fmt.Errorf("infermux-ui has no -age-identity, so it cannot open %s", s.KeySecrets)
	}
	if passphrase == "" {
		return "", ErrPassphrase
	}
	f, err := os.Open(s.AgeIdentity)
	if err != nil {
		return "", err
	}
	defer f.Close()
	id, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return "", err
	}
	r, err := age.Decrypt(f, id)
	if err != nil {
		return "", fmt.Errorf("the passphrase does not open %s: %w", s.AgeIdentity, err)
	}
	text, err := io.ReadAll(r)
	return string(text), err
}

// index is sops' path to a top-level entry. A key's name passed Check, so it
// needs no escaping.
func index(name string) string { return `["` + name + `"]` }

// sops runs the sops CLI in the secrets file's directory, where it looks for
// .sops.yaml. The key goes in on stdin, never as an argument.
//
// Its environment holds the unlocked identity and nothing else. Left to
// itself, sops reads ~/.config/sops/age/keys.txt and ~/.ssh/id_ed25519,
// finds them protected, and asks for their passphrases in a window on the
// desktop, which was seen on 2026-10-03. With no HOME, no display and no
// session bus, and no terminal, it can only fail.
func (s *Store) sops(identity string, stdin []byte, args ...string) ([]byte, error) {
	bin := s.Sops
	if bin == "" {
		bin = "sops"
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = filepath.Dir(s.KeySecrets)
	cmd.Env = []string{"HOME=/nonexistent", "XDG_CONFIG_HOME=/nonexistent"}
	if identity != "" {
		cmd.Env = append(cmd.Env, "SOPS_AGE_KEY="+identity)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
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
