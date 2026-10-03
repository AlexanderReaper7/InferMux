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
	if !m.Raw && filepath.IsAbs(m.GGUF) {
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
	return s.apply(files, changed)
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
	for _, f := range files {
		if _, ok := changed[f.name]; ok {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.ModelsDir, f.name))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(tmp, f.name), raw, 0o644); err != nil {
			return err
		}
	}
	for name, raw := range changed {
		if raw == nil {
			continue
		}
		if err := os.WriteFile(filepath.Join(tmp, name), raw, 0o644); err != nil {
			return err
		}
	}
	if _, err := config.LoadConfigSources(s.BaseConfig, tmp); err != nil {
		return fmt.Errorf("llama-swap would refuse this: %w", err)
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
func writeAtomic(path string, raw []byte) error {
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

// SaveWarden writes every setting out, defaults included, keeping the file's
// leading comment.
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

	tmp, err := os.CreateTemp("", "infermux-warden-*.yaml")
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
}

// IntentToAdd marks new model files with `git add -N`, so a flake built
// from the repository sees them; Nix copies only what git tracks. The files'
// content stays unstaged, and Commit adds it as before.
func (s *Store) IntentToAdd() error {
	_, err := s.git("add", "--intent-to-add", "--", s.ModelsDir)
	return err
}

func (s *Store) paths() []string { return []string{s.ModelsDir, s.WardenFile} }

func (s *Store) git(args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", s.ModelsDir}, args...)...)
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
	st := GitState{Repo: strings.TrimSpace(repo), Changes: []string{}}
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
// identity. It never pushes.
func (s *Store) Commit(message string) (string, error) {
	if strings.TrimSpace(message) == "" {
		return "", fmt.Errorf("a commit needs a message")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.git(append([]string{"add", "-A", "--"}, s.paths()...)...); err != nil {
		return "", err
	}
	if _, err := s.git(append([]string{"commit", "-m", message, "--"}, s.paths()...)...); err != nil {
		return "", err
	}
	hash, err := s.git("rev-parse", "--short", "HEAD")
	return strings.TrimSpace(hash), err
}
