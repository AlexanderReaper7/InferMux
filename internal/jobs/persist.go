package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// A job is written to disk only on a shutdown signal, and read back and the
// file deleted at the next start (0023). The file holds each unfinished
// job's request, its events and the reply so far, never its key: a job read
// back waits for its client to come back with the key, when keys are
// required.

type savedFile struct {
	Version int        `json:"version"`
	Jobs    []savedJob `json:"jobs"`
}

type savedJob struct {
	ID        string          `json:"id"`
	Client    string          `json:"client"`
	Path      string          `json:"path"`
	Request   json.RawMessage `json:"request"`
	Created   time.Time       `json:"created"`
	Reason    string          `json:"reason,omitempty"`
	Attempts  int             `json:"attempts"`
	Events    []Event         `json:"events"`
	Reply     reply           `json:"reply"`
	NotBefore time.Time       `json:"not_before,omitzero"`
}

// Drain starts no new attempt from now on and lets the ones running go on,
// for a deploy's stop to wait for (0026). Stop pauses what is still running.
func (s *Store) Drain() {
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	s.draining.Store(true)
}

// begin counts an attempt about to start, or refuses it once Drain has
// begun. The check and the count are one step under drainMu: a runner that
// wait let go just before Drain either is counted before Drain returns, or
// starts nothing (sol, review 11).
func (s *Store) begin() bool {
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	if s.draining.Load() {
		return false
	}
	s.attempting.Add(1)
	return true
}

// Attempting is how many attempts are running.
func (s *Store) Attempting() int { return int(s.attempting.Load()) }

// Stop refuses new jobs, stops every attempt as a pause, waits up to timeout
// for the runners, and writes the unfinished jobs to File. Then the event
// streams end.
func (s *Store) Stop(timeout time.Duration) error {
	s.mu.Lock()
	if !s.started || s.stopping {
		s.mu.Unlock()
		return nil
	}
	s.stopping = true
	s.mu.Unlock()
	s.stopAll(errStopping)
	done := make(chan struct{})
	go func() {
		s.runners.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		s.logf("jobs: an attempt still running after %s; writing the replies as they are", timeout)
	}
	err := s.save()
	close(s.closing)
	return err
}

func (s *Store) save() error {
	s.mu.Lock()
	var out savedFile
	out.Version = 1
	for _, j := range s.jobs {
		j.mu.Lock()
		if !j.state.final() {
			out.Jobs = append(out.Jobs, savedJob{
				ID: j.id, Client: j.client, Path: j.path, Request: j.request, Created: j.created,
				Reason: j.reason, Attempts: j.attempts, Events: j.events, Reply: j.reply, NotBefore: j.notBefore,
			})
		}
		j.mu.Unlock()
	}
	s.mu.Unlock()
	if len(out.Jobs) == 0 {
		return nil
	}
	if s.File == "" {
		s.logf("jobs: %d unfinished jobs are lost, since no jobs file is set", len(out.Jobs))
		return nil
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return err
	}
	tmp := s.File + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("jobs: %w", err)
	}
	if err := os.Rename(tmp, s.File); err != nil {
		return fmt.Errorf("jobs: %w", err)
	}
	s.logf("jobs: wrote %d unfinished jobs to %s", len(out.Jobs), s.File)
	return nil
}

// load reads back the jobs the last Stop wrote, and deletes the file. A file
// that cannot be read is moved aside, not deleted.
func (s *Store) load() []*Job {
	if s.File == "" {
		return nil
	}
	raw, err := os.ReadFile(s.File)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	var in savedFile
	if err == nil {
		err = json.Unmarshal(raw, &in)
	}
	if err != nil {
		aside := s.File + ".unreadable"
		os.Rename(s.File, aside)
		s.logf("jobs: could not read %s, moved to %s: %v", s.File, filepath.Base(aside), err)
		return nil
	}
	os.Remove(s.File)
	var out []*Job
	for _, saved := range in.Jobs {
		k, ok := endpoints[saved.Path]
		model, err := check(k, saved.Request)
		if !ok || err != nil {
			s.logf("jobs: dropped %s read back: %s %v", saved.ID, saved.Path, err)
			continue
		}
		j := &Job{
			id: saved.ID, client: saved.Client, path: saved.Path, model: model, kind: k,
			request: saved.Request, created: saved.Created, attempts: saved.Attempts,
			events: saved.Events, reply: saved.Reply, notBefore: saved.NotBefore,
			state: Paused, reason: saved.Reason,
			changed: make(chan struct{}), nudge: make(chan struct{}, 1),
		}
		reason := "InferMux restarted"
		if s.keysRequired() {
			reason += "; waiting for its client to come back with its key"
		} else {
			j.auth = http.Header{}
		}
		j.setState(Paused, "", reason)
		out = append(out, j)
	}
	if len(out) > 0 {
		s.logf("jobs: read back %d unfinished jobs from %s", len(out), s.File)
	}
	return out
}
