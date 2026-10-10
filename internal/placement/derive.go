package placement

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Part is one host's share of the config.
type Part struct {
	Host string
	// LlamaSwap is what llama-swap loads from its -config-dir: this host's
	// models, one group per device, and the clouds as peers.
	LlamaSwap []byte
	// Routes is each model's places as the failover handler tries them,
	// balance first, for every model on any host, so this host routes a
	// request for a model it does not run.
	Routes map[string][]Route
	// Local is each llama-swap model this host runs, by its name here.
	Local map[string]Local
	// Devices are this host's.
	Devices map[string]Device
	// Remotes are the other hosts' URLs.
	Remotes map[string]string
	// Files are every model's files, on any host, by path: what this host
	// may serve another (internal/fetch).
	Files map[string]File
}

// Route is one place of a model's lanes.
type Route struct {
	// Place is the failover handler's: the host, which serves the model's
	// own name, or host/name for a second device there (Instance).
	Place       string `json:"place"`
	Host        string `json:"host"`
	Device      string `json:"device"`
	MaxInflight int    `json:"max_inflight,omitempty"`
	OnlyIfIdle  bool   `json:"only_if_idle,omitempty"`
	BatchOnly   bool   `json:"batch_only,omitempty"`
	// Failover is a place of the failover lane.
	Failover bool `json:"failover,omitempty"`
}

// Local is a llama-swap model of this host: the config's model it runs,
// the devices it occupies, its place's first, and the paths of its files.
type Local struct {
	Model   string   `json:"model"`
	Devices []string `json:"devices"`
	Files   []string `json:"files"`
}

// Instance is the name a model has on one device of a host. Its first place
// on the host, balance before failover, serves the model's own name, so a
// client asking that host for it gets that device. Any further device there
// serves <model>@<device>, unlisted.
func Instance(model string, places []Place, host, device string) string {
	first := true
	for _, p := range places {
		if p.Host() != host {
			continue
		}
		if p.Device() == device {
			if first {
				return model
			}
			return model + "@" + device
		}
		first = false
	}
	return model + "@" + device
}

// Options are what the host running the part knows and the config does not.
type Options struct {
	// CPURunner goes before a CPU device's commands, so the runner starts
	// them in its slice instead of the daemon in its own cgroup
	// (internal/cpurun): `infermux-cpu run -socket <path> --`. Empty, they
	// run as the daemon's children.
	CPURunner []string
}

// Derive is host's part. A host the config does not list runs no model of
// its own and still routes to the others.
func (c *Config) Derive(host string) (*Part, error) {
	return c.DeriveFor(host, Options{})
}

// DeriveFor is Derive on a host with opts.
func (c *Config) DeriveFor(host string, opts Options) (*Part, error) {
	part := &Part{
		Host:    host,
		Routes:  map[string][]Route{},
		Local:   map[string]Local{},
		Devices: c.Hosts[host].Devices,
		Remotes: map[string]string{},
		Files:   map[string]File{},
	}
	for name, h := range c.Hosts {
		if name != host {
			part.Remotes[name] = h.URL
		}
	}
	models := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	members := map[string][]string{}
	for _, name := range c.ModelNames() {
		m := c.Models[name]
		var files []string
		for p, f := range m.Files {
			part.Files[p] = f
			files = append(files, p)
		}
		sort.Strings(files)
		balance, failover, err := c.Lanes(name)
		if err != nil {
			return nil, err
		}
		all := append(slices.Clone(balance), failover...)
		for i, p := range all {
			instance := Instance(name, all, p.Host(), p.Device())
			route := Route{
				Place:       p.Host(),
				Host:        p.Host(),
				Device:      p.Device(),
				MaxInflight: p.MaxInflight,
				OnlyIfIdle:  p.OnlyIfIdle,
				BatchOnly:   p.BatchOnly,
				Failover:    i >= len(balance),
			}
			if instance != name {
				route.Place = p.Host() + "/" + instance
			}
			part.Routes[name] = append(part.Routes[name], route)
			if p.Host() != host {
				continue
			}
			entry, err := c.entry(m, p, instance, opts)
			if err != nil {
				return nil, fmt.Errorf("%s on %s: %w", name, p.Place, err)
			}
			models.Content = append(models.Content, scalar(instance), entry)
			members[p.Device()] = append(members[p.Device()], instance)
			part.Local[instance] = Local{Model: name, Devices: append([]string{p.Device()}, p.Occupies...), Files: files}
		}
	}
	doc := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	if len(models.Content) > 0 {
		doc.Content = append(doc.Content, scalar("models"), models)
	}
	if len(members) > 0 {
		groups := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		for _, device := range sortedKeys(members) {
			sort.Strings(members[device])
			g := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			setKey(g, "swap", boolean(!part.Devices[device].Concurrent))
			// A device's model never stops another device's.
			setKey(g, "exclusive", boolean(false))
			list := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
			for _, m := range members[device] {
				list.Content = append(list.Content, scalar(m))
			}
			setKey(g, "members", list)
			groups.Content = append(groups.Content, scalar(device), g)
		}
		doc.Content = append(doc.Content, scalar("groups"), groups)
	}
	if len(c.Clouds) > 0 {
		peers := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		for _, name := range sortedKeys(c.Clouds) {
			cl := c.Clouds[name]
			p := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			setKey(p, "proxy", scalar(cl.URL))
			if cl.APIKey != "" {
				setKey(p, "apiKey", scalar(cl.APIKey))
			}
			list := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
			for _, m := range cl.Models {
				list.Content = append(list.Content, scalar(m))
			}
			setKey(p, "models", list)
			peers.Content = append(peers.Content, scalar(name), p)
		}
		doc.Content = append(doc.Content, scalar("peers"), peers)
	}
	doc.HeadComment = "Derived by InferMux from the signed config for " + host + " (0022). Rewritten on every change; edit the config, not this."
	out, err := yaml.Marshal(&yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{doc}})
	if err != nil {
		return nil, err
	}
	part.LlamaSwap = out
	return part, nil
}

// DeriveAll is every listed host's part. A publisher runs it before signing,
// so a config that some host cannot run is refused where it is written, not
// found later on that host.
func (c *Config) DeriveAll() (map[string]*Part, error) {
	parts := map[string]*Part{}
	for _, host := range sortedKeys(c.Hosts) {
		part, err := c.Derive(host)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", host, err)
		}
		parts[host] = part
	}
	return parts, nil
}

// entry is the model's llama-swap entry on one device: the device decides
// the environment, a CPU also the device flags, and the place's overrides
// apply before those, so no override reaches a card the model was not
// placed on (0022).
func (c *Config) entry(m *Model, p Place, instance string, opts Options) (*yaml.Node, error) {
	device := c.Hosts[p.Host()].Devices[p.Device()]
	body := deepCopy(m.Body)
	a := splitCmd(m.Cmd())
	changed := len(p.Flags) > 0
	a = applyFlags(a, p.Flags)
	if device.Kind == KindCPU && isLlamaCpp(a) {
		a = a.without(append(slices.Clone(gpuLayerFlags), deviceFlags...)...).set("--device", "none").set("-ngl", "0")
		changed = true
	}
	cmd := a.render()
	if device.Kind == KindCPU && len(opts.CPURunner) > 0 {
		prefix := make([]string, len(opts.CPURunner))
		for i, t := range opts.CPURunner {
			prefix[i] = quote(t)
		}
		cmd = strings.Join(prefix, " ") + " " + cmd
		changed = true
	}
	if changed {
		setKey(body, "cmd", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: cmd + "\n", Style: yaml.LiteralStyle})
	}
	env := mapValue(body, "env")
	if env == nil {
		env = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		setKey(body, "env", env)
	}
	if env.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("env is a list of NAME=value")
	}
	for _, e := range env.Content {
		if strings.HasPrefix(e.Value, "CUDA_VISIBLE_DEVICES=") {
			return nil, fmt.Errorf("env sets CUDA_VISIBLE_DEVICES, which the device decides")
		}
	}
	visible := ""
	if device.Kind == KindGPU {
		visible = strconv.Itoa(device.GPUIndex())
	}
	env.Content = append(env.Content, scalar("CUDA_VISIBLE_DEVICES="+visible))
	if p.TTL != nil {
		setKey(body, "ttl", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(*p.TTL)})
	}
	if instance != m.Name {
		// A second device's copy: the model's own name already lists it, and
		// an alias on both would be a clash.
		setKey(body, "unlisted", boolean(true))
		deleteKey(body, "aliases")
	}
	return body, nil
}

func scalar(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
}

func boolean(b bool) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(b)}
}

func deepCopy(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	out := *n
	out.Content = make([]*yaml.Node, len(n.Content))
	for i, c := range n.Content {
		out.Content[i] = deepCopy(c)
	}
	return &out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
