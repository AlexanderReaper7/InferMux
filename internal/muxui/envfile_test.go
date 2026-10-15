package muxui

import (
	"os"
	"path/filepath"
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
			// A value of a variable the part does not read stays: "x" is in
			// the load's own temporary path.
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
	} {
		_, err := ReadEnvFile(envFile(t, line+"\n"))
		if err == nil {
			t.Errorf("%q read", line)
		} else if strings.Contains(err.Error(), "secret") {
			t.Errorf("%q: the error %q holds the value", line, err)
		}
	}
}
