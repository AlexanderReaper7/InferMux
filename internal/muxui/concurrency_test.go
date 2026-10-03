package muxui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"gopkg.in/yaml.v3"
)

// Saves from several tabs at once, while the daemon's watcher reads: every
// save lands, and no reader ever sees a truncated or half-written file.
func TestConcurrentSavesLandWhileAReaderNeverSeesHalfAFile(t *testing.T) {
	f := newFixture(t)
	base := f.model(t, "qwen")
	const writers, rounds = 8, 15

	stop := make(chan struct{})
	var reads, bad atomic.Int64
	var firstBad atomic.Value
	var readers sync.WaitGroup
	for r := 0; r < 2; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				names, _ := filepath.Glob(filepath.Join(f.store.ModelsDir, "*.yaml"))
				for _, name := range names {
					raw, err := os.ReadFile(name)
					if os.IsNotExist(err) {
						continue // renamed away between the listing and the read
					}
					var doc struct {
						Models map[string]any `yaml:"models"`
					}
					if err == nil {
						err = yaml.Unmarshal(raw, &doc)
					}
					if err == nil && len(doc.Models) == 0 {
						err = fmt.Errorf("%s has no models: %d bytes", filepath.Base(name), len(raw))
					}
					if err != nil {
						bad.Add(1)
						firstBad.CompareAndSwap(nil, err.Error())
					}
				}
				reads.Add(1)
			}
		}()
	}

	var wg sync.WaitGroup
	errs := make(chan error, writers*rounds)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			m := base
			m.Name, m.File, m.Comment = fmt.Sprintf("m%d", w), "", ""
			m.Flags = append([]Flag{}, base.Flags...)
			if err := f.store.SaveModel("", m); err != nil {
				errs <- err
				return
			}
			for i := 0; i < rounds; i++ {
				m.Description = fmt.Sprintf("round %d %s", i, strings.Repeat("x", 4000))
				if err := f.store.SaveModel(m.Name, m); err != nil {
					errs <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(stop)
	readers.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("save: %v", err)
	}
	if bad.Load() > 0 {
		t.Fatalf("%d of %d reads failed, first: %v", bad.Load(), reads.Load(), firstBad.Load())
	}
	for w := 0; w < writers; w++ {
		got := f.model(t, fmt.Sprintf("m%d", w))
		if !strings.HasPrefix(got.Description, fmt.Sprintf("round %d ", rounds-1)) {
			t.Errorf("m%d lost its last save: %.20q", w, got.Description)
		}
	}
	leftovers, _ := filepath.Glob(filepath.Join(f.store.ModelsDir, ".*"))
	if len(leftovers) > 0 {
		t.Errorf("temporary files left: %v", leftovers)
	}
	t.Logf("%d reads during the saves", reads.Load())
}
