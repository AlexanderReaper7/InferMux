package catalog

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Peers reads a cloud peer's facts from the peer's own /v1/models, in
// OpenRouter's format: context_length, architecture.input_modalities and
// supported_parameters (the user's choice, 2026-10-03). A peer whose list is
// not in that format, or not readable without a key, gets none.
type Peers struct {
	// Lookup is a peer model's proxy URL and the peer's name for it, false
	// for a name that is not a peer's.
	Lookup func(id string) (proxy, model string, ok bool)
	Client *http.Client

	mu    sync.Mutex
	lists map[string]peerList
}

type peerList struct {
	next   time.Time
	models map[string]Facts
}

const (
	// Every Codex start and every health probe lists the models; the
	// provider's list changes on the scale of days.
	peerListTTL = time.Hour
	// A failed read is tried again sooner, and the last good list is kept.
	peerListRetry = time.Minute
)

// facts is one peer model's Facts, false when its peer says nothing about it.
func (p *Peers) facts(id string) (Facts, bool) {
	proxy, model, ok := p.Lookup(id)
	if !ok {
		return Facts{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	list := p.lists[proxy]
	if now.After(list.next) {
		list.next = now.Add(peerListRetry)
		if models, err := p.read(proxy); err == nil {
			list = peerList{next: now.Add(peerListTTL), models: models}
		}
		if p.lists == nil {
			p.lists = map[string]peerList{}
		}
		p.lists[proxy] = list
	}
	f, ok := list.models[model]
	return f, ok
}

// openRouterModel is the part of OpenRouter's /api/v1/models entry the
// facts come from.
type openRouterModel struct {
	ID            string `json:"id"`
	ContextLength int    `json:"context_length"`
	Architecture  struct {
		InputModalities []string `json:"input_modalities"`
	} `json:"architecture"`
	SupportedParameters []string `json:"supported_parameters"`
}

func (p *Peers) read(proxy string) (map[string]Facts, error) {
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Get(strings.TrimSuffix(proxy, "/") + "/v1/models")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", proxy, resp.Status)
	}
	var list struct {
		Data []openRouterModel `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}
	models := map[string]Facts{}
	for _, m := range list.Data {
		if m.ID == "" || m.ContextLength <= 0 {
			continue
		}
		f := Facts{ContextWindow: m.ContextLength, InputModalities: []string{"text"}, ReasoningEfforts: []string{}}
		if slices.Contains(m.Architecture.InputModalities, "image") {
			f.InputModalities = append(f.InputModalities, "image")
		}
		// What the codex catalog for OpenRouter had before InferMux
		// (agents/t3/codex/openrouter.models.sh): OpenRouter maps the effort
		// onto each provider's own setting.
		if slices.Contains(m.SupportedParameters, "reasoning_effort") {
			f.ReasoningEfforts, f.DefaultEffort = []string{"low", "medium", "high"}, "medium"
		}
		models[m.ID] = f
	}
	return models, nil
}
