package muxui

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// A host with no desktop session has nowhere to show gpg's pinentry, so
// Commit there takes the passphrase of the user's GPG key from the browser
// and hands it to gpg in loopback mode (0008). It travels on a pipe at fd 3,
// which git passes on to its gpg.program, and never in an argument, the
// environment or a file. gpg-agent caches it for its own cache TTL, as it
// would after a pinentry.
//
// git runs gpg.program with its own arguments, so the program is infermux-ui
// itself, started with gpgShimEnv naming the real gpg: GPGShim turns that
// process into gpg with the loopback flags added.

const gpgShimEnv = "INFERMUX_UI_GPG"

// ErrGPGPassphrase is a Commit, on a UI that signs with the passphrase,
// asked without it.
var ErrGPGPassphrase = errors.New("a commit here needs the passphrase of your GPG key")

// GPGShim makes this process gpg when git started it as gpg.program for a
// Commit, and returns at once otherwise. Call it first in main, before flags
// are parsed: the arguments are git's, for gpg.
func GPGShim() {
	gpg := os.Getenv(gpgShimEnv)
	if gpg == "" {
		return
	}
	args := append([]string{gpg, "--batch", "--pinentry-mode", "loopback", "--passphrase-fd", "3"}, os.Args[1:]...)
	err := syscall.Exec(gpg, args, os.Environ())
	fmt.Fprintln(os.Stderr, "infermux-ui as gpg:", err)
	os.Exit(2)
}

// signWith prepares cmd, a git commit, to sign with passphrase. The returned
// function closes this process's end of the pipe once cmd has run.
func (s *Store) signWith(cmd *exec.Cmd, passphrase string) (func(), error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	gpg := s.GPG
	if gpg == "" {
		gpg = "gpg"
	}
	if gpg, err = exec.LookPath(gpg); err != nil {
		return nil, err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	// A passphrase is far below the pipe's buffer, so this does not block.
	_, err = w.WriteString(passphrase)
	w.Close()
	if err != nil {
		r.Close()
		return nil, err
	}
	cmd.Args = append([]string{cmd.Args[0], "-c", "gpg.program=" + self}, cmd.Args[1:]...)
	cmd.Env = append(os.Environ(), gpgShimEnv+"="+gpg)
	cmd.ExtraFiles = []*os.File{r}
	return func() { r.Close() }, nil
}
