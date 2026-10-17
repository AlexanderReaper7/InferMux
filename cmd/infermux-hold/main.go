// infermux-hold clears this host's card before it starts a command, and hands
// the GPU back to the warden's judgement when the command exits (0024).
//
//	infermux-hold [-socket /run/infermux/control.sock] [-timeout 10s] -- <command>
//
// As a Steam launch option: infermux-hold -- gamescope ... -- %command%
//
// The launch always wins over every client (the user, 2026-10-17; 0024, 4):
// the daemon cuts the requests on the GPU, answered 503 gpu_held, and stops
// ComfyUI and the models. The command always starts. A card that is not clear
// within -timeout, or a daemon that cannot be reached, is a warning on stderr
// and the desktop. A daemon that is not running holds nothing.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/mostlygeek/llama-swap/internal/hold"
)

func main() {
	fs := flag.NewFlagSet("infermux-hold", flag.ExitOnError)
	socket := fs.String("socket", "/run/infermux/control.sock", "the daemon's control socket")
	timeout := fs.Duration("timeout", 10*time.Second, "how long to wait for InferMux's VRAM to come back before starting anyway")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: infermux-hold [-socket <path>] [-timeout <duration>] -- <command> [args...]")
		fs.PrintDefaults()
	}
	fs.Parse(os.Args[1:])
	if fs.NArg() == 0 {
		fs.Usage()
		os.Exit(2)
	}
	// A signal is passed on to the command, and this process waits for it to
	// exit rather than dying first, so the pause is still handed back.
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout+10*time.Second)
	release, err := hold.Dial(*socket).Clear(ctx, *timeout)
	cancel()
	switch {
	case errors.Is(err, hold.ErrNotRunning):
		warn("InferMux is not running, so it holds no VRAM; starting anyway")
	case err != nil:
		notify("GPU may not be clear", "starting anyway: "+err.Error())
	}

	cmd := exec.Command(fs.Arg(0), fs.Args()[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		handBack(release)
		refuse(err)
	}
	go func() {
		for s := range signals {
			cmd.Process.Signal(s)
		}
	}()
	err = cmd.Wait()
	handBack(release)
	var exit *exec.ExitError
	switch {
	case err == nil:
		os.Exit(0)
	case errors.As(err, &exit):
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			os.Exit(128 + int(status.Signal()))
		}
		os.Exit(exit.ExitCode())
	default:
		warn(err.Error())
		os.Exit(1)
	}
}

func handBack(release func(context.Context) error) {
	if release == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := release(ctx); err != nil {
		warn("the manual pause was not handed back to the warden: " + err.Error())
	}
}

func warn(msg string) {
	fmt.Fprintln(os.Stderr, "infermux-hold:", msg)
}

// notify says msg on stderr and, since a Steam launch has no terminal, on
// the desktop as well.
func notify(title, msg string) {
	warn(msg)
	if send, err := exec.LookPath("notify-send"); err == nil {
		exec.Command(send, "--app-name=infermux-hold", "--urgency=critical", title, msg).Run()
	}
}

// refuse is a command that could not be started.
func refuse(err error) {
	notify("Not started", "not started: "+err.Error())
	os.Exit(1)
}
