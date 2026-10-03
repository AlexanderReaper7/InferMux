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

	// OurUnits are systemd units whose GPU work is ours, not contention,
	// matched against the last component of /proc/<pid>/cgroup. InferMux's own
	// unit is always ours, since the models it starts are its children.
	OurUnits []string `yaml:"our_units" json:"our_units"`

	// DesktopProcesses are process names whose GPU work is never contention:
	// the compositor, and the Electron GPU process that draws T3 Code's
	// streamed reply. On 2026-10-02 those two alone read 30 to 38% and
	// unloaded the model that was writing the reply.
	DesktopProcesses []string `yaml:"desktop_processes" json:"desktop_processes"`

	// BatchAPIKeys mark a request as batch: it is refused while the verdict is
	// pause and cancelled on a yield. Any other key, or none, is interactive.
	// These are labels, not secrets: InferMux listens on loopback and does no
	// authentication of its own.
	BatchAPIKeys []string `yaml:"batch_api_keys" json:"-"`

	Policy    Policy     `yaml:"policy" json:"policy"`
	Consumers []Consumer `yaml:"consumers" json:"-"`
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
	Name           string  `yaml:"name"`
	URL            string  `yaml:"url"`
	AnnouncePath   string  `yaml:"announce_path"`
	TimeoutSeconds float64 `yaml:"timeout_seconds"`
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
	if cfg.Policy.PollSeconds < 1 {
		cfg.Policy.PollSeconds = 1
	}
	return cfg, nil
}
