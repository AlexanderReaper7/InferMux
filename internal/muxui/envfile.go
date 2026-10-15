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
// infermux-ui itself started with loadShimEnv, whose environment is the
// file's variables and nothing of the UI's session. The file is read at each
// check, so a new key needs no restart.

const loadShimEnv = "INFERMUX_UI_LOAD"

// loadTimeout bounds a load the child does not finish.
const loadTimeout = 30 * time.Second

// LoadShim makes this process load a part, read from stdin, with the base
// config named by its one argument, when the UI started it for a check, and
// returns at once otherwise. Call it first in main, beside GPGShim. It exits
// 0 when the part loads, and 1 with llama-swap's error on stdout when not.
func LoadShim() {
	if os.Getenv(loadShimEnv) == "" {
		return
	}
	part, err := io.ReadAll(os.Stdin)
	if err == nil && len(os.Args) != 2 {
		err = errors.New("infermux-ui as the loader takes the base config, and only it")
	}
	if err == nil {
		err = (&placement.Part{LlamaSwap: part}).LoadsWith(os.Args[1], true)
	}
	if err != nil {
		fmt.Print(err)
		os.Exit(1)
	}
	os.Exit(0)
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ReadEnvFile reads a systemd EnvironmentFile, as the daemon's unit loads it,
// in the one form whose values systemd reads as written: NAME=VALUE lines,
// blank lines and lines that begin with # or ;. A value with a quote, a
// backslash, a $ or whitespace at either end is refused, as is any other
// line, since systemd would read it differently. An error names the line,
// never its value.
func ReadEnvFile(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	for i, line := range strings.Split(string(raw), "\n") {
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		switch {
		case !ok || !envName.MatchString(name):
			return nil, fmt.Errorf("%s, line %d: not NAME=VALUE", path, i+1)
		case strings.ContainsAny(value, "\"'\\$") || strings.TrimSpace(value) != value:
			return nil, fmt.Errorf("%s, line %d: %s's value has a quote, a backslash, a $ or whitespace at an end, which systemd does not read as written", path, i+1, name)
		}
		if _, twice := env[name]; twice {
			return nil, fmt.Errorf("%s, line %d: %s is set twice", path, i+1, name)
		}
		env[name] = value
	}
	return env, nil
}

// loads loads part with the base config and the environment file in a child,
// and returns llama-swap's error with each value the part or the base reads
// put back as the ${env.VAR} it came from: llama-swap quotes a value it
// refuses, such as a duplicate alias or a proxy URL, and the check's answer
// goes to whoever reaches the UI. Only those values are pasted into the text,
// so a short value of another variable is not replaced in a path that never
// held it.
func (s *Store) loads(part *placement.Part) error {
	env := map[string]string{}
	if s.EnvFile != "" {
		var err error
		if env, err = ReadEnvFile(s.EnvFile); err != nil {
			return err
		}
	}
	read := map[string]string{}
	base, _ := os.ReadFile(s.BaseConfig) // unreadable, the child's load says so
	for _, raw := range [][]byte{part.LlamaSwap, base} {
		for _, m := range placement.EnvMacro.FindAllSubmatch(raw, -1) {
			if value, ok := env[string(m[1])]; ok {
				read[string(m[1])] = value
			}
		}
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, s.BaseConfig)
	cmd.Env = []string{loadShimEnv + "=1"}
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
		return errors.New(redact(stdout.String(), read))
	default:
		return fmt.Errorf("the load failed: %v %s", err, redact(strings.TrimSpace(stderr.String()), read))
	}
}

// redact puts ${env.NAME} in place of each value of env in s, the longest
// first, so that one value inside another is not left half in place.
func redact(s string, env map[string]string) string {
	names := make([]string, 0, len(env))
	for name, value := range env {
		if value != "" {
			names = append(names, name)
		}
	}
	sort.Slice(names, func(i, j int) bool { return len(env[names[i]]) > len(env[names[j]]) })
	for _, name := range names {
		s = strings.ReplaceAll(s, env[name], "${env."+name+"}")
	}
	return s
}
