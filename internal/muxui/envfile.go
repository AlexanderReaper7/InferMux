package muxui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"regexp"
	"slices"
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
// in the one form that systemd reads as written: NAME=VALUE lines, blank
// lines and lines that begin with # or ;. A value is printable ASCII, which
// the check's redaction needed until sol's review 16, since an error prints
// any other byte escaped (sol, review 15, H1); the check now answers with a
// load that never had the values (loads). It has no quote or backslash, which
// systemd unquotes and llama-swap escapes, no $, which llama-swap can expand
// as a macro once the value is in the config, and no space at either end,
// which systemd trims. Any other line, a NUL byte, for which systemd refuses the
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
			return "a byte other than printable ASCII"
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

// loads loads part with the base config and the environment file in a child.
// The check's answer goes to whoever reaches the UI, and what the child says
// can hold a value of the file: llama-swap quotes a value it refuses, such as
// a duplicate alias or a proxy URL, as YAML left it, with a comment or a tag
// cut off, and the Go runtime prints a variable such as GOMEMLIMIT that it
// refuses before LoadShim runs (sol, review 16, H1). So the answer is never
// what that load said. When it fails, part is loaded again with a stand-in for
// each value the part or the base reads and without the file's other
// variables, and the answer is what the second load said, each stand-in shown
// as the ${env.NAME} it stands for. The first load adds only that it failed.
// The second load never has a value of the file, whatever YAML reads the value
// as alone: where the part puts it, it can be text, as `300 # rest` is in a
// quoted alias (sol, review 17, H1). Nothing in the answer is chosen by
// comparing the draft's text with a value, which would tell whoever writes a
// guess into the draft whether the guess is part of a key.
func (s *Store) loads(part *placement.Part) error {
	env := map[string]string{}
	if s.EnvFile != "" {
		var err error
		if env, err = ReadEnvFile(s.EnvFile); err != nil {
			return err
		}
	}
	failed, err := s.load(part, env)
	if err != nil || failed == "" {
		return err
	}
	base, _ := os.ReadFile(s.BaseConfig) // unreadable, both loads say so
	stood := map[string]string{}
	var shown, unread []string
	for _, name := range slices.Sorted(maps.Keys(env)) {
		if !reads(name, part.LlamaSwap, base) {
			unread = append(unread, name)
			continue
		}
		stand := standIn(len(shown) / 2)
		stood[name] = stand
		shown = append(shown, stand, "${env."+name+"}")
	}
	again, err := s.load(part, stood)
	if err != nil {
		return err
	}
	if again != "" {
		return errors.New(strings.NewReplacer(shown...).Replace(again))
	}
	without := ""
	if len(unread) > 0 {
		without = " and without " + strings.Join(unread, ", ") + ", which nothing reads"
	}
	return fmt.Errorf("the load fails with the env file's values and loads with a stand-in for each%s, so a value fails it; its error is not shown, since it can hold the value", without)
}

// standIn is the i-th stand-in: a number of 9 digits, which YAML reads as a
// number where the macro stands alone and as text inside quotes or other
// text, so a field such as ttl reads it (sol, review 12), and which yaml.v3's
// type error prints whole, as it does each value of 10 characters or fewer.
func standIn(i int) string {
	return fmt.Sprintf("7357%02d357", i)
}

// reads is whether the part or the base reads the variable name.
func reads(name string, texts ...[]byte) bool {
	for _, text := range texts {
		for _, m := range placement.EnvMacro.FindAllSubmatch(text, -1) {
			if string(m[1]) == name {
				return true
			}
		}
	}
	return false
}

// load loads part in the loader child, whose whole environment is env, and
// returns "" when it loads, and when not, llama-swap's error or, for an exit
// but 1, the Go runtime's.
func (s *Store) load(part *placement.Part, env map[string]string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
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
		return "", nil
	case ctx.Err() != nil:
		return "", fmt.Errorf("the load did not finish in %s", loadTimeout)
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return stdout.String(), nil
	case errors.As(err, &exit):
		return fmt.Sprintf("the load failed: %v %s", err, strings.TrimSpace(stderr.String())), nil
	default:
		return "", err
	}
}
