package muxui

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const openrouter = `# The cloud models every key may use (0006, 6).
peers:
  openrouter:
    proxy: https://openrouter.ai/api
    apiKey: ${env.INFERMUX_TEST_OPENROUTER_KEY}
    models:
      - openrouter/free
`

func TestAPeersModelListIsEditedAndItsKeyStaysTheDaemons(t *testing.T) {
	f := newFixture(t)
	os.WriteFile(filepath.Join(f.store.ModelsDir, "openrouter.yaml"), []byte(openrouter), 0o644)

	peers, err := f.store.Peers()
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || peers[0].Name != "openrouter" || peers[0].Proxy != "https://openrouter.ai/api" ||
		strings.Join(peers[0].Models, ",") != "openrouter/free" || peers[0].File != "openrouter.yaml" {
		t.Fatalf("%+v", peers)
	}

	// The UI has no INFERMUX_TEST_OPENROUTER_KEY, and the check still passes.
	if err := f.store.SavePeerModels("openrouter", []string{"openrouter/free", " qwen/qwen3.8-27b:free "}); err != nil {
		t.Fatal(err)
	}
	got := f.file(t, "openrouter.yaml")
	for _, want := range []string{"# The cloud models", "apiKey: ${env.INFERMUX_TEST_OPENROUTER_KEY}", "- openrouter/free\n", "- qwen/qwen3.8-27b:free\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in\n%s", want, got)
		}
	}
	// A model's save is checked against the peer file too, and against a base
	// config that names the daemon's environment.
	raw, _ := os.ReadFile(f.store.BaseConfig)
	os.WriteFile(f.store.BaseConfig, []byte(strings.Replace(string(raw), "macros:\n", "macros:\n  token: ${env.INFERMUX_TEST_BASE}\n", 1)), 0o644)
	m := f.model(t, "qwen")
	m.Description = "everyday"
	if err := f.store.SaveModel("qwen", m); err != nil {
		t.Fatal(err)
	}

	for _, bad := range [][]string{{"a", "a"}, {"a", " "}} {
		if err := f.store.SavePeerModels("openrouter", bad); err == nil {
			t.Fatalf("%q was taken", bad)
		}
	}
	if f.file(t, "openrouter.yaml") != got {
		t.Fatal("a refused list was written")
	}
	if err := f.store.SavePeerModels("nobody", []string{"a"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestAPeerFileSharedByASymlinkStaysOneFileAndIsCommitted(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	f := newFixture(t)
	repo := filepath.Dir(f.store.ModelsDir)
	shared := filepath.Join(repo, "shared", "openrouter.yaml")
	os.MkdirAll(filepath.Dir(shared), 0o755)
	os.WriteFile(shared, []byte(openrouter), 0o644)
	link := filepath.Join(f.store.ModelsDir, "openrouter.yaml")
	if err := os.Symlink("../shared/openrouter.yaml", link); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
		return string(out)
	}
	git("init", "-q")
	git("add", "-A")
	git("commit", "-qm", "start")

	if err := f.store.SavePeerModels("openrouter", []string{"google/gemma-4-31b-it:free"}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the link was replaced by a copy")
	}
	if raw, _ := os.ReadFile(shared); !strings.Contains(string(raw), "- google/gemma-4-31b-it:free") {
		t.Fatalf("the shared file was not written:\n%s", raw)
	}

	st, err := f.store.Git()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(st.Diff, "+      - google/gemma-4-31b-it:free") {
		t.Fatalf("the shared file's change is not shown: %+v", st)
	}
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")
	if _, err := f.store.Commit("openrouter: gemma", ""); err != nil {
		t.Fatal(err)
	}
	if files := git("show", "--name-only", "--format="); strings.TrimSpace(files) != "shared/openrouter.yaml" {
		t.Fatalf("committed %q", files)
	}
}
