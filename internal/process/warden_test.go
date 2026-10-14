package process

import (
	"context"
	"errors"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// InferMux 0025: a start the gate refuses answers with the gate's error and
// never begins, so the process stays stopped.
func TestTheStartGateRefusesAStart(t *testing.T) {
	refused := errors.New("the local models are off")
	var asked []string
	SetStartGate(func(id string) error {
		asked = append(asked, id)
		return refused
	})
	t.Cleanup(func() { SetStartGate(nil) })

	// A command that cannot run: a start that began would fail with exec's
	// error, not the gate's.
	p := newProcessCommand(t, config.ModelConfig{Cmd: "/nonexistent/llama-server", Proxy: "http://127.0.0.1:1"})
	err := p.EnsureReady(context.Background(), testStartTimeout)
	if !errors.Is(err, refused) || len(asked) != 1 || asked[0] != t.Name() {
		t.Fatalf("err %v, asked %v", err, asked)
	}
	if p.State() != StateStopped {
		t.Fatalf("state %s", p.State())
	}

	SetStartGate(nil)
	if err := p.EnsureReady(context.Background(), testStartTimeout); err == nil || errors.Is(err, refused) {
		t.Fatalf("without the gate: %v", err)
	}
}
