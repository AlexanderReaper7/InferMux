package e2e

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// load-check.sh refuses a run while a card is busy, a card's reading is not a
// number, or nvidia-smi fails, and passes one otherwise (sol, review 18, M2).
// It runs with a fake nvidia-smi, and a sleep that returns at once, in a
// directory first on PATH, and with coreutils', so a check that uses sort or
// tail is judged on what it does. No directory on PATH holds the real
// nvidia-smi, so the test reads no card.
func TestTheLoadCheckRefusesABusyOrUnreadCard(t *testing.T) {
	sort, err := exec.LookPath("sort")
	if err != nil {
		t.Fatal(err)
	}
	if sort, err = filepath.EvalSymlinks(sort); err != nil {
		t.Fatal(err)
	}
	coreutils := filepath.Dir(sort)
	if _, err := os.Stat(filepath.Join(coreutils, "nvidia-smi")); err == nil {
		t.Fatalf("%s holds nvidia-smi", coreutils)
	}
	script, err := filepath.Abs("load-check.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		// outs is what each call prints, the last for the calls after it;
		// nil is no nvidia-smi.
		outs  []string
		exit  int
		calls int
		// refusal is part of the check's error, or "" when it passes.
		refusal string
	}{
		{"N/A beside 89", []string{"N/A\n89\n"}, 0, 1, "load as 'N/A'"},
		{"89 beside N/A", []string{"89\nN/A\n"}, 0, 1, "load as 'N/A'"},
		{"[N/A] alone", []string{"[N/A]\n"}, 0, 1, "load as '[N/A]'"},
		{"no output", []string{""}, 0, 1, "load as ''"},
		{"a failed nvidia-smi that printed 50", []string{"50\n"}, 9, 1, "nvidia-smi failed"},
		{"a failed nvidia-smi that printed nothing", []string{""}, 9, 1, "nvidia-smi failed"},
		{"100", []string{"100\n"}, 0, 1, "at 100%"},
		{"90", []string{"90\n"}, 0, 1, "at 90%"},
		{"95 beside 10", []string{"10\n95\n"}, 0, 1, "at 95%"},
		{"99 at the fourth sample", []string{"10\n", "10\n", "10\n", "99\n", "10\n"}, 0, 4, "at 99%"},
		{"89", []string{"89\n"}, 0, 5, ""},
		{"89 beside 12", []string{"89\n12\n"}, 0, 5, ""},
		{"no nvidia-smi", nil, 0, 0, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, filepath.Join(dir, "sleep"), "#!/bin/sh\n")
			if c.outs != nil {
				write(t, filepath.Join(dir, "nvidia-smi"), fakeSMI(dir, c.outs, c.exit))
			}
			cmd := exec.Command(script)
			cmd.Env = []string{"PATH=" + dir + ":" + coreutils}
			var stderr strings.Builder
			cmd.Stderr = &stderr
			err := cmd.Run()
			calls := 0
			if raw, err := os.ReadFile(filepath.Join(dir, "calls")); err == nil {
				calls, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
			}
			if c.refusal == "" {
				if err != nil || stderr.Len() > 0 || calls != c.calls {
					t.Errorf("%v, %q, %d calls; want a pass after %d", err, stderr.String(), calls, c.calls)
				}
				return
			}
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(stderr.String(), c.refusal) || calls != c.calls {
				t.Errorf("%v, %q, %d calls; want exit 1 with %q after %d", err, stderr.String(), calls, c.refusal, c.calls)
			}
		})
	}
}

// fakeSMI is an nvidia-smi that counts its calls in dir and prints outs[i] on
// the i-th, the last on the calls after it, then exits with exit.
func fakeSMI(dir string, outs []string, exit int) string {
	calls := filepath.Join(dir, "calls")
	var b strings.Builder
	fmt.Fprintf(&b, "#!/bin/sh\nn=0\n[ -f %s ] && read -r n < %s\nn=$((n+1))\necho $n > %s\ncase $n in\n", calls, calls, calls)
	for i, out := range outs {
		pattern := strconv.Itoa(i + 1)
		if i == len(outs)-1 {
			pattern = "*"
		}
		fmt.Fprintf(&b, "%s) printf '%%s' '%s' ;;\n", pattern, out)
	}
	fmt.Fprintf(&b, "esac\nexit %d\n", exit)
	return b.String()
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}
