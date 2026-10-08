//go:build !linux

package process

// InferMux: the notify socket needs SO_PASSCRED, which is Linux's (0020).
// Elsewhere a model that opts in fails to start, and every other model is
// polled as before.

import (
	"errors"
	"os/exec"
)

type notifySocket struct{}

func (p *ProcessCommand) listenNotify(cmd *exec.Cmd) (*notifySocket, error) {
	want, err := notifyWanted(p.config)
	if err != nil || !want {
		return nil, err
	}
	return nil, errors.New("metadata.readiness: notify needs Linux")
}

func (n *notifySocket) close() {}

func (n *notifySocket) serve(pid int) <-chan notifyEvent { return nil }
