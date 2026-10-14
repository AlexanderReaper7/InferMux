package muxui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/configstore"
	"github.com/mostlygeek/llama-swap/internal/placement"
)

// The one config's editor (0022, 9). The draft lives in the browser. This
// side checks it with the loader the daemons run, signs it with the config
// key that the passphrase opens, from the browser or from ConfigPassphrase,
// hands it to the daemon, which sends it to the other hosts, and writes the
// signed copy to nixcfg, where Commit records it. The UI never pushes (0005).

// daemonClient reaches this host's daemon as the UI does through /daemon/:
// with X-InferMux and the UI's own key.
type daemonClient struct {
	url    *url.URL
	key    string
	client *http.Client
}

// call returns the daemon's status and body; err is only a daemon that
// could not be reached.
func (d daemonClient) call(method, path string, body any) (int, []byte, error) {
	var in io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		in = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, d.url.JoinPath(path).String(), in)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("X-InferMux", "infermux-ui")
	if d.key != "" {
		req.Header.Set("Authorization", "Bearer "+d.key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := d.client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("the daemon: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	return resp.StatusCode, raw, err
}

// bundle is a version the daemon keeps, nil when it has none by that name.
func (d daemonClient) bundle(path string) (*configstore.Bundle, error) {
	status, raw, err := d.call(http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, nil
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("the daemon's %s: %d %s", path, status, raw)
	}
	var w configstore.Wire
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, err
	}
	return w.Bundle()
}

// Problem is where the loader stopped: the file and line when its message
// names them.
type Problem struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Message string `json:"message"`
}

// Check is a draft as the daemons would run it: the first error, or the
// warnings and every model's places.
type Check struct {
	Error    *Problem                     `json:"error"`
	Warnings []string                     `json:"warnings"`
	Routes   map[string][]placement.Route `json:"routes"`
	// Devices are each host's, by name, with their kind: the placement
	// table's columns.
	Devices map[string]map[string]string `json:"devices"`
}

var linePattern = regexp.MustCompile(`\bline (\d+)\b`)

func problem(files map[string][]byte, err error) *Problem {
	p := &Problem{Message: err.Error()}
	if name, rest, ok := strings.Cut(p.Message, ": "); ok {
		if _, known := files[name]; known {
			p.File, p.Message = name, rest
		}
	}
	if m := linePattern.FindStringSubmatch(p.Message); m != nil && p.File != "" {
		p.Line, _ = strconv.Atoi(m[1])
	}
	return p
}

// check parses files and derives every host's part, as Publish and the
// daemons do; a config one host cannot run is refused (0022, the daemon).
func check(files map[string][]byte) (*placement.Config, Check) {
	res := Check{Warnings: []string{}, Routes: map[string][]placement.Route{}, Devices: map[string]map[string]string{}}
	cfg, warnings, err := placement.Parse(files)
	if err == nil {
		_, err = cfg.DeriveAll()
	}
	if err != nil {
		res.Error = problem(files, err)
		return nil, res
	}
	res.Warnings = append(res.Warnings, warnings...)
	// A host the config does not list routes every model and runs none.
	part, err := cfg.Derive("")
	if err != nil {
		res.Error = problem(files, err)
		return nil, res
	}
	res.Routes = part.Routes
	for name, h := range cfg.Hosts {
		res.Devices[name] = map[string]string{}
		for d, dev := range h.Devices {
			res.Devices[name][d] = dev.Kind
		}
	}
	return cfg, res
}

// checkHere is check, and this host's part loaded by llama-swap with the
// base config, as the daemon loads it before it keeps a config: a macro this
// host's Nix does not define fails here (0022, 11). Each other host's
// daemon loads its own part. The UI has not the daemon's environment, which
// holds the clouds' keys, so what reads it is left to the daemon (LoadsWith).
func (s *Store) checkHere(files map[string][]byte) Check {
	cfg, res := check(files)
	if res.Error != nil {
		return res
	}
	host, err := s.host()
	if err == nil {
		var part *placement.Part
		if part, err = cfg.Derive(host); err == nil {
			err = part.LoadsWith(s.BaseConfig, false)
		}
	}
	if err != nil {
		res.Error = &Problem{Message: fmt.Sprintf("%s: llama-swap: %v", host, err)}
	}
	return res
}

func toBytes(files map[string]string) map[string][]byte {
	out := make(map[string][]byte, len(files))
	for name, content := range files {
		out[name] = []byte(content)
	}
	return out
}

// copySummary is nixcfg's copy, nil when there is none.
func (s *Store) copySummary() (*configstore.Summary, error) {
	b, err := configstore.ReadBundle(s.SignedConfig)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m := b.Info()
	return &configstore.Summary{Hash: b.Hash(), Revision: m.Revision, Time: m.Time, Host: m.Host}, nil
}

// hashFile is a GGUF's SHA-256 for its files: entry. Only a file under the
// GGUF directories, which the add-model command lists.
func (s *Store) hashFile(path string) (string, int64, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", 0, fmt.Errorf("%q is not a clean absolute path", path)
	}
	under := false
	for _, dir := range s.GGUFDirs {
		if rel, err := filepath.Rel(dir, path); err == nil && !strings.HasPrefix(rel, "..") && rel != "." {
			under = true
		}
	}
	if !under {
		return "", 0, fmt.Errorf("%s is not under the GGUF directories: %s", path, strings.Join(s.GGUFDirs, ", "))
	}
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// hfFile is a Hugging Face file's SHA-256 and size, from its LFS ETag.
func (s *Store) hfFile(source string) (string, int64, error) {
	if err := checkHF(source); err != nil {
		return "", 0, err
	}
	d := s.HF
	if d == nil {
		d = &HuggingFace{}
	}
	tag, size, err := d.etag(source)
	if err != nil {
		return "", 0, err
	}
	if !sha256Hex.MatchString(tag) {
		return "", 0, fmt.Errorf("%s is not an LFS file, so Hugging Face gives no SHA-256 for it; download it and add it from this host", source)
	}
	return tag, size, nil
}

// configPassphrase runs ConfigPassphrase. A locked keyring asks to be
// unlocked first, so it may take as long as the user does.
func (s *Store) configPassphrase() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, s.ConfigPassphrase)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("the config key's passphrase from %s: %w %s", s.ConfigPassphrase, err, strings.TrimSpace(stderr.String()))
	}
	pass := strings.TrimRight(string(out), "\r\n")
	if pass == "" {
		return "", fmt.Errorf("%s printed no passphrase for the config key", s.ConfigPassphrase)
	}
	return pass, nil
}

type publishRequest struct {
	Files map[string]string `json:"files"`
	// Base is the hash of the version the draft was edited from, "" for the
	// first config.
	Base       string `json:"base"`
	Passphrase string `json:"passphrase"`
	// Force publishes over a version the draft did not start from. The two
	// then conflict, and the later one runs with the warning (0022, 6).
	Force bool `json:"force"`
}

// errStale is a draft begun from a version that is no longer the one
// running.
var errStale = errors.New("stale")

// publish signs the draft and hands it to the daemon, then writes the copy.
func (s *Store) publish(d daemonClient, req publishRequest, now time.Time) (map[string]any, *Problem, error) {
	files := toBytes(req.Files)
	if res := s.checkHere(files); res.Error != nil {
		return nil, res.Error, nil
	}
	current, err := d.bundle("/warden/config")
	if err != nil {
		return nil, nil, err
	}
	var parent *configstore.Bundle
	switch {
	case current != nil && current.Hash() == req.Base:
		parent = current
	case !req.Force && (current != nil || req.Base != ""):
		return nil, nil, errStale
	case req.Base != "":
		if parent, err = d.bundle("/warden/config/version/" + url.PathEscape(req.Base)); err != nil {
			return nil, nil, err
		}
		if parent == nil {
			return nil, nil, fmt.Errorf("the draft's base %.12s is no longer kept; start the draft again from the running version", req.Base)
		}
	}
	host, err := s.host()
	if err != nil {
		return nil, nil, err
	}
	encrypted, err := os.ReadFile(s.ConfigKey)
	if err != nil {
		return nil, nil, fmt.Errorf("the config key: %w", err)
	}
	pass := req.Passphrase
	if pass == "" && s.ConfigPassphrase != "" {
		if pass, err = s.configPassphrase(); err != nil {
			return nil, nil, err
		}
	}
	key, err := configstore.DecryptKey(encrypted, pass)
	if err != nil {
		return nil, nil, fmt.Errorf("the passphrase does not open the config key: %w", err)
	}
	b, err := configstore.Sign(files, key, host, parent, now)
	clear(key)
	if err != nil {
		return nil, nil, err
	}
	status, raw, err := d.call(http.MethodPost, "/warden/config", b.Wire())
	if err != nil {
		return nil, nil, err
	}
	if status != http.StatusOK {
		var detail struct{ Detail string }
		json.Unmarshal(raw, &detail)
		return nil, nil, fmt.Errorf("the daemon refused it: %s", detail.Detail)
	}
	var accepted map[string]any
	if err := json.Unmarshal(raw, &accepted); err != nil {
		return nil, nil, err
	}
	// nixcfg's copy is the version the daemons run: not one kept only as a
	// conflict's loser, nor one older than the running one.
	switch configstore.Result(fmt.Sprint(accepted["result"])) {
	case configstore.Applied, configstore.Replaced, configstore.Same:
	default:
		return nil, nil, fmt.Errorf("the daemon kept it but does not run it (%v); nixcfg's copy is unchanged", accepted["result"])
	}
	s.mu.Lock()
	err = configstore.WriteBundle(s.SignedConfig, b)
	s.mu.Unlock()
	if err != nil {
		return nil, nil, fmt.Errorf("the daemon runs it, but nixcfg's copy was not written: %w", err)
	}
	m := b.Info()
	accepted["published"] = configstore.Summary{Hash: b.Hash(), Revision: m.Revision, Time: m.Time, Host: m.Host}
	return accepted, nil, nil
}

// host is the name a Publish is signed as: warden.yaml's host.
func (s *Store) host() (string, error) {
	cfg, err := s.Warden()
	if err != nil {
		return "", err
	}
	if cfg.Host == "" {
		return "", errors.New("warden.yaml names no host, which the one config needs")
	}
	return cfg.Host, nil
}

// configRoutes are /api/config/*, the one config's editor (0022, 9).
func configRoutes(mux *http.ServeMux, store *Store, build *Builder, d daemonClient) {
	mux.HandleFunc("GET /api/config/schema", func(rw http.ResponseWriter, r *http.Request) {
		reply(rw, map[string]any{"main": placement.Schema(), "model": placement.ModelSchema()})
	})
	mux.HandleFunc("POST /api/config/check", func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Files map[string]string `json:"files"`
		}
		if !decode(rw, r, &body) {
			return
		}
		res := store.checkHere(toBytes(body.Files))
		reply(rw, res)
	})
	mux.HandleFunc("POST /api/config/publish", func(rw http.ResponseWriter, r *http.Request) {
		var body publishRequest
		if !decode(rw, r, &body) {
			return
		}
		res, p, err := store.publish(d, body, time.Now())
		switch {
		case p != nil:
			writeJSON(rw, http.StatusUnprocessableEntity, map[string]any{"detail": p.Message, "problem": p})
		case errors.Is(err, errStale):
			writeJSON(rw, http.StatusConflict, map[string]string{"detail": "the running config is not the one this draft began from: another Publish came first. Publishing anyway makes the two conflict, and this one runs with the warning."})
		case err != nil:
			fail(rw, err)
		default:
			build.Outdated()
			reply(rw, res)
		}
	})
	mux.HandleFunc("GET /api/config/copy", func(rw http.ResponseWriter, r *http.Request) {
		sum, err := store.copySummary()
		if err != nil {
			fail(rw, err)
			return
		}
		reply(rw, map[string]any{"dir": store.SignedConfig, "copy": sum})
	})
	// The running version into nixcfg, when another host published it.
	mux.HandleFunc("POST /api/config/copy", func(rw http.ResponseWriter, r *http.Request) {
		b, err := d.bundle("/warden/config")
		if err == nil && b == nil {
			err = errors.New("the daemon runs no signed config")
		}
		if err == nil {
			store.mu.Lock()
			err = configstore.WriteBundle(store.SignedConfig, b)
			store.mu.Unlock()
		}
		if err != nil {
			fail(rw, err)
			return
		}
		sum, err := store.copySummary()
		if err != nil {
			fail(rw, err)
			return
		}
		reply(rw, map[string]any{"dir": store.SignedConfig, "copy": sum})
	})
	mux.HandleFunc("POST /api/config/hash", func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Path string `json:"path"`
		}
		if !decode(rw, r, &body) {
			return
		}
		sum, size, err := store.hashFile(body.Path)
		if err != nil {
			fail(rw, err)
			return
		}
		reply(rw, map[string]any{"sha256": sum, "bytes": size})
	})
	mux.HandleFunc("GET /api/config/hf", func(rw http.ResponseWriter, r *http.Request) {
		sum, size, err := store.hfFile(r.URL.Query().Get("source"))
		if err != nil {
			fail(rw, err)
			return
		}
		reply(rw, map[string]any{"sha256": sum, "bytes": size})
	})
}
