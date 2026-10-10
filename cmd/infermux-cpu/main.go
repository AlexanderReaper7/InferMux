// infermux-cpu runs a CPU device's model servers in the idle slice, as the
// runner's children rather than the daemon's (0022, internal/cpurun).
//
//	infermux-cpu serve -socket /run/infermux-cpu/socket
//	infermux-cpu run -socket /run/infermux-cpu/socket -- llama-server ...
//
// serve is the runner, a service of its own in the slice. run is a CPU
// model's command in llama-swap's config: it stays in the foreground for as
// long as the server runs, prints its output, and stops it on SIGTERM.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/mostlygeek/llama-swap/internal/cpurun"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "run":
		var code int
		code, err = run(os.Args[2:])
		if err == nil {
			os.Exit(code)
		}
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "infermux-cpu:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: infermux-cpu serve -socket <path> | run -socket <path> -- <command>")
	os.Exit(2)
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	socket := fs.String("socket", "", "where to listen")
	fs.Parse(args)
	if *socket == "" {
		return errors.New("-socket is required")
	}
	// A socket left by a runner that was killed would refuse the bind.
	if err := os.Remove(*socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: *socket, Net: "unix"})
	if err != nil {
		return err
	}
	if err := os.Chmod(*socket, 0o600); err != nil {
		return err
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-stop
		// The servers go with the runner's cgroup when systemd stops it.
		ln.Close()
	}()
	log.Printf("infermux-cpu: serving on %s", *socket)
	return cpurun.Serve(ln, log.Printf)
}

func run(args []string) (int, error) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	socket := fs.String("socket", "", "the runner's socket")
	fs.Parse(args)
	if *socket == "" || fs.NArg() == 0 {
		return 0, errors.New("run -socket <path> -- <command>")
	}
	dir, err := os.Getwd()
	if err != nil {
		return 0, err
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	stop := make(chan struct{})
	go func() {
		<-signals
		close(stop)
	}()
	return cpurun.Run(*socket, cpurun.Request{Argv: fs.Args(), Env: os.Environ(), Dir: dir}, os.Stdout, stop)
}
