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
type Failover map[string][]string

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
		for i, place := range places {
			host, _, _ := strings.Cut(place, "/")
			if !hosts[host] {
				return nil, fmt.Errorf("%s: %s: %q is not this host or a remote", path, model, place)
			}
			if slices.Contains(places[:i], place) {
				return nil, fmt.Errorf("%s: %s: %q is listed twice", path, model, place)
			}
		}
	}
	return f, nil
}
