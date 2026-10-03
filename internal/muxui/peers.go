package muxui

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Peer is one of llama-swap's peers, a cloud API such as OpenRouter (0006,
// 6). The UI edits only its model list; the proxy and the key stay as written
// in the file, and the key is a ${env.NAME} only the daemon can fill.
type Peer struct {
	Name   string   `json:"name"`
	Proxy  string   `json:"proxy"`
	Models []string `json:"models"`
	File   string   `json:"file"`
}

// Peers lists the peers in the models directory's files, by name.
func (s *Store) Peers() ([]Peer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	files, err := readModelFiles(s.ModelsDir)
	if err != nil {
		return nil, err
	}
	peers := []Peer{}
	for _, f := range files {
		node := mappingValue(f.root(), "peers", false)
		if node == nil {
			continue
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			p := Peer{Name: node.Content[i].Value, File: f.name, Models: []string{}}
			def := node.Content[i+1]
			if proxy := mappingValue(def, "proxy", false); proxy != nil {
				p.Proxy = proxy.Value
			}
			if list := mappingValue(def, "models", false); list != nil {
				for _, m := range list.Content {
					p.Models = append(p.Models, m.Value)
				}
			}
			peers = append(peers, p)
		}
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].Name < peers[j].Name })
	return peers, nil
}

// SavePeerModels replaces the model list of the peer called name, in the file
// that holds it, after llama-swap's loader has accepted the result.
func (s *Store) SavePeerModels(name string, models []string) error {
	seen := map[string]bool{}
	list := &yaml.Node{Kind: yaml.SequenceNode}
	for _, m := range models {
		m = strings.TrimSpace(m)
		if m == "" {
			return fmt.Errorf("a model name is empty")
		}
		if seen[m] {
			return fmt.Errorf("%s is listed twice", m)
		}
		seen[m] = true
		list.Content = append(list.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: m})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	files, err := readModelFiles(s.ModelsDir)
	if err != nil {
		return err
	}
	for _, f := range files {
		node := mappingValue(f.root(), "peers", false)
		if node == nil {
			continue
		}
		def := mappingValue(node, name, false)
		if def == nil {
			continue
		}
		setValue(def, "models", list)
		out, err := f.bytes()
		if err != nil {
			return err
		}
		return s.apply(files, map[string][]byte{f.name: out})
	}
	return fmt.Errorf("%w: peer %s", ErrNotFound, name)
}
