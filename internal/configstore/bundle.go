// Package configstore keeps the one config (0022) as each daemon holds it:
// signed with a dedicated ed25519 key, checked against public keys from Nix
// and never from the config itself, and replaced by a later signed one. Two
// hosts edited apart resolve by the later signed timestamp, ties by host
// name, and the losing version is kept with a warning until the user
// dismisses it (0022, 6).
//
// A bundle is the config's files, a manifest of their SHA-256 with the
// revision, the time and the host that published it, and the manifest's
// signature. Only the manifest is signed; the hashes bind the files to it.
package configstore

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// The two files beside the config's own in a bundle directory.
const (
	ManifestFile  = "manifest.json"
	SignatureFile = "manifest.sig"
)

// MaxClockSkew is how far ahead of this host's clock a signed time may be.
// The later time wins a conflict, so a config stamped a year ahead would
// beat every edit for a year.
const MaxClockSkew = 10 * time.Minute

// Manifest is what the key signs.
type Manifest struct {
	Revision int       `json:"revision"`
	Time     time.Time `json:"time"`
	Host     string    `json:"host"`
	// Parent is the manifest hash this one was edited from; empty for the
	// first. A config whose parent is not the one a host runs was edited
	// apart from it, and replacing it is a conflict.
	Parent string            `json:"parent,omitempty"`
	Files  map[string]string `json:"files"`
}

// Bundle is a signed config as it travels between hosts and lies on disk.
type Bundle struct {
	// Manifest is the signed bytes, kept exactly as signed.
	Manifest  []byte            `json:"manifest"`
	Signature []byte            `json:"signature"`
	Files     map[string][]byte `json:"files"`
}

// Hash names a bundle: the SHA-256 of its manifest.
func (b *Bundle) Hash() string {
	sum := sha256.Sum256(b.Manifest)
	return hex.EncodeToString(sum[:])
}

// Info is the manifest, decoded. Call only on a verified bundle.
func (b *Bundle) Info() Manifest {
	var m Manifest
	_ = json.Unmarshal(b.Manifest, &m)
	return m
}

// Newer orders two manifests the way a conflict is settled: the later
// signed time, then the greater host name.
func Newer(a, b Manifest) bool {
	if !a.Time.Equal(b.Time) {
		return a.Time.After(b.Time)
	}
	return a.Host > b.Host
}

// ParseKey reads a public key: 32 bytes in standard base64, as Nix writes
// it. A line may carry a comment after the key.
func ParseKey(s string) (ed25519.PublicKey, error) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return nil, errors.New("empty key")
	}
	raw, err := base64.StdEncoding.DecodeString(fields[0])
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%q is not an ed25519 public key in base64", fields[0])
	}
	return ed25519.PublicKey(raw), nil
}

// ReadKeys reads a file of public keys, one a line; # starts a comment.
func ReadKeys(file string) ([]ed25519.PublicKey, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var keys []ed25519.PublicKey
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, err := ParseKey(line)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%s: no keys, so no config could be trusted", file)
	}
	return keys, nil
}

// Wire is a bundle as JSON carries it between hosts and from the UI: the
// config's files as text.
type Wire struct {
	Manifest  string            `json:"manifest"`
	Signature string            `json:"signature"`
	Files     map[string]string `json:"files"`
}

func (b *Bundle) Wire() Wire {
	w := Wire{Manifest: string(b.Manifest), Signature: base64.StdEncoding.EncodeToString(b.Signature), Files: map[string]string{}}
	for name, content := range b.Files {
		w.Files[name] = string(content)
	}
	return w
}

func (w Wire) Bundle() (*Bundle, error) {
	sig, err := base64.StdEncoding.DecodeString(w.Signature)
	if err != nil {
		return nil, fmt.Errorf("signature: %w", err)
	}
	b := &Bundle{Manifest: []byte(w.Manifest), Signature: sig, Files: map[string][]byte{}}
	for name, content := range w.Files {
		b.Files[name] = []byte(content)
	}
	return b, nil
}

// Sign makes a bundle of files, edited from parent (nil for the first).
func Sign(files map[string][]byte, key ed25519.PrivateKey, host string, parent *Bundle, now time.Time) (*Bundle, error) {
	m := Manifest{Revision: 1, Time: now.UTC().Truncate(time.Second), Host: host, Files: map[string]string{}}
	if parent != nil {
		m.Revision = parent.Info().Revision + 1
		m.Parent = parent.Hash()
	}
	for name, content := range files {
		if err := checkName(name); err != nil {
			return nil, err
		}
		sum := sha256.Sum256(content)
		m.Files[name] = hex.EncodeToString(sum[:])
	}
	manifest, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	manifest = append(manifest, '\n')
	return &Bundle{Manifest: manifest, Signature: ed25519.Sign(key, manifest), Files: maps.Clone(files)}, nil
}

// Verify checks the signature against the trusted keys, then every file
// against the manifest: none missing, none extra, none changed.
func (b *Bundle) Verify(trusted []ed25519.PublicKey, now time.Time) (Manifest, error) {
	signed := false
	for _, k := range trusted {
		if ed25519.Verify(k, b.Manifest, b.Signature) {
			signed = true
			break
		}
	}
	if !signed {
		return Manifest{}, errors.New("the signature is not from a trusted config key")
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(b.Manifest))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("manifest: %w", err)
	}
	if m.Time.After(now.Add(MaxClockSkew)) {
		return Manifest{}, fmt.Errorf("signed at %s, ahead of this host's clock", m.Time.Format(time.RFC3339))
	}
	if m.Host == "" || m.Revision < 1 {
		return Manifest{}, errors.New("manifest: no host or revision")
	}
	for name := range b.Files {
		if _, ok := m.Files[name]; !ok {
			return Manifest{}, fmt.Errorf("%s is not in the manifest", name)
		}
	}
	for name, want := range m.Files {
		if err := checkName(name); err != nil {
			return Manifest{}, err
		}
		content, ok := b.Files[name]
		if !ok {
			return Manifest{}, fmt.Errorf("%s is missing", name)
		}
		sum := sha256.Sum256(content)
		if hex.EncodeToString(sum[:]) != want {
			return Manifest{}, fmt.Errorf("%s does not match the manifest", name)
		}
	}
	return m, nil
}

// checkName keeps a bundle's files inside its directory: infermux.yaml and
// models/<name>.yaml, nothing else.
func checkName(name string) error {
	if name == "infermux.yaml" {
		return nil
	}
	dir, file := path.Split(name)
	if dir == "models/" && strings.HasSuffix(file, ".yaml") && !strings.HasPrefix(file, ".") && !strings.ContainsAny(file, "/\\") {
		return nil
	}
	return fmt.Errorf("%q is not a config file: infermux.yaml or models/<name>.yaml", name)
}

// ReadBundle reads a bundle directory: the manifest, its signature, and the
// files the manifest names. Files it does not name are not read, so a
// README beside the config stays out of it.
func ReadBundle(dir string) (*Bundle, error) {
	manifest, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		return nil, err
	}
	sig, err := os.ReadFile(filepath.Join(dir, SignatureFile))
	if err != nil {
		return nil, err
	}
	signature, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", SignatureFile, err)
	}
	var m Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", ManifestFile, err)
	}
	b := &Bundle{Manifest: manifest, Signature: signature, Files: map[string][]byte{}}
	for name := range m.Files {
		if err := checkName(name); err != nil {
			return nil, err
		}
		content, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			return nil, err
		}
		b.Files[name] = content
	}
	return b, nil
}

// WriteBundle writes the bundle into dir, which it creates. It removes
// config files the bundle does not have, and leaves anything else, such as
// a README, alone.
func WriteBundle(dir string, b *Bundle) error {
	if err := os.MkdirAll(filepath.Join(dir, "models"), 0o755); err != nil {
		return err
	}
	names := slices.Collect(maps.Keys(b.Files))
	sort.Strings(names)
	for _, name := range names {
		if err := checkName(name); err != nil {
			return err
		}
		if err := writeFile(filepath.Join(dir, filepath.FromSlash(name)), b.Files[name]); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(filepath.Join(dir, "models"))
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := "models/" + e.Name()
		if _, keep := b.Files[name]; !keep && checkName(name) == nil {
			if err := os.Remove(filepath.Join(dir, "models", e.Name())); err != nil {
				return err
			}
		}
	}
	if err := writeFile(filepath.Join(dir, SignatureFile), []byte(base64.StdEncoding.EncodeToString(b.Signature)+"\n")); err != nil {
		return err
	}
	// The manifest last: a reader that finds it finds the files it names.
	return writeFile(filepath.Join(dir, ManifestFile), b.Manifest)
}

func writeFile(name string, content []byte) error {
	tmp := name + ".tmp"
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, name)
}
