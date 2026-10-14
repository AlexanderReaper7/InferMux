package muxui

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/mostlygeek/llama-swap/internal/warden"
	"gopkg.in/yaml.v3"
)

// Store is the files InferMux reads, as infermux-ui edits them. Nothing is
// written that llama-swap's or the warden's own loader refuses: a bad edit
// fails here, not as a reload the daemon logs and ignores.
type Store struct {
	WardenFile string   // the warden's -warden-config
	BaseConfig string   // llama-swap's -config, from Nix: the runtimes' macros and global settings
	GGUFDirs   []string // where to look for model files
	// KVKernels is, per runtime macro, the K-V cache pairs its FlashAttention
	// kernels were compiled for. A runtime missing here is not checked.
	KVKernels map[string][]string
	// KeySecrets is the sops file with each key's plaintext; Sops is the sops
	// binary, "sops" from PATH when empty.
	KeySecrets string
	Sops       string
	// AgeIdentity is the user's age identity, encrypted with a passphrase by
	// `age -p`: what opens KeySecrets.
	AgeIdentity string
	// CommitPassphrase makes Commit take the passphrase of the user's GPG
	// key and sign in gpg's loopback mode, with GPG, "gpg" from PATH when
	// empty: for a host with no session to show a pinentry (0008).
	CommitPassphrase bool
	GPG              string
	// HF is where Add model reads a Hugging Face file's SHA-256; nil asks
	// huggingface.co with no token.
	HF *HuggingFace
	// SignedConfig is nixcfg's copy of the one config, which the daemons
	// start from, and ConfigKey the key that signs it, encrypted with a
	// passphrase (0022).
	SignedConfig string
	ConfigKey    string
	// ConfigPassphrase is an executable that prints ConfigKey's passphrase,
	// such as a lookup in the user's keyring. Publish then takes none from
	// the browser. Empty, the browser sends it.
	ConfigPassphrase string

	mu sync.Mutex
}

// ErrNotFound is a key that is not in the keys file.
var ErrNotFound = errors.New("not found")

// Runtimes are the global macros of the Nix-generated config: the runtimes a
// model's cmd can start with, by name, with what they expand to.
func (s *Store) Runtimes() (map[string]string, error) {
	raw, err := os.ReadFile(s.BaseConfig)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Macros map[string]string `yaml:"macros"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if doc.Macros == nil {
		doc.Macros = map[string]string{}
	}
	return doc.Macros, nil
}

// writeAtomic replaces the file at path, or the file a symlink there points
// to: a file shared between hosts' directories stays one file.
func writeAtomic(path string, raw []byte) error {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Warden reads the warden's file with its defaults filled in.
func (s *Store) Warden() (warden.Config, error) {
	return warden.LoadConfig(s.WardenFile)
}

// TrustedHosts is the warden file's trusted_hosts, read on every request so a
// change applies without a restart. A file that does not load trusts only
// loopback.
func (s *Store) TrustedHosts() []string {
	cfg, err := s.Warden()
	if err != nil {
		return nil
	}
	return cfg.TrustedHosts
}

// SaveWarden writes every setting out, defaults included. It edits the file's
// node tree rather than replacing it, so its comments, its key order and keys
// the UI does not know survive the save (0005).
func (s *Store) SaveWarden(cfg warden.Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var body yaml.Node
	if err := body.Encode(cfg); err != nil {
		return err
	}
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{&body}}
	if raw, err := os.ReadFile(s.WardenFile); err == nil {
		var old yaml.Node
		if yaml.Unmarshal(raw, &old) == nil && len(old.Content) == 1 {
			old.Content[0] = mergeNode(old.Content[0], &body)
			doc = &old
		}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return err
	}
	enc.Close()

	// Beside the file, so a relative keys_file resolves as the warden will.
	tmp, err := os.CreateTemp(filepath.Dir(s.WardenFile), ".infermux-warden-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	tmp.Write(buf.Bytes())
	tmp.Close()
	loaded, err := warden.LoadConfig(tmp.Name())
	if err == nil {
		err = loaded.CheckOneConfig()
	}
	if err != nil {
		return fmt.Errorf("the warden would refuse this: %w", err)
	}
	return writeAtomic(s.WardenFile, buf.Bytes())
}

// mergeNode is next written over old, keeping old's nodes, and with them their
// comments, wherever next has the same thing. A mapping keeps old's key order
// and the keys next does not have, which are the ones the struct does not
// know; keys only next has go at the end. A sequence takes next's items in
// next's order, each matched to an old item by its name, by its value, or by
// position. A scalar keeps its node and takes next's value.
func mergeNode(old, next *yaml.Node) *yaml.Node {
	if old == nil || old.Kind != next.Kind {
		return next
	}
	switch next.Kind {
	case yaml.ScalarNode:
		if old.Value != next.Value {
			old.Value, old.Tag, old.Style = next.Value, next.Tag, next.Style
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(next.Content); i += 2 {
			key, value := next.Content[i], next.Content[i+1]
			if v := mappingValue(old, key.Value); v != nil {
				setValueNode(old, key.Value, mergeNode(v, value))
			} else {
				old.Content = append(old.Content, key, value)
			}
		}
	case yaml.SequenceNode:
		used := make([]bool, len(old.Content))
		items := make([]*yaml.Node, len(next.Content))
		for i, item := range next.Content {
			match := -1
			for j, o := range old.Content {
				if !used[j] && sameItem(o, item) {
					match = j
					break
				}
			}
			if match < 0 && i < len(old.Content) && !used[i] && !named(old.Content[i]) {
				match = i
			}
			if match < 0 {
				items[i] = item
				continue
			}
			used[match] = true
			items[i] = mergeNode(old.Content[match], item)
		}
		old.Content = items
	}
	return old
}

// sameItem is whether two sequence items are the same entry: equal scalars,
// or mappings with the same name.
func sameItem(a, b *yaml.Node) bool {
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case yaml.ScalarNode:
		return a.Value == b.Value
	case yaml.MappingNode:
		an, bn := mappingValue(a, "name"), mappingValue(b, "name")
		return an != nil && bn != nil && an.Value == bn.Value
	}
	return false
}

// named is whether an item can only be matched by its name, which keeps a
// removed consumer's comment from landing on the next one by position.
func named(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode || (n.Kind == yaml.MappingNode && mappingValue(n, "name") != nil)
}

// setValueNode replaces a key's value node in place, comments travelling with
// the node rather than being copied from the old value.
func setValueNode(m *yaml.Node, key string, v *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = v
			return
		}
	}
}

// mappingValue is key's value in the mapping m, nil when it has none.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// GGUF is a model file on disk.
type GGUF struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// GGUFs lists the .gguf files up to three levels under each GGUF directory.
func (s *Store) GGUFs() ([]GGUF, error) {
	out := []GGUF{}
	for _, root := range s.GGUFDirs {
		filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			if d.IsDir() && strings.Count(rel, string(filepath.Separator)) >= 3 {
				return filepath.SkipDir
			}
			if d.IsDir() || !strings.HasSuffix(path, ".gguf") {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			out = append(out, GGUF{Path: path, Bytes: info.Size()})
			return nil
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// --- git --------------------------------------------------------------------

// GitState is what is uncommitted among InferMux's files, and only those: the
// repository may hold anything else, and none of it is the UI's business.
type GitState struct {
	Repo    string   `json:"repo"`
	Branch  string   `json:"branch"`
	Changes []string `json:"changes"` // `git status --porcelain` lines
	Diff    string   `json:"diff"`
	// SignPassphrase is Commit asking for the GPG key's passphrase.
	SignPassphrase bool `json:"sign_passphrase"`
}

// IntentToAdd marks new model files with `git add -N`, so a flake built
// from the repository sees them; Nix copies only what git tracks. The files'
// content stays unstaged, and Commit adds it as before.
func (s *Store) IntentToAdd() error {
	_, err := s.git("add", "--intent-to-add", "--", s.SignedConfig)
	return err
}

// paths is what Git shows and Commit commits. The config's copy and the keys'
// two files count once they exist: git refuses a path that matches nothing.
func (s *Store) paths() []string {
	paths := []string{s.WardenFile}
	keys, _ := s.keysPath()
	for _, p := range []string{s.SignedConfig, keys, s.KeySecrets} {
		if _, err := os.Stat(p); p != "" && err == nil {
			paths = append(paths, p)
		}
	}
	return paths
}

func (s *Store) git(args ...string) (string, error) {
	return s.run(exec.Command("git", append([]string{"-C", s.repoDir()}, args...)...), args)
}

// repoDir is a directory inside the repository the UI edits.
func (s *Store) repoDir() string {
	return filepath.Dir(s.WardenFile)
}

func (s *Store) run(cmd *exec.Cmd, args []string) (string, error) {
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	if err != nil {
		var exit *exec.ExitError
		// git diff --no-index exits 1 when the files differ.
		if !(errors.As(err, &exit) && exit.ExitCode() == 1 && args[0] == "diff") {
			return "", fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(errOut.String()))
		}
	}
	return out.String(), nil
}

func (s *Store) Git() (GitState, error) {
	repo, err := s.git("rev-parse", "--show-toplevel")
	if err != nil {
		return GitState{}, err
	}
	st := GitState{Repo: strings.TrimSpace(repo), Changes: []string{}, SignPassphrase: s.CommitPassphrase}
	if branch, err := s.git("branch", "--show-current"); err == nil {
		st.Branch = strings.TrimSpace(branch)
	}
	status, err := s.git(append([]string{"status", "--porcelain", "--untracked-files=all", "--"}, s.paths()...)...)
	if err != nil {
		return st, err
	}
	var diff strings.Builder
	tracked, err := s.git(append([]string{"diff", "HEAD", "--"}, s.paths()...)...)
	if err != nil {
		return st, err
	}
	diff.WriteString(tracked)
	for _, line := range strings.Split(strings.TrimRight(status, "\n"), "\n") {
		if line == "" {
			continue
		}
		st.Changes = append(st.Changes, line)
		if strings.HasPrefix(line, "??") {
			path := filepath.Join(st.Repo, strings.TrimSpace(line[3:]))
			added, err := s.git("diff", "--no-index", "--", "/dev/null", path)
			if err != nil {
				return st, err
			}
			diff.WriteString(added)
		}
	}
	st.Diff = diff.String()
	return st, nil
}

// Commit commits InferMux's files and nothing else, with the user's own git
// identity. It never pushes. passphrase is the GPG key's, used only with
// CommitPassphrase; whether the commit is signed is the repository's git
// config.
func (s *Store) Commit(message, passphrase string) (string, error) {
	if strings.TrimSpace(message) == "" {
		return "", fmt.Errorf("a commit needs a message")
	}
	if s.CommitPassphrase && passphrase == "" {
		return "", ErrGPGPassphrase
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.git(append([]string{"add", "-A", "--"}, s.paths()...)...); err != nil {
		return "", err
	}
	args := append([]string{"commit", "-m", message, "--"}, s.paths()...)
	cmd := exec.Command("git", append([]string{"-C", s.repoDir()}, args...)...)
	if s.CommitPassphrase {
		done, err := s.signWith(cmd, passphrase)
		if err != nil {
			return "", err
		}
		defer done()
	}
	if _, err := s.run(cmd, args); err != nil {
		return "", err
	}
	hash, err := s.git("rev-parse", "--short", "HEAD")
	return strings.TrimSpace(hash), err
}
