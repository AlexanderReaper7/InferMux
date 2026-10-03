package stats

import (
	"math"
	"slices"
	"sort"
)

// Spread is a median and a 95th percentile, over the requests that
// reported the number. nil when none did.
type Spread struct {
	Median float64 `json:"median"`
	P95    float64 `json:"p95"`
	N      int     `json:"n"`
}

// Summary is one model's requests. Only successful ones count towards the
// numbers: a refused or cancelled request says nothing about the model's
// speed.
type Summary struct {
	Model            string  `json:"model"`
	Requests         int     `json:"requests"`
	Failed           int     `json:"failed"`
	TTFTMs           *Spread `json:"ttft_ms"`
	PrefillMs        *Spread `json:"prefill_ms"`
	WaitMs           *Spread `json:"wait_ms"`
	PrefillPerSecond *Spread `json:"prefill_per_second"`
	DecodePerSecond  *Spread `json:"decode_per_second"`
	// CacheShare is the cached prompt tokens over all prompt tokens, and
	// DraftAcceptance the accepted drafts over all drafts: each a ratio of
	// sums, so one long request weighs what it cost.
	CacheShare      *float64 `json:"cache_share"`
	DraftAcceptance *float64 `json:"draft_acceptance"`
}

// Summarize is each model's summary, by model name.
func Summarize(requests []Request) []Summary {
	by := map[string][]Request{}
	for _, r := range requests {
		by[r.Model] = append(by[r.Model], r)
	}
	out := []Summary{}
	for model, rs := range by {
		s := Summary{Model: model, Requests: len(rs)}
		var ttft, prefill, wait, prefillRate, decode []float64
		var prompt, cached, drafted, accepted int
		for _, r := range rs {
			if r.Status != 200 {
				s.Failed++
				continue
			}
			add(&ttft, r.TTFTMs)
			add(&prefill, r.PrefillMs)
			add(&prefillRate, r.PrefillPerSecond)
			add(&decode, r.DecodePerSecond)
			if r.TTFTMs != nil && r.PrefillMs != nil {
				w := max(*r.TTFTMs-*r.PrefillMs, 0)
				wait = append(wait, w)
			}
			if r.PromptTokens != nil && r.CachedTokens != nil {
				prompt += *r.PromptTokens
				cached += *r.CachedTokens
			}
			if r.DraftTokens != nil && r.DraftAccepted != nil {
				drafted += *r.DraftTokens
				accepted += *r.DraftAccepted
			}
		}
		s.TTFTMs, s.PrefillMs, s.WaitMs = spread(ttft), spread(prefill), spread(wait)
		s.PrefillPerSecond, s.DecodePerSecond = spread(prefillRate), spread(decode)
		s.CacheShare, s.DraftAcceptance = ratio(cached, prompt), ratio(accepted, drafted)
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

func add(to *[]float64, v *float64) {
	if v != nil && !math.IsNaN(*v) && !math.IsInf(*v, 0) {
		*to = append(*to, *v)
	}
}

func ratio(part, whole int) *float64 {
	if whole <= 0 {
		return nil
	}
	r := float64(part) / float64(whole)
	return &r
}

// spread uses the nearest rank, so every number shown is one a request had.
func spread(v []float64) *Spread {
	if len(v) == 0 {
		return nil
	}
	v = slices.Clone(v)
	slices.Sort(v)
	rank := func(p float64) float64 {
		i := int(math.Ceil(p*float64(len(v)))) - 1
		return v[min(max(i, 0), len(v)-1)]
	}
	return &Spread{Median: rank(0.5), P95: rank(0.95), N: len(v)}
}
