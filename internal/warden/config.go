// Package warden is InferMux's GPU policy: it measures what else is using the
// card, decides whether that other work is at stake, unloads the models,
// frees an idle ComfyUI, and tells its consumers to pause or resume. It also
// sorts every request into interactive or batch, so a prompt the user sent is
// never the thing that gets killed.
//
// The decision records are in docs/decisions. (0001) means
// docs/decisions/0001-*.md.
package warden

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the warden's own file, separate from llama-swap's so that a merge
// from upstream can never collide with it. `-warden-config` names it; without
// the flag InferMux runs as plain llama-swap.
//
// Absent keys keep their defaults, so a file with only `consumers:` in it is a
// complete configuration.
type Config struct {
	// ComfyUIURL is empty when there is no ComfyUI to watch.
	ComfyUIURL string `yaml:"comfyui_url" json:"comfyui_url"`

	// ComfyUIUnit is ComfyUI's systemd unit. Its only job is to be refused in
	// OurUnits: ComfyUI's work is contention, and listing it as ours would
	// hide it from the utilization signal and count its VRAM as the models'
	// (0002). A save through the UI did exactly that on 2026-10-04.
	ComfyUIUnit string `yaml:"comfyui_unit" json:"comfyui_unit"`

	// OurUnits are systemd units whose GPU work is ours, not contention,
	// matched against the last component of /proc/<pid>/cgroup. InferMux's own
	// unit is always ours, since the models it starts are its children.
	OurUnits []string `yaml:"our_units" json:"our_units"`

	// DesktopProcesses are process names whose GPU work is never contention:
	// the compositor, and the Electron GPU process that draws T3 Code's
	// streamed reply. On 2026-10-02 those two alone read 30 to 38% and
	// unloaded the model that was writing the reply.
	DesktopProcesses []string `yaml:"desktop_processes" json:"desktop_processes"`

	// Host is this machine's name among the hosts, the <host> in
	// <host>/<model> (0006, 2). Empty means "this", in a single-host setup.
	Host string `yaml:"host" json:"host"`

	// KeysFile is keys.yaml, relative to this file. Set, every request but a
	// health check needs a key in it, and the key decides the class (0006, 4).
	// Empty, no key is needed and every request is interactive.
	KeysFile string `yaml:"keys_file" json:"keys_file"`

	// Remotes are the other hosts whose models this one routes to (0006, 2).
	Remotes []Remote `yaml:"remotes" json:"remotes"`

	// Keys is KeysFile, read with this file. Nil without a KeysFile.
	Keys *KeyFile `yaml:"-" json:"-"`
	// KeysPath is KeysFile resolved, for the watcher.
	KeysPath string `yaml:"-" json:"-"`

	// TrustedHosts are host names, besides the loopback ones, on which a
	// browser may write: the name `tailscale serve` answers on, which it
	// passes through as the Host. Nobody can point a DNS name of the tailnet
	// at a page of theirs, which is what keeps this from undoing the
	// rebinding check (0005).
	TrustedHosts []string `yaml:"trusted_hosts" json:"trusted_hosts"`

	Policy    Policy     `yaml:"policy" json:"policy"`
	Consumers []Consumer `yaml:"consumers" json:"consumers"`
}

// Remote is another InferMux. URL is its HTTPS address on the tailnet
// (0006, 5); KeyFile holds this host's own key there, for discovery: a
// forwarded request carries its client's key, not this one.
type Remote struct {
	Name    string `yaml:"name" json:"name"`
	URL     string `yaml:"url" json:"url"`
	KeyFile string `yaml:"key_file" json:"key_file"`
}

// Policy is the decision table's numbers. See Decide for what each one does.
type Policy struct {
	// GPUBusyPercent is the foreign utilization that counts as contention.
	// 25% is well above an idle desktop and well below anything that renders.
	GPUBusyPercent float64 `yaml:"gpu_busy_percent" json:"gpu_busy_percent"`

	// MinFreeVRAMMB is read only while no model is loaded. The desktop holds
	// 4.2 to 4.7 GB of the 10 GB card, so the Windows value of 6000 would read
	// an idle desktop as contended and never resume.
	MinFreeVRAMMB int `yaml:"min_free_vram_mb" json:"min_free_vram_mb"`

	// ResumeQuietSeconds: yield at once, come back only after this much quiet.
	ResumeQuietSeconds int `yaml:"resume_quiet_seconds" json:"resume_quiet_seconds"`

	// PollSeconds is the yield latency. NVML answers in ~6 ms (measured
	// 2026-09-28 on the 3080), so a short tick costs nothing.
	PollSeconds float64 `yaml:"poll_seconds" json:"poll_seconds"`

	// ComfyUIIdleSeconds is how long ComfyUI's queue stays empty before it is
	// told to drop its models (0002).
	ComfyUIIdleSeconds int `yaml:"comfyui_idle_seconds" json:"comfyui_idle_seconds"`

	// InteractiveRecentSeconds is how long after the last interactive request
	// the models are still not unloaded on a yield. Ten minutes covers reading
	// a reply and typing the next prompt (0004).
	InteractiveRecentSeconds int `yaml:"interactive_recent_seconds" json:"interactive_recent_seconds"`

	// Enabled false measures on request only and announces nothing.
	Enabled bool `yaml:"enabled" json:"enabled"`
}

// Consumer is something that yields the GPU when told to. URL is the base of
// an HTTP service and AnnouncePath the route that takes
// {"action": "pause"|"resume", "reason": "..."}. What a consumer does about it
// is the consumer's business.
type Consumer struct {
	Name           string  `yaml:"name" json:"name"`
	URL            string  `yaml:"url" json:"url"`
	AnnouncePath   string  `yaml:"announce_path" json:"announce_path"`
	TimeoutSeconds float64 `yaml:"timeout_seconds" json:"timeout_seconds"`
}

func (c Consumer) Endpoint() string {
	return strings.TrimRight(c.URL, "/") + c.AnnouncePath
}

func (c Consumer) timeout() time.Duration {
	return time.Duration(c.TimeoutSeconds * float64(time.Second))
}

// DefaultConfig is what an empty file means.
func DefaultConfig() Config {
	return Config{
		Policy: Policy{
			GPUBusyPercent:           25,
			MinFreeVRAMMB:            3000,
			ResumeQuietSeconds:       300,
			PollSeconds:              5,
			ComfyUIIdleSeconds:       600,
			InteractiveRecentSeconds: 600,
			Enabled:                  true,
		},
	}
}

// LoadConfig reads the file over the defaults.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	var keys map[string]any
	if yaml.Unmarshal(raw, &keys) == nil {
		if _, old := keys["batch_api_keys"]; old {
			// Dropping it silently would make a batch client interactive.
			return cfg, fmt.Errorf("%s: batch_api_keys moved to keys.yaml, as keys with class: batch (0006)", path)
		}
	}
	if cfg.KeysFile != "" {
		cfg.KeysPath = keysPath(path, cfg.KeysFile)
		kf, err := LoadKeys(cfg.KeysPath)
		if err != nil {
			return cfg, err
		}
		cfg.Keys = &kf
	}
	seen := map[string]bool{cfg.Host: true}
	for i, r := range cfg.Remotes {
		if r.Name == "" || r.URL == "" {
			return cfg, fmt.Errorf("%s: remote %d needs a name and a url", path, i+1)
		}
		if seen[r.Name] {
			return cfg, fmt.Errorf("%s: remote %s is named twice, or is this host", path, r.Name)
		}
		seen[r.Name] = true
	}
	if cfg.ComfyUIUnit != "" && slices.Contains(cfg.OurUnits, cfg.ComfyUIUnit) {
		return cfg, fmt.Errorf("%s: our_units lists %s, which is ComfyUI: its work is contention, never ours (0002)", path, cfg.ComfyUIUnit)
	}
	for i := range cfg.Consumers {
		c := &cfg.Consumers[i]
		if c.Name == "" || c.URL == "" {
			return cfg, fmt.Errorf("%s: consumer %d needs a name and a url", path, i+1)
		}
		if c.AnnouncePath == "" {
			c.AnnouncePath = "/api/pipeline/announce"
		}
		if c.TimeoutSeconds <= 0 {
			c.TimeoutSeconds = 10
		}
	}
	if err := cfg.Policy.check(); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// check refuses a number no one could mean, rather than quietly running on
// another: a negative threshold would read every desktop as contention and
// never resume. A reload that fails keeps the settings before it.
func (p Policy) check() error {
	if p.GPUBusyPercent < 0 || p.GPUBusyPercent > 100 {
		return fmt.Errorf("gpu_busy_percent is %v, not 0 to 100", p.GPUBusyPercent)
	}
	if p.PollSeconds < 1 {
		return fmt.Errorf("poll_seconds is %v, less than 1", p.PollSeconds)
	}
	for name, v := range map[string]int{
		"min_free_vram_mb":           p.MinFreeVRAMMB,
		"resume_quiet_seconds":       p.ResumeQuietSeconds,
		"comfyui_idle_seconds":       p.ComfyUIIdleSeconds,
		"interactive_recent_seconds": p.InteractiveRecentSeconds,
	} {
		if v < 0 {
			return fmt.Errorf("%s is %d, below 0", name, v)
		}
	}
	return nil
}
