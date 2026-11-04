// infermux-hold clears this host's card before it starts a command, and hands
// the GPU back to the warden's judgement when the command exits (0024).
//
//	infermux-hold [-socket /run/infermux/control.sock] [-timeout 10s] -- <command>
//
//	infermux-hold [-socket ...] [-timeout 10s] -steam ~/.steam/root
//
// As a Steam launch option: infermux-hold -- gamescope ... -- %command%
//
// With -steam it runs no command. It does the same for each game the Steam
// install at that root starts, from the start of the game's reaper, Steam's
// first program of a launch, until its exit, and runs until a signal
// (0024, 6). The module's services.infermux.steam runs it so as a user
// service, and then no game needs a launch option.
//
// The launch always wins over every client (the user, 2026-10-17; 0024, 4):
// the daemon cuts the requests on the GPU, answered 503 gpu_held, and stops
// ComfyUI and the models. Until the command exits the daemon starts no model
// on the GPU, for any client, and answers each start 503 gpu_held (0024, 5).
// The command always starts. A card that is not clear within -timeout, or a
// daemon that cannot be reached, is a warning on stderr and the desktop. A
// daemon that is not running holds nothing.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/mostlygeek/llama-swap/internal/hold"
)

func main() {
	fs := flag.NewFlagSet("infermux-hold", flag.ExitOnError)
	socket := fs.String("socket", "/run/infermux/control.sock", "the daemon's control socket")
	timeout := fs.Duration("timeout", 10*time.Second, "how long to wait for InferMux's VRAM to come back before starting anyway")
	steam := fs.String("steam", "", "instead of running a command, hold the card for each game the Steam install at this root starts")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: infermux-hold [-socket <path>] [-timeout <duration>] -- <command> [args...]")
		fmt.Fprintln(os.Stderr, "       infermux-hold [-socket <path>] [-timeout <duration>] -steam <root>")
		fs.PrintDefaults()
	}
	fs.Parse(os.Args[1:])
	if (*steam == "") == (fs.NArg() == 0) {
		fs.Usage()
		os.Exit(2)
	}
	c := hold.Dial(*socket)
	if *steam != "" {
		os.Exit(holdSteam(c, *steam, *timeout))
	}
	// A signal is passed on to the command, and this process waits for it to
	// exit rather than dying first, so the pause is still handed back.
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	done := take(c, *timeout, "starting")
	cmd := exec.Command(fs.Arg(0), fs.Args()[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		done()
		refuse(err)
	}
	go func() {
		for s := range signals {
			cmd.Process.Signal(s)
		}
	}()
	err := cmd.Wait()
	done()
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
		say(err.Error())
		os.Exit(1)
	}
}

// take holds the card and clears it for a launch, and says what did not
// happen. launch is what goes ahead anyway. done hands the card back.
func take(c *hold.Client, timeout time.Duration, launch string) (done func()) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout+10*time.Second)
	defer cancel()
	// The hold first, so that no model starts on the GPU between the unload
	// and the launch's exit (0024, 5).
	unhold, holdErr := c.Hold(ctx)
	release, err := c.Clear(ctx, timeout)
	switch {
	case errors.Is(err, hold.ErrNotRunning):
		say("InferMux is not running, so it holds no VRAM; " + launch + " anyway")
	case err != nil || holdErr != nil:
		if holdErr != nil {
			err = errors.Join(fmt.Errorf("not held: %w", holdErr), err)
		}
		notify("GPU may not be clear", launch+" anyway: "+err.Error())
	}
	return func() {
		handBack(release)
		unhold()
	}
}

// holdSteam takes the card for each game Steam starts under root, from its
// reaper's start until its exit, until a signal. The card of a game still
// running is handed back before it returns.
func holdSteam(c *hold.Client, root string, timeout time.Duration) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	watching, cancel := context.WithCancel(ctx)
	var games sync.WaitGroup
	say("holding the card for each game Steam starts under " + root)
	err := hold.WatchSteam(watching, root, func(g hold.Game) {
		games.Add(1)
		go func() {
			defer games.Done()
			say(g.String() + " started")
			done := take(c, timeout, g.String()+" runs")
			g.Wait(watching)
			done()
			if watching.Err() == nil {
				say(g.String() + " exited; the card is the warden's again")
			} else {
				say(g.String() + " still runs; the card is the warden's again, since infermux-hold stops")
			}
		}()
	})
	cancel()
	games.Wait()
	if err != nil {
		notify("Steam's games are not held", err.Error())
		return 1
	}
	return 0
}

func handBack(release func(context.Context) error) {
	if release == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := release(ctx); err != nil {
		say("the manual pause was not handed back to the warden: " + err.Error())
	}
}

// say writes msg to stderr, the journal's for the user service.
func say(msg string) {
	fmt.Fprintln(os.Stderr, "infermux-hold:", msg)
}

// notify says msg on stderr and, since a Steam launch has no terminal, on
// the desktop as well.
func notify(title, msg string) {
	say(msg)
	if send, err := exec.LookPath("notify-send"); err == nil {
		exec.Command(send, "--app-name=infermux-hold", "--urgency=critical", title, msg).Run()
	}
}

// refuse is a command that could not be started.
func refuse(err error) {
	notify("Not started", "not started: "+err.Error())
	os.Exit(1)
}
