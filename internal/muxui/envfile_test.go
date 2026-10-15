package muxui

import (
	"encoding/hex"
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// envFile writes content as an EnvironmentFile and returns its path.
func envFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "infermux.env")
	if err := os.WriteFile(path, []byte(content), 0o400); err != nil {
		t.Fatal(err)
	}
	return path
}

func checkMessage(u *configUI, files map[string]string) string {
	_, out := u.call("POST", "/api/config/check", map[string]any{"files": files})
	p, _ := out["error"].(map[string]any)
	if p == nil {
		return ""
	}
	return p["message"].(string)
}

// The check reads ${env.VAR} from the daemon's environment file and from
// nothing else, as the daemon does (the user's decision 9 of 2026-10-15).
func TestCheckLoadsWithTheDaemonsEnvironmentFile(t *testing.T) {
	u := newConfigUI(t)
	files := draft(u.f.gguf, "clouds:\n  c:\n    url: https://c.example\n    api_key: ${env.INFERMUX_TEST_KEY}\n    models: [m]\n")
	const unset = "environment variable 'INFERMUX_TEST_KEY' is not set"
	if got := checkMessage(&u, files); !strings.Contains(got, unset) {
		t.Errorf("with no environment file: %q, want %q", got, unset)
	}
	u.f.store.EnvFile = envFile(t, "# the clouds' keys\nOTHER=x\n")
	if got := checkMessage(&u, files); !strings.Contains(got, unset) {
		t.Errorf("with a file that lacks it: %q, want %q", got, unset)
	}
	// The UI's own environment is not the daemon's.
	t.Setenv("INFERMUX_TEST_KEY", "sk-session")
	if got := checkMessage(&u, files); !strings.Contains(got, unset) {
		t.Errorf("with the variable in the UI's environment only: %q, want %q", got, unset)
	}
	// Nor with no file or an empty one, where the child has no variable.
	for _, file := range []string{"", envFile(t, "")} {
		u.f.store.EnvFile = file
		if got := checkMessage(&u, files); !strings.Contains(got, unset) {
			t.Errorf("with the variable in the UI's environment and the file %q: %q, want %q", file, got, unset)
		}
	}
	os.Unsetenv("INFERMUX_TEST_KEY")
	u.f.store.EnvFile = envFile(t, "OTHER=x\nINFERMUX_TEST_KEY=sk-test\n")
	if got := checkMessage(&u, files); got != "" {
		t.Errorf("with a file that holds it: %q", got)
	}
	if v, ok := os.LookupEnv("INFERMUX_TEST_KEY"); ok {
		t.Errorf("the check left INFERMUX_TEST_KEY=%q in the UI's environment, which git and nix inherit", v)
	}
	u.f.store.EnvFile = envFile(t, "INFERMUX_TEST_KEY='sk-test'\n")
	if got := checkMessage(&u, files); !strings.Contains(got, "line 1: INFERMUX_TEST_KEY's value has a quote") || strings.Contains(got, "sk-test") {
		t.Errorf("with a file systemd reads differently: %q", got)
	}
}

// llama-swap quotes a value it refuses, and the check's answer reaches
// whoever reaches the UI, so each value of the file goes back to its name.
func TestACheckErrorNamesTheVariableNotItsValue(t *testing.T) {
	const key = "sk-or-v1-deadbeefcafe"
	alias := "aliases: [\"${env.INFERMUX_TEST_KEY}\"]\n"
	for name, c := range map[string]struct {
		files func(map[string]string)
		want  string
	}{
		"a duplicate alias": {func(files map[string]string) {
			files["infermux.yaml"] = strings.Replace(files["infermux.yaml"], "balance:\n", "balance:\n  other: [here/gpu]\n", 1)
			files["models/other.yaml"] = strings.Replace(files["models/qwen.yaml"], "files:", alias+"files:", 1)
			files["models/qwen.yaml"] += alias
		}, "duplicate alias ${env.INFERMUX_TEST_KEY} found"},
		"a proxy URL": {func(files map[string]string) {
			files["infermux.yaml"] += "clouds:\n  a:\n    url: http://[${env.INFERMUX_TEST_KEY}\n    models: [m]\n"
		}, "[${env.INFERMUX_TEST_KEY}"},
	} {
		t.Run(name, func(t *testing.T) {
			u := newConfigUI(t)
			// A variable nothing reads is left out of the second load, and
			// named only where the answer says so.
			u.f.store.EnvFile = envFile(t, "UNREAD=x\nINFERMUX_TEST_KEY="+key+"\n")
			files := draft(u.f.gguf, "")
			c.files(files)
			got := checkMessage(&u, files)
			if strings.Contains(got, "sk-or") || !strings.Contains(got, c.want) || strings.Contains(got, "${env.UNREAD}") {
				t.Errorf("%q, want %q in it, and neither the key nor ${env.UNREAD}", got, c.want)
			}
		})
	}
}

func TestReadEnvFileReadsOnlyWhatSystemdReadsAsWritten(t *testing.T) {
	got, err := ReadEnvFile(envFile(t, "# a comment\n; another\n\nA=1\nB=x=y\nC=\nD_2=sk-or-v1-x"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": "1", "B": "x=y", "C": "", "D_2": "sk-or-v1-x"}
	if len(got) != len(want) {
		t.Errorf("%v, want %v", got, want)
	}
	for name, value := range want {
		if got[name] != value {
			t.Errorf("%s=%q, want %q", name, got[name], value)
		}
	}
	for _, line := range []string{
		`A="secret"`, `A='secret'`, `A=sec\ret`, `A=$secret`, `A=secret `, ` A=secret`, "A=secret\r",
		"export A=secret", "A secret", "1A=secret", "A=secret\nA=again", "A=\nA=secret",
		"A=sec\tret", "A=sec\x01ret", "A=sec\x7fret", "A=sec\u00e9ret", "A=sec\xffret", "A=sec\x00ret",
		"A=one\rB=secret", "# a comment\rA=secret", "; another\rA=secret",
	} {
		_, err := ReadEnvFile(envFile(t, line+"\n"))
		if err == nil {
			t.Errorf("%q read", line)
		} else if strings.Contains(err.Error(), "secret") {
			t.Errorf("%q: the error %q holds the value", line, err)
		}
	}
}

// testdata/systemd-env/cases.json is systemd's own reading of each file in
// probe.py's list, through the load_env_file behind a unit's
// EnvironmentFile. What ReadEnvFile reads, it reads as systemd does, and each
// value it reads Go's %q and %+q print unchanged (sol, review 15, H1 and M3).
// What it refuses, systemd may read another way.
func TestReadEnvFileReadsAsSystemdDoes(t *testing.T) {
	raw, err := os.ReadFile("testdata/systemd-env/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Systemd string
		Cases   []struct {
			File string
			RC   int
			Vars []string
		}
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	read := 0
	for _, c := range record.Cases {
		file, _ := hex.DecodeString(c.File)
		got, err := ReadEnvFile(envFile(t, string(file)))
		if err != nil {
			continue
		}
		read++
		want := map[string]string{}
		for _, v := range c.Vars {
			b, _ := hex.DecodeString(v)
			name, value, _ := strings.Cut(string(b), "=")
			want[name] = value
		}
		if c.RC != 0 || !maps.Equal(got, want) {
			t.Errorf("%q: read as %q, and %s reads %q (%d)", file, got, record.Systemd, want, c.RC)
		}
		for name, value := range got {
			if q := strconv.Quote(value); q[1:len(q)-1] != value {
				t.Errorf("%q: %%q prints %s's value as %s", file, name, q)
			}
			if q := strconv.QuoteToASCII(value); q[1:len(q)-1] != value {
				t.Errorf("%q: %%+q prints %s's value as %s", file, name, q)
			}
		}
	}
	// And the rule refuses no more than it says.
	for c := byte(' '); c <= '~'; c++ {
		if strings.IndexByte("\"'\\$", c) >= 0 {
			continue
		}
		if _, err := ReadEnvFile(envFile(t, "A=x"+string(c)+"y\n")); err != nil {
			t.Errorf("%q refused: %v", c, err)
		}
	}
	if read < 95 {
		t.Errorf("%d of %d cases read", read, len(record.Cases))
	}
}

// A value an error would print escaped refuses the file, and no part of it
// reaches the check's answer: net/url quotes a URL it refuses, and a tab in
// it becomes \t (sol's TestSolR15QuotedKeyMustNotReachHTTP, review 15, H1).
func TestAValueAnErrorWouldPrintEscapedNeverReachesTheCheck(t *testing.T) {
	u := newConfigUI(t)
	u.f.store.EnvFile = envFile(t, "INFERMUX_TEST_KEY=sk-or-v1-a\tbsecret\n")
	files := draft(u.f.gguf, "")
	files["infermux.yaml"] += "clouds:\n  a:\n    url: http://[${env.INFERMUX_TEST_KEY}\n    models: [m]\n"
	got := checkMessage(&u, files)
	if strings.Contains(got, "sk-or-v1-a") || strings.Contains(got, "bsecret") || !strings.Contains(got, "line 1: INFERMUX_TEST_KEY's value has a byte other than printable ASCII") {
		t.Fatalf("%q", got)
	}
}

// A variable of the file never chooses what the loader child runs: the
// argument makes it the loader, and the GPG shim's variable, set in the
// file, is not looked at (sol, review 15, L4).
func TestTheFileCannotChooseWhatTheLoaderRuns(t *testing.T) {
	ok, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	files := func(u configUI) map[string]string {
		return draft(u.f.gguf, "clouds:\n  c:\n    url: https://c.example\n    api_key: ${env.INFERMUX_TEST_UNSET_KEY}\n    models: [m]\n")
	}
	const unset = "environment variable 'INFERMUX_TEST_UNSET_KEY' is not set"
	for _, file := range []string{"INFERMUX_UI_GPG=" + ok + "\n", "INFERMUX_UI_LOAD=\n", "INFERMUX_UI_LOAD=1\n"} {
		u := newConfigUI(t)
		u.f.store.EnvFile = envFile(t, file)
		if got := checkMessage(&u, files(u)); !strings.Contains(got, unset) {
			t.Errorf("with %q: %q, want %q", file, got, unset)
		}
	}
}

// leakRun is the shortest run of a value's characters the check's answer may
// not hold. yaml.v3's type error keeps 7.
const leakRun = 6

// runOf is the first run of leakRun characters of value in s outside each
// ${env.NAME}, or "".
func runOf(s, value string) string {
	for _, part := range regexp.MustCompile(`\$\{env\.[A-Za-z0-9_]+\}`).Split(s, -1) {
		for i := 0; i+leakRun <= len(part); i++ {
			if strings.Contains(value, part[i:i+leakRun]) {
				return part[i : i+leakRun]
			}
		}
	}
	return ""
}

// The check's answer holds no run of leakRun characters of a value of the
// file, whatever YAML made of the value before llama-swap quoted it, a
// comment or a tag cut off (sol's TestSolR16ChangedValue, review 16, H1),
// whatever the Go runtime printed of a variable it refused before the loader
// started (sol's TestSolR16RuntimeEnv), and whatever yaml.v3's type error
// keeps of a string in an int field, which the second load's number fills.
func TestACheckAnswerHoldsNoRunOfAValue(t *testing.T) {
	block := "aliases:\n  - ${env.INFERMUX_TEST_KEY}\n"
	for _, c := range []struct{ value, alias string }{
		{"sk-or-v1-abcdefgh # rest", block},
		{"!!str sk-or-v1-abcdefghijklmnop", block},
		{"sk-or-v1-abcdef, moresecret", "aliases: [\"${env.INFERMUX_TEST_KEY}\"]\n"},
		{"sk-or-v1-abcdefgh%zzrest", "aliases: [\"${env.INFERMUX_TEST_KEY}\"]\n"},
	} {
		u := newConfigUI(t)
		u.f.store.EnvFile = envFile(t, "INFERMUX_TEST_KEY="+c.value+"\n")
		files := draft(u.f.gguf, "")
		files["infermux.yaml"] = strings.Replace(files["infermux.yaml"], "balance:\n", "balance:\n  other: [here/gpu]\n", 1)
		files["models/other.yaml"] = strings.Replace(files["models/qwen.yaml"], "files:", c.alias+"files:", 1)
		files["models/qwen.yaml"] += c.alias
		got := checkMessage(&u, files)
		if !strings.Contains(got, "duplicate alias ${env.INFERMUX_TEST_KEY} found in model: ") || runOf(got, c.value) != "" {
			t.Errorf("%q as %q: %q", c.value, c.alias, got)
		}
	}

	u := newConfigUI(t)
	const value = "sk-or-v1-madeup-runtime-secret"
	u.f.store.EnvFile = envFile(t, "GOMEMLIMIT="+value+"\n")
	got := checkMessage(&u, draft(u.f.gguf, ""))
	if !strings.Contains(got, "loads with a stand-in for each and without GOMEMLIMIT, which nothing reads, so a value fails it; its error is not shown") || runOf(got, value) != "" {
		t.Errorf("a variable the Go runtime refuses: %q", got)
	}

	u = newConfigUI(t)
	u.f.store.EnvFile = envFile(t, "INFERMUX_TEST_KEY="+value+"\n")
	files := draft(u.f.gguf, "")
	files["models/qwen.yaml"] += "concurrencyLimit: ${env.INFERMUX_TEST_KEY}\n"
	got = checkMessage(&u, files)
	if !strings.Contains(got, "loads with a stand-in for each, so a value fails it; its error is not shown") || runOf(got, value) != "" {
		t.Errorf("a type error, which yaml.v3 prints with 7 characters of the value: %q", got)
	}
}

// The second load has a stand-in for each value it reads, whatever YAML
// reads the value as alone: where the part puts it, the value can be text, as
// `300 # rest` is in a quoted alias (sol's TestSolR17TypedCommentLeaks,
// review 17, H1).
func TestTheSecondLoadHasNoValueOfTheFile(t *testing.T) {
	quoted := "aliases: [\"${env.INFERMUX_TEST_KEY}\"]\n"
	for _, value := range []string{
		"300 # sk-or-v1-hidden-suffix", "1.5 # sk-or-v1-hidden-suffix", "true # sk-or-v1-hidden-suffix",
		"~ # sk-or-v1-hidden-suffix", "0x1F", "false",
	} {
		u := newConfigUI(t)
		u.f.store.EnvFile = envFile(t, "INFERMUX_TEST_KEY="+value+"\n")
		files := draft(u.f.gguf, "")
		files["infermux.yaml"] = strings.Replace(files["infermux.yaml"], "balance:\n", "balance:\n  other: [here/gpu]\n", 1)
		files["models/other.yaml"] = strings.Replace(files["models/qwen.yaml"], "files:", quoted+"files:", 1)
		files["models/qwen.yaml"] += quoted
		got := checkMessage(&u, files)
		if !strings.Contains(got, "duplicate alias ${env.INFERMUX_TEST_KEY} found in model: ") || strings.Contains(got, "hidden-suffix") || runOf(got, value) != "" {
			t.Errorf("%q: %q", value, got)
		}
	}
}

// A stand-in is a number, so a field such as ttl reads it (sol, review 12),
// and the answer is the error the first load had: a duplicate alias, which
// llama-swap finds after it has decoded the fields. A bool field fails for
// it, and the type error shows the stand-in whole, as the variable's name.
func TestAStandInIsANumber(t *testing.T) {
	for _, c := range []struct{ field, value, want string }{
		{"ttl", "300", "duplicate alias dup found in model: "},
		{"ttl", "0x1F", "duplicate alias dup found in model: "},
		{"ttl", "~", "duplicate alias dup found in model: "},
		{"ttl", "", "duplicate alias dup found in model: "},
		{"unlisted", "true", "cannot unmarshal !!int `${env.INFERMUX_TEST_TYPED}` into bool"},
	} {
		u := newConfigUI(t)
		u.f.store.EnvFile = envFile(t, "INFERMUX_TEST_TYPED="+c.value+"\nINFERMUX_TEST_KEY=sk-or-v1-0123456789\n")
		files := draft(u.f.gguf, "")
		files["infermux.yaml"] = strings.Replace(files["infermux.yaml"], "balance:\n", "balance:\n  other: [here/gpu]\n", 1)
		files["models/other.yaml"] = strings.Replace(files["models/qwen.yaml"], "files:", "aliases: [dup]\nfiles:", 1)
		files["models/qwen.yaml"] += c.field + ": ${env.INFERMUX_TEST_TYPED}\nname: ${env.INFERMUX_TEST_KEY}\naliases: [dup]\n"
		if got := checkMessage(&u, files); !strings.Contains(got, c.want) || strings.Contains(got, "sk-or") {
			t.Errorf("%s: %q: %q, want %q in it", c.field, c.value, got, c.want)
		}
	}
}

// What the check answers does not depend on whether text in the draft is part
// of a key: a guess written into the draft comes back as written, whatever
// part of the key it is, so that guessing cannot find the key a run at a
// time. The key is read by the part, and by the base.
func TestACheckIsNoOracleForAKey(t *testing.T) {
	const key = "sk-or-v1-0123456789abcdef"
	for _, reader := range []string{"part", "base"} {
		answers := map[string]bool{}
		for _, guess := range []string{key[:12], key[4:16], key[9:], "sk-or-v1-fedcba98", "zzzzzzzzzzzz"} {
			u := newConfigUI(t)
			u.f.store.EnvFile = envFile(t, "INFERMUX_TEST_KEY="+key+"\nUNREAD=sk-or-v1-0123456789\n")
			cloud := "clouds:\n  a:\n    url: http://[" + guess + "\n    models: [m]\n"
			if reader == "part" {
				cloud += "  b:\n    url: https://b.example\n    api_key: ${env.INFERMUX_TEST_KEY}\n    models: [n]\n"
			} else {
				if err := os.WriteFile(u.f.store.BaseConfig, []byte(base+"apiKeys: [\"${env.INFERMUX_TEST_KEY}\"]\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got := checkMessage(&u, draft(u.f.gguf, cloud))
			if !strings.Contains(got, "["+guess) || strings.Contains(got, "not shown") {
				t.Errorf("read by the %s, %q: %q", reader, guess, got)
			}
			answers[strings.ReplaceAll(got, guess, "GUESS")] = true
		}
		if len(answers) != 1 {
			t.Errorf("read by the %s, the answers differ with the guess: %q", reader, slices.Collect(maps.Keys(answers)))
		}
	}
}
