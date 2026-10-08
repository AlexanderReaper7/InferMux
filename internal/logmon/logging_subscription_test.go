package logmon

import (
	"io"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/event"
)

func TestLogMonitor_SubscriptionExcludesQueuedHistory(t *testing.T) {
	// Hold the broadcaster until after subscription. Scheduling cannot hide
	// the race by publishing history before the subscriber is registered.
	m := &Monitor{eventbus: event.NewDispatcherConfig(8), stdout: io.Discard, broadcastCh: make(chan DataEvent, 8), level: LevelInfo}
	m.Write([]byte("history"))
	received := make(chan string, 8)
	cancel := m.OnLogData(func(data []byte) { received <- string(data) })
	defer cancel()
	m.Write([]byte("live"))
	close(m.broadcastCh)
	m.broadcastLoop()
	select {
	case got := <-received:
		if got != "live" {
			t.Fatalf("new subscriber received %q, want live only", got)
		}
	case <-time.After(time.Second):
		t.Fatal("new subscriber received no live log")
	}
	if got := string(m.GetHistory()); got != "historylive" {
		t.Fatalf("stored history changed: %q", got)
	}
}
