package configstore

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Store is the daemon's copy under its state directory, not in the nixcfg
// checkout: the zbox's nixcfg-pull stops on an uncommitted change, and a
// pushed config would be one (0022, 4).
//
//	versions/<hash>/   each bundle kept, the current one and the last few
//	current            a symlink to the version that runs
//	known.json         the hashes settled: applied, or replaced
//	conflict.json      the last conflict, until the user dismisses it
type Store struct {
	dir     string
	trusted []ed25519.PublicKey
	now     func() time.Time

	mu       sync.Mutex
	current  *Bundle
	info     Manifest
	known    map[string]bool
	conflict *Conflict
}

// Conflict is two versions edited apart: the one kept and the one lost.
type Conflict struct {
	At   time.Time `json:"at"`
	Kept Summary   `json:"kept"`
	Lost Summary   `json:"lost"`
}

// Summary names a version.
type Summary struct {
	Hash     string    `json:"hash"`
	Revision int       `json:"revision"`
	Time     time.Time `json:"time"`
	Host     string    `json:"host"`
}

func summary(b *Bundle, m Manifest) Summary {
	return Summary{Hash: b.Hash(), Revision: m.Revision, Time: m.Time, Host: m.Host}
}

// Result is what an offered bundle did.
type Result string

const (
	Same    Result = "same"    // the one running
	Old     Result = "old"     // settled before, or an older seed
	Applied Result = "applied" // now running
	// Replaced is applied over a version edited apart from it, which is
	// kept as the conflict's loser.
	Replaced Result = "replaced"
	// Lost is an older version edited apart from the one running: kept
	// as the conflict's loser, and not applied.
	Lost Result = "lost"
)

// keepVersions is how many versions stay on disk besides the current one
// and a conflict's loser.
const keepVersions = 20

// Open reads the store in dir, checking the current version against the
// trusted keys again: a rebuild may have changed them, and a version that
// no longer passes does not run (0022, 10).
func Open(dir string, trusted []ed25519.PublicKey) (*Store, error) {
	s := &Store{dir: dir, trusted: trusted, now: time.Now, known: map[string]bool{}}
	if err := os.MkdirAll(filepath.Join(dir, "versions"), 0o755); err != nil {
		return nil, err
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "known.json")); err == nil {
		var list []string
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, fmt.Errorf("known.json: %w", err)
		}
		for _, h := range list {
			s.known[h] = true
		}
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "conflict.json")); err == nil {
		s.conflict = &Conflict{}
		if err := json.Unmarshal(raw, s.conflict); err != nil {
			return nil, fmt.Errorf("conflict.json: %w", err)
		}
	}
	target, err := os.Readlink(filepath.Join(dir, "current"))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	b, err := ReadBundle(filepath.Join(dir, target))
	if err != nil {
		return nil, fmt.Errorf("the current version: %w", err)
	}
	m, err := b.Verify(trusted, s.now())
	if err != nil {
		return s, fmt.Errorf("the current version no longer verifies, so none runs: %w", err)
	}
	s.current, s.info = b, m
	return s, nil
}

// Current is the bundle that runs, nil before the first.
func (s *Store) Current() *Bundle {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

// CurrentInfo is its manifest, with ok false before the first.
func (s *Store) CurrentInfo() (Manifest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info, s.current != nil
}

// Conflict is the last one, nil once dismissed.
func (s *Store) Conflict() *Conflict {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conflict == nil {
		return nil
	}
	c := *s.conflict
	return &c
}

// Dismiss clears the conflict warning. The lost version stays among the
// versions until it is pruned.
func (s *Store) Dismiss() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conflict = nil
	err := os.Remove(filepath.Join(s.dir, "conflict.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Version reads a kept version, for showing a conflict's loser.
func (s *Store) Version(hash string) (*Bundle, error) {
	if len(hash) != 64 {
		return nil, fmt.Errorf("%q is not a version", hash)
	}
	return ReadBundle(filepath.Join(s.dir, "versions", hash))
}

// Offer verifies a bundle and settles it against the one running: the
// later signed time wins (Newer). seed is the nixcfg copy read at start,
// which is often older than what runs; an older seed is ignored rather
// than reported as a conflict.
func (s *Store) Offer(b *Bundle, seed bool) (Result, error) {
	m, err := b.Verify(s.trusted, s.now())
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	hash := b.Hash()
	switch {
	case s.current != nil && hash == s.current.Hash():
		return Same, nil
	case s.known[hash]:
		return Old, nil
	}
	if s.current == nil {
		return Applied, s.apply(b, m)
	}
	cur, curInfo := s.current, s.info
	if Newer(m, curInfo) {
		if err := s.apply(b, m); err != nil {
			return "", err
		}
		if m.Parent == cur.Hash() {
			return Applied, nil
		}
		return Replaced, s.record(Conflict{At: s.now(), Kept: summary(b, m), Lost: summary(cur, curInfo)})
	}
	if seed {
		return Old, nil
	}
	if err := s.keep(b); err != nil {
		return "", err
	}
	s.known[hash] = true
	if err := s.saveKnown(); err != nil {
		return "", err
	}
	return Lost, s.record(Conflict{At: s.now(), Kept: summary(cur, curInfo), Lost: summary(b, m)})
}

// apply makes b current: written in full under versions/, then the
// symlink replaced in one rename.
func (s *Store) apply(b *Bundle, m Manifest) error {
	if err := s.keep(b); err != nil {
		return err
	}
	link := filepath.Join(s.dir, "current")
	tmp := link + ".tmp"
	_ = os.Remove(tmp)
	if err := os.Symlink(filepath.Join("versions", b.Hash()), tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		return err
	}
	if s.current != nil {
		s.known[s.current.Hash()] = true
	}
	s.known[b.Hash()] = true
	if m.Parent != "" {
		// An ancestor of what runs is history, not an edit made apart.
		s.known[m.Parent] = true
	}
	s.current, s.info = b, m
	if err := s.saveKnown(); err != nil {
		return err
	}
	s.prune()
	return nil
}

func (s *Store) keep(b *Bundle) error {
	dir := filepath.Join(s.dir, "versions", b.Hash())
	if _, err := os.Stat(filepath.Join(dir, ManifestFile)); err == nil {
		return nil
	}
	return WriteBundle(dir, b)
}

func (s *Store) record(c Conflict) error {
	s.conflict = &c
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(s.dir, "conflict.json"), raw)
}

func (s *Store) saveKnown() error {
	list := make([]string, 0, len(s.known))
	for h := range s.known {
		list = append(list, h)
	}
	sort.Strings(list)
	raw, err := json.Marshal(list)
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(s.dir, "known.json"), raw)
}

// prune removes the oldest versions beyond keepVersions, never the current
// one or a conflict's.
func (s *Store) prune() {
	entries, err := os.ReadDir(filepath.Join(s.dir, "versions"))
	if err != nil {
		return
	}
	type version struct {
		name string
		mod  time.Time
	}
	var versions []version
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		versions = append(versions, version{e.Name(), info.ModTime()})
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].mod.After(versions[j].mod) })
	keep := map[string]bool{}
	if s.current != nil {
		keep[s.current.Hash()] = true
	}
	if s.conflict != nil {
		keep[s.conflict.Kept.Hash], keep[s.conflict.Lost.Hash] = true, true
	}
	for i, v := range versions {
		if i >= keepVersions && !keep[v.name] {
			os.RemoveAll(filepath.Join(s.dir, "versions", v.name))
		}
	}
}
