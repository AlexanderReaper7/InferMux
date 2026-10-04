package muxui

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/warden"
	"gopkg.in/yaml.v3"
)

// Store is the files InferMux reads, as infermux-ui edits them. Nothing is
// written that llama-swap's or the warden's own loader refuses: a bad edit
// fails here, not as a reload the daemon logs and ignores.
type Store struct {
	ModelsDir  string   // llama-swap's -config-dir
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
	// HF downloads a model's files from Hugging Face when it is saved (0012).
	// Nil: a model cannot name a Hugging Face source.
	HF *Downloads

	mu sync.Mutex
}

// ErrNotFound is a model that is not in any file.
var ErrNotFound = errors.New("no such model")

func (s *Store) Models() ([]Model, error) {
	files, err := readModelFiles(s.ModelsDir)
	if err != nil {
		return nil, err
	}
	return listModels(files), nil
}

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

// SaveModel creates a model (original empty) or replaces one, renaming it if
// m.Name differs from original. A new model gets a file of its own.
func (s *Store) SaveModel(original string, m Model) error {
	if err := checkName(m.Name); err != nil {
		return err
	}
	if m.HF != nil && len(m.HF.sources()) == 0 {
		m.HF = nil
	}
	if m.HF != nil {
		if s.HF == nil {
			return fmt.Errorf("this UI has no download directory for Hugging Face files")
		}
		if err := fromHF(&m, s.HF); err != nil {
			return err
		}
	} else if !m.Raw && filepath.IsAbs(m.GGUF) {
		if _, err := os.Stat(m.GGUF); err != nil {
			return fmt.Errorf("the GGUF: %w", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	files, err := readModelFiles(s.ModelsDir)
	if err != nil {
		return err
	}
	byFile := map[string]modelFile{}
	where := map[string]string{} // model name -> file
	for _, f := range files {
		byFile[f.name] = f
		if models := mappingValue(f.root(), "models", false); models != nil {
			for i := 0; i+1 < len(models.Content); i += 2 {
				where[models.Content[i].Value] = f.name
			}
		}
	}
	if m.Name != original {
		if _, taken := where[m.Name]; taken {
			return fmt.Errorf("a model named %s already exists", m.Name)
		}
	}

	changed := map[string][]byte{}
	var node *yaml.Node
	target := m.Name + ".yaml"
	if original == "" {
		if _, exists := byFile[target]; exists {
			return fmt.Errorf("%s already exists", target)
		}
		f := modelFile{name: target, doc: &yaml.Node{}}
		node = &yaml.Node{Kind: yaml.MappingNode}
		f.models().Content = append(f.models().Content, &yaml.Node{Kind: yaml.ScalarNode, Value: m.Name}, node)
		byFile[target] = f
	} else {
		name, ok := where[original]
		if !ok {
			return ErrNotFound
		}
		f := byFile[name]
		models := f.models()
		for i := 0; i+1 < len(models.Content); i += 2 {
			if models.Content[i].Value == original {
				models.Content[i].Value = m.Name
				node = models.Content[i+1]
			}
		}
		target = name
		// A file named after its only model follows the model's name.
		if m.Name != original && name == original+".yaml" && len(models.Content) == 2 {
			if _, exists := byFile[m.Name+".yaml"]; exists {
				return fmt.Errorf("%s.yaml already exists", m.Name)
			}
			changed[name] = nil
			delete(byFile, name)
			target = m.Name + ".yaml"
			f.name = target
			byFile[target] = f
		}
	}
	if err := encodeModel(node, m); err != nil {
		return err
	}
	out, err := byFile[target].bytes()
	if err != nil {
		return err
	}
	changed[target] = out
	if err := s.apply(files, changed); err != nil {
		return err
	}
	if m.HF != nil {
		s.HF.Fetch(m.HF.sources()...)
	}
	return nil
}

func (s *Store) DeleteModel(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	files, err := readModelFiles(s.ModelsDir)
	if err != nil {
		return err
	}
	for _, f := range files {
		models := mappingValue(f.root(), "models", false)
		if models == nil || !deleteKey(models, name) {
			continue
		}
		changed := map[string][]byte{}
		if len(models.Content) == 0 && len(f.root().Content) == 2 {
			changed[f.name] = nil // the file held nothing else
		} else {
			out, err := f.bytes()
			if err != nil {
				return err
			}
			changed[f.name] = out
		}
		return s.apply(files, changed)
	}
	return ErrNotFound
}

// apply validates the models directory as it would be after the change, with
// llama-swap's loader and the Nix base config, then writes it. A nil content
// deletes the file.
func (s *Store) apply(files []modelFile, changed map[string][]byte) error {
	tmp, err := os.MkdirTemp("", "infermux-ui-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	models := filepath.Join(tmp, "models")
	if err := os.Mkdir(models, 0o755); err != nil {
		return err
	}
	for _, f := range files {
		if _, ok := changed[f.name]; ok {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.ModelsDir, f.name))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(models, f.name), withoutDaemonEnv(raw), 0o644); err != nil {
			return err
		}
	}
	for name, raw := range changed {
		if raw == nil {
			continue
		}
		if err := os.WriteFile(filepath.Join(models, name), withoutDaemonEnv(raw), 0o644); err != nil {
			return err
		}
	}
	base, err := os.ReadFile(s.BaseConfig)
	if err != nil {
		return err
	}
	baseCopy := filepath.Join(tmp, "config.yaml")
	if err := os.WriteFile(baseCopy, withoutDaemonEnv(base), 0o644); err != nil {
		return err
	}
	cfg, err := config.LoadConfigSources(baseCopy, models)
	if err != nil {
		return fmt.Errorf("llama-swap would refuse this: %w", err)
	}
	// llama-swap takes a group member no model has, and the model it meant
	// then swaps with everything.
	for id, g := range cfg.Groups {
		for _, member := range g.Members {
			if _, ok := cfg.RealModelName(member); !ok {
				return fmt.Errorf("group %s names %s, which is not a model", id, member)
			}
		}
	}
	names := make([]string, 0, len(changed))
	for name := range changed {
		names = append(names, name)
	}
	sort.Strings(names)
	// Writes before deletions, so a rename never leaves the model in no file.
	for _, name := range names {
		if raw := changed[name]; raw != nil {
			if err := writeAtomic(filepath.Join(s.ModelsDir, name), raw); err != nil {
				return err
			}
		}
	}
	for _, name := range names {
		if changed[name] == nil {
			if err := os.Remove(filepath.Join(s.ModelsDir, name)); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeAtomic replaces path in one rename. The temporary name does not end in
// .yaml, so the daemon's directory watcher never reads a half-written file.
// daemonEnv is llama-swap's ${env.NAME}.
var daemonEnv = regexp.MustCompile(`\$\{env\.[a-zA-Z_][a-zA-Z0-9_]*\}`)

// withoutDaemonEnv fills every ${env.NAME} with a placeholder, for the check
// only. The daemon's environment is not the UI's: a peer's key reaches the
// daemon alone, and the UI never holds it. A name missing from the daemon's
// environment fails at the daemon's reload instead.
func withoutDaemonEnv(raw []byte) []byte {
	return daemonEnv.ReplaceAll(raw, []byte("infermux-ui-placeholder"))
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
	if _, err := warden.LoadConfig(tmp.Name()); err != nil {
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
			if v := mappingValue(old, key.Value, false); v != nil {
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
		an, bn := mappingValue(a, "name", false), mappingValue(b, "name", false)
		return an != nil && bn != nil && an.Value == bn.Value
	}
	return false
}

// named is whether an item can only be matched by its name, which keeps a
// removed consumer's comment from landing on the next one by position.
func named(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode || (n.Kind == yaml.MappingNode && mappingValue(n, "name", false) != nil)
}

// setValueNode replaces a key's value node in place, comments travelling with
// the node rather than being copied the way setValue copies them.
func setValueNode(m *yaml.Node, key string, v *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = v
			return
		}
	}
}

// GGUF is a model file on disk and the models that use it.
type GGUF struct {
	Path   string   `json:"path"`
	Bytes  int64    `json:"bytes"`
	UsedBy []string `json:"used_by"`
}

// GGUFs lists the .gguf files up to three levels under each GGUF directory.
func (s *Store) GGUFs() ([]GGUF, error) {
	models, err := s.Models()
	if err != nil {
		return nil, err
	}
	used := map[string][]string{}
	for _, m := range models {
		if m.Raw {
			continue
		}
		used[m.GGUF] = append(used[m.GGUF], m.Name)
		for _, f := range m.Flags {
			if f.Value != nil && strings.HasSuffix(*f.Value, ".gguf") {
				used[*f.Value] = append(used[*f.Value], m.Name)
			}
		}
	}
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
			by := used[path]
			if by == nil {
				by = []string{}
			}
			out = append(out, GGUF{Path: path, Bytes: info.Size(), UsedBy: by})
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
	_, err := s.git("add", "--intent-to-add", "--", s.ModelsDir)
	return err
}

// paths is what Git shows and Commit commits. The keys' two files count once
// they exist: git refuses a path that matches nothing. A models file that is a
// symlink brings the file it points to, which may sit outside ModelsDir.
func (s *Store) paths() []string {
	paths := []string{s.ModelsDir, s.WardenFile}
	if entries, err := os.ReadDir(s.ModelsDir); err == nil {
		for _, e := range entries {
			if e.Type()&fs.ModeSymlink == 0 {
				continue
			}
			if real, err := filepath.EvalSymlinks(filepath.Join(s.ModelsDir, e.Name())); err == nil {
				paths = append(paths, real)
			}
		}
	}
	keys, _ := s.keysPath()
	for _, p := range []string{keys, s.KeySecrets} {
		if _, err := os.Stat(p); p != "" && err == nil {
			paths = append(paths, p)
		}
	}
	return paths
}

func (s *Store) git(args ...string) (string, error) {
	return s.run(exec.Command("git", append([]string{"-C", s.ModelsDir}, args...)...), args)
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
	cmd := exec.Command("git", append([]string{"-C", s.ModelsDir}, args...)...)
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
