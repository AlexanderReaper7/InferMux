// infermux-hold clears this host's card before it starts a command, and hands
// the GPU back to the warden's judgement when the command exits (0024).
//
//	infermux-hold [-socket /run/infermux/control.sock] [-timeout 30s] -- <command>
//
// As a Steam launch option: infermux-hold -- gamescope ... -- %command%
//
// A card that does not come clear within -timeout, because an interactive
// request is still running, refuses the launch: a game started beside a model
// keeps its render targets in system memory for its whole run. A daemon that
// is not running holds nothing, so the command starts with a warning.
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
	timeout := fs.Duration("timeout", 30*time.Second, "how long to wait for the card to come clear before refusing")
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
		refuse(err)
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

// refuse says why on stderr and, since a Steam launch has no terminal, on
// the desktop as well.
func refuse(err error) {
	msg := "not started: " + err.Error()
	warn(msg)
	if notify, lookErr := exec.LookPath("notify-send"); lookErr == nil {
		exec.Command(notify, "--app-name=infermux-hold", "--urgency=critical", "GPU not clear", msg).Run()
	}
	os.Exit(1)
}
