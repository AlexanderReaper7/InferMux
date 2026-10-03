package warden

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// Telling the consumers: one POST per transition, repeated until it lands.
//
// Push rather than a verdict somebody polls, so a game gets the card within one
// tick instead of within the consumer's next cron (0001). Three things make a
// push survivable without a lock or a lease:
//
//   - Delivery is tracked. A consumer that was down, or answered anything but
//     2xx, keeps its old recorded state, so the next tick tries again. The tick
//     is the retry.
//   - The announcement is repeated every reannounce interval even when nothing
//     changed. A human who resumes Episteme by hand in the middle of a game
//     would otherwise keep the GPU until the game ended.
//   - It is idempotent at the other end. Re-announcing pause to a paused
//     consumer must not re-stamp when the pause began.
//
// A consumer being unreachable is a normal state, not an error. It is logged
// once per change of error, not per attempt.

const reannounceInterval = 300 * time.Second

type delivery struct {
	action string
	at     time.Time
}

// ConsumerState is what /warden/verdict reports per consumer.
type ConsumerState struct {
	URL        string   `json:"url"`
	Action     *string  `json:"action"`
	AgeSeconds *float64 `json:"age_seconds"`
	Error      *string  `json:"error"`
}

type announcer struct {
	consumers  []Consumer
	reannounce time.Duration
	post       func(ctx context.Context, url string, payload any) (any, error)
	log        Logger
	now        func() time.Time

	mu        sync.Mutex
	delivered map[string]delivery
	lastError map[string]string
}

func newAnnouncer(consumers []Consumer, log Logger) *announcer {
	return &announcer{
		consumers:  consumers,
		reannounce: reannounceInterval,
		post:       postJSON,
		log:        log,
		now:        time.Now,
		delivered:  map[string]delivery{},
		lastError:  map[string]string{},
	}
}

func (a *announcer) state() map[string]ConsumerState {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	rows := map[string]ConsumerState{}
	for _, c := range a.consumers {
		row := ConsumerState{URL: c.Endpoint()}
		if d, ok := a.delivered[c.Name]; ok {
			action := d.action
			age := round1(now.Sub(d.at).Seconds())
			row.Action, row.AgeSeconds = &action, &age
		}
		if e, ok := a.lastError[c.Name]; ok {
			row.Error = &e
		}
		rows[c.Name] = row
	}
	return rows
}

// sync brings every consumer up to date with the verdict. Returns how many
// were contacted.
func (a *announcer) sync(v Verdict) int {
	contacted := 0
	for _, c := range a.consumers {
		if !a.due(c, v) {
			continue
		}
		a.announce(c, v)
		contacted++
	}
	return contacted
}

func (a *announcer) due(c Consumer, v Verdict) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	d, ok := a.delivered[c.Name]
	if !ok || d.action != v.Action() {
		return true
	}
	return a.now().Sub(d.at) >= a.reannounce
}

func (a *announcer) announce(c Consumer, v Verdict) {
	payload := map[string]any{
		"action": v.Action(),
		"reason": v.Reason,
		"since":  formatTime(v.Since),
		"warden": "infermux",
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout())
	defer cancel()
	body, err := a.post(ctx, c.Endpoint(), payload)

	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		detail := err.Error()
		if a.lastError[c.Name] != detail {
			a.log.Infof("Consumer %s did not take %s: %s", c.Name, v.Action(), detail)
		}
		a.lastError[c.Name] = detail
		return
	}
	previous, ok := a.delivered[c.Name]
	a.delivered[c.Name] = delivery{action: v.Action(), at: a.now()}
	delete(a.lastError, c.Name)
	if !ok || previous.action != v.Action() {
		answer, _ := json.Marshal(body)
		a.log.Infof("Consumer %s took %s (%s): %s", c.Name, v.Action(), v.Reason, answer)
	}
}

func formatTime(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(time.RFC3339)
	return &s
}
