package warden

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Failover is failover.yaml (0016): a model's name, as clients send it, to the
// places to try in order. A place is a host, which serves the same name, or
// <host>/<model>, which serves another. This host's own name means its own
// models, so a CPU build of a model can be a place without being a host.
//
//	Octen-Embedding-4B.Q8_0: [zbox, reaperboi]
type Failover map[string][]FailoverPlace

// FailoverPlace optionally limits requests at a destination. A bare string
// keeps failure-only routing; a mapping enables overflow when it is busy.
type FailoverPlace struct {
	Place       string `yaml:"place"`
	MaxInflight int    `yaml:"max_inflight,omitempty"`
	OnlyIfIdle  bool   `yaml:"only_if_idle,omitempty"`
	BatchOnly   bool   `yaml:"batch_only,omitempty"`
}

func (p *FailoverPlace) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode && n.Tag == "!!str" {
		p.Place = n.Value
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("a place must be a host name or a mapping")
	}
	seen := map[string]bool{}
	for i := 0; i < len(n.Content); i += 2 {
		key, value := n.Content[i].Value, n.Content[i+1]
		if seen[key] {
			return fmt.Errorf("duplicate place setting %q", key)
		}
		seen[key] = true
		switch key {
		case "place":
			if value.Tag != "!!str" {
				return fmt.Errorf("place must be a string")
			}
			if err := value.Decode(&p.Place); err != nil {
				return err
			}
		case "max_inflight":
			if value.Tag != "!!int" {
				return fmt.Errorf("max_inflight must be an integer")
			}
			if err := value.Decode(&p.MaxInflight); err != nil {
				return err
			}
		case "only_if_idle":
			if value.Tag != "!!bool" {
				return fmt.Errorf("only_if_idle must be a boolean")
			}
			if err := value.Decode(&p.OnlyIfIdle); err != nil {
				return err
			}
		case "batch_only":
			if value.Tag != "!!bool" {
				return fmt.Errorf("batch_only must be a boolean")
			}
			if err := value.Decode(&p.BatchOnly); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown place setting %q", key)
		}
	}
	return nil
}

// LoadFailover reads the file and checks every place against hosts, this host
// and the remotes, so a typo fails the load instead of a request.
func LoadFailover(path string, hosts map[string]bool) (Failover, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f Failover
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for model, places := range f {
		if len(places) < 2 {
			return nil, fmt.Errorf("%s: %s needs at least two places to fail over between", path, model)
		}
		seen := []string{}
		for _, destination := range places {
			place := destination.Place
			if destination.MaxInflight < 0 {
				return nil, fmt.Errorf("%s: %s: max_inflight must not be negative", path, model)
			}
			if strings.TrimSpace(model) == "" || strings.TrimSpace(model) != model || strings.TrimSpace(place) != place || place == "" || strings.HasSuffix(place, "/") {
				return nil, fmt.Errorf("%s: model and place names must not be empty or have surrounding whitespace", path)
			}
			host, _, _ := strings.Cut(place, "/")
			if !hosts[host] {
				return nil, fmt.Errorf("%s: %s: %q is not this host or a remote", path, model, place)
			}
			if slices.Contains(seen, place) {
				return nil, fmt.Errorf("%s: %s: %q is listed twice", path, model, place)
			}
			seen = append(seen, place)
		}
	}
	return f, nil
}
