package warden

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Every client has a key (0006, 4). keys.yaml holds a key's SHA-256, never
// the key: the file is committed and read by both hosts, and nothing in it
// can be used to get in. The plaintext lives in a sops file only the user can
// open, so a key can be read again.
//
//	keys:
//	  episteme-batch:
//	    sha256: 9f2c...
//	    class: batch
//	    allow: ["reaperboi/*", "zbox/*"]
//
// No allow list means every model. A pattern is matched with path.Match
// against the qualified name, <host>/<model> or <peer>/<model>, so one rule
// reads the same at either host.

// Key is one client's entry in keys.yaml.
type Key struct {
	SHA256 string   `yaml:"sha256" json:"sha256"`
	Class  Class    `yaml:"class" json:"class"`
	Allow  []string `yaml:"allow,omitempty" json:"allow,omitempty"`
}

// KeyFile is keys.yaml.
type KeyFile struct {
	Keys map[string]Key `yaml:"keys" json:"keys"`
}

var (
	keyName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	hexHash = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// HashKey is how a key is stored.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// LoadKeys reads and checks keys.yaml. A file that does not check out is
// refused whole: a key that silently dropped out would lock its client out,
// and one whose class fell back to interactive would never be refused.
func LoadKeys(file string) (KeyFile, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return KeyFile{}, err
	}
	var kf KeyFile
	if err := yaml.Unmarshal(raw, &kf); err != nil {
		return KeyFile{}, fmt.Errorf("%s: %w", file, err)
	}
	if err := kf.Check(); err != nil {
		return KeyFile{}, fmt.Errorf("%s: %w", file, err)
	}
	return kf, nil
}

// Check is LoadKeys' rule, shared with infermux-ui.
func (kf KeyFile) Check() error {
	seen := map[string]string{}
	for name, k := range kf.Keys {
		if !keyName.MatchString(name) {
			return fmt.Errorf("key name %q: letters, digits, '.', '_' and '-' only", name)
		}
		if !hexHash.MatchString(k.SHA256) {
			return fmt.Errorf("key %s: sha256 must be 64 lowercase hex digits", name)
		}
		if other, dup := seen[k.SHA256]; dup {
			return fmt.Errorf("keys %s and %s are the same key", other, name)
		}
		seen[k.SHA256] = name
		if k.Class != Interactive && k.Class != Batch {
			return fmt.Errorf("key %s: class must be interactive or batch, not %q", name, k.Class)
		}
		for _, p := range k.Allow {
			if _, err := path.Match(p, ""); err != nil || p == "" {
				return fmt.Errorf("key %s: allow pattern %q is not a valid pattern", name, p)
			}
		}
	}
	return nil
}

// keyring is the loaded file, by hash.
type keyring struct {
	byHash map[string]namedKey
}

type namedKey struct {
	name string
	Key
}

func newKeyring(kf KeyFile) *keyring {
	k := &keyring{byHash: map[string]namedKey{}}
	for name, key := range kf.Keys {
		k.byHash[key.SHA256] = namedKey{name: name, Key: key}
	}
	return k
}

// identify finds the first presented key that is in the file.
func (k *keyring) identify(r *http.Request) (namedKey, bool) {
	for _, presented := range presentedKeys(r) {
		if presented == "" {
			continue
		}
		if key, ok := k.byHash[HashKey(presented)]; ok {
			return key, true
		}
	}
	return namedKey{}, false
}

// allows is the key's allow list against a qualified model name.
func (k namedKey) allows(model string) bool {
	if k.Allow == nil {
		return true
	}
	for _, p := range k.Allow {
		if ok, _ := path.Match(p, model); ok {
			return true
		}
	}
	return false
}

// keyless are the routes llama-swap serves without a key when it has
// apiKeys: health checks, and what a browser fetches on its own while
// deciding to offer the PWA install, which it cannot attach a key to.
func keyless(r *http.Request) bool {
	if r.Method == http.MethodOptions {
		return true // a CORS preflight never carries credentials
	}
	switch r.URL.Path {
	case "/health", "/wol-health", "/favicon.ico",
		"/ui/site.webmanifest", "/ui/web-app-manifest-192x192.png", "/ui/web-app-manifest-512x512.png":
		return true
	}
	return false
}

// keysPath resolves keys_file against the warden file's directory.
func keysPath(wardenFile, keysFile string) string {
	if keysFile == "" || filepath.IsAbs(keysFile) {
		return keysFile
	}
	return filepath.Join(filepath.Dir(wardenFile), keysFile)
}

// websocketKeyProtocol is how a browser, which cannot set headers on a
// WebSocket, presents a key: OpenAI's realtime clients offer it as a
// subprotocol.
const websocketKeyProtocol = "openai-insecure-api-key."

func websocketKeys(r *http.Request) []string {
	var keys []string
	for _, header := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, p := range strings.Split(header, ",") {
			if k, ok := strings.CutPrefix(strings.TrimSpace(p), websocketKeyProtocol); ok {
				keys = append(keys, k)
			}
		}
	}
	return keys
}

func isWebSocket(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}
