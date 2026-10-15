package muxui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/placement"
)

// The UI loads its host's part with the daemon's environment file, which
// holds the clouds' keys, so the check reads each ${env.VAR} as the daemon
// does (the user's decision 9 of 2026-10-15; 0022, 11). The file's variables
// never enter this process's environment, so nothing the UI starts, git, nix
// or the passphrase command, inherits them: each load runs in a child,
// infermux-ui itself started with loadShimArg, whose environment is the
// file's variables and nothing of the UI's session. The argument, not the
// environment, makes the child the loader, so no variable of the file
// changes what it runs (sol, review 15, L4). The file is read at each check,
// so a new key needs no restart.

const loadShimArg = "--infermux-ui-loads-a-part"

// loadTimeout bounds a load the child does not finish.
const loadTimeout = 30 * time.Second

// LoadShim makes this process load a part, read from stdin, with the base
// config named by the argument after loadShimArg, when the UI started it for
// a check, and returns at once otherwise. Call it first in main, before
// GPGShim, which the file's variables could otherwise start. It exits 0 when
// the part loads, and 1 with llama-swap's error on stdout when not.
func LoadShim() {
	if len(os.Args) < 2 || os.Args[1] != loadShimArg {
		return
	}
	part, err := io.ReadAll(os.Stdin)
	if err == nil && len(os.Args) != 3 {
		err = errors.New("infermux-ui as the loader takes the base config, and only it")
	}
	if err == nil {
		err = (&placement.Part{LlamaSwap: part}).LoadsWith(os.Args[2], true)
	}
	if err != nil {
		fmt.Print(err)
		os.Exit(1)
	}
	os.Exit(0)
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ReadEnvFile reads a systemd EnvironmentFile, as the daemon's unit loads it,
// in the one form that systemd reads as written and whose values an error
// prints as written: NAME=VALUE lines, blank lines and lines that begin with
// # or ;. A value is printable ASCII: Go's %q and %+q print any other byte
// escaped, and redact finds only the characters of a value printed as
// written (sol, review 15, H1). It has no quote or backslash, which systemd
// unquotes and llama-swap escapes, no $, which llama-swap can expand as a
// macro once the value is in the config, and no space at either end, which
// systemd trims. Any other line, a NUL byte, for which systemd refuses the
// file, and a carriage return, which it reads as a line break, refuse the
// file. testdata/systemd-env holds systemd's own reading of each case the
// test compares. An error names the line, never its value.
func ReadEnvFile(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if i := bytes.IndexByte(raw, 0); i >= 0 {
		return nil, fmt.Errorf("%s, line %d: a NUL byte, for which systemd refuses the file", path, bytes.Count(raw[:i], []byte("\n"))+1)
	}
	env := map[string]string{}
	for i, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.Contains(line, "\r"):
			return nil, fmt.Errorf("%s, line %d: a carriage return, which systemd reads as a line break", path, i+1)
		case line == "" || line[0] == '#' || line[0] == ';':
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok || !envName.MatchString(name) {
			return nil, fmt.Errorf("%s, line %d: not NAME=VALUE", path, i+1)
		}
		if why := notAsWritten(value); why != "" {
			return nil, fmt.Errorf("%s, line %d: %s's value has %s", path, i+1, name, why)
		}
		if _, twice := env[name]; twice {
			return nil, fmt.Errorf("%s, line %d: %s is set twice", path, i+1, name)
		}
		env[name] = value
	}
	return env, nil
}

// notAsWritten is what in value systemd, llama-swap or an error's quoting
// would not keep as written, or "" for nothing.
func notAsWritten(value string) string {
	for _, c := range []byte(value) {
		switch {
		case c < ' ' || c > '~':
			return "a byte other than printable ASCII, which an error would print escaped, where the check could not hide it"
		case c == '"' || c == '\'' || c == '\\':
			return "a quote or a backslash, which systemd unquotes and llama-swap escapes"
		case c == '$':
			return "a $, which llama-swap can expand as a macro once the value is in the config"
		}
	}
	if strings.Trim(value, " ") != value {
		return "a space at an end, which systemd trims"
	}
	return ""
}

// loads loads part with the base config and the environment file in a child,
// and returns what the child said, llama-swap's error or the Go runtime's,
// through redact: the check's answer goes to whoever reaches the UI.
func (s *Store) loads(part *placement.Part) error {
	env := map[string]string{}
	if s.EnvFile != "" {
		var err error
		if env, err = ReadEnvFile(s.EnvFile); err != nil {
			return err
		}
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, loadShimArg, s.BaseConfig)
	// Not nil, which would hand the child the UI's own environment.
	cmd.Env = []string{}
	for name, value := range env {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	cmd.Stdin = bytes.NewReader(part.LlamaSwap)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return fmt.Errorf("the load did not finish in %s", loadTimeout)
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return errors.New(redact(stdout.String(), env))
	default:
		return fmt.Errorf("the load failed: %v %s", err, redact(strings.TrimSpace(stderr.String()), env))
	}
}

// redactRun is the shortest run of a value's characters that redact puts
// back. yaml.v3's type error keeps 7 characters of a value, `sk-or-v...`.
const redactRun = 6

// redact puts ${env.NAME} in place of each run of redactRun or more
// characters of s that is part of NAME's value, at each place the longest
// run of any value. A run and not the whole value, because what the child
// prints is the value as YAML left it, with a comment or a tag cut off (sol,
// review 16, H1). Every value of the file, and not only those the config
// reads, because the Go runtime reads a variable such as GOMEMLIMIT before
// LoadShim and prints the one it refuses. So no redactRun characters in a
// row of a value reach the answer, outside the ${env.NAME} put in for them.
// A value shorter than redactRun is not looked for, and does not replace
// the same letters in a path.
func redact(s string, env map[string]string) string {
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for i := 0; i < len(s); {
		name, run := "", 0
		for _, n := range names {
			l := 0
			for i+l < len(s) && strings.Contains(env[n], s[i:i+l+1]) {
				l++
			}
			if l > run {
				name, run = n, l
			}
		}
		if run >= redactRun {
			b.WriteString("${env." + name + "}")
			i += run
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
