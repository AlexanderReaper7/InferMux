package muxui

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A Commit that signs runs this test binary as git's gpg.program.
func TestMain(m *testing.M) {
	GPGShim()
	os.Exit(m.Run())
}

func TestACommitIsSignedWithThePassphraseFromTheBrowser(t *testing.T) {
	for _, bin := range []string{"git", "gpg", "gpgconf"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip("no " + bin)
		}
	}
	// gpg's socket path has a length limit that t.TempDir can pass.
	home, err := os.MkdirTemp("", "gpg")
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(home, 0o700)
	t.Setenv("GNUPGHOME", home)
	// No pinentry, so a commit that skips the loopback fails rather than
	// opening a passphrase dialog on the desktop running the tests.
	os.WriteFile(filepath.Join(home, "gpg-agent.conf"), []byte("pinentry-program /nonexistent/pinentry\n"), 0o600)
	t.Cleanup(func() {
		exec.Command("gpgconf", "--kill", "gpg-agent").Run()
		os.RemoveAll(home)
	})
	if out, err := exec.Command("gpg", "--batch", "--pinentry-mode", "loopback", "--passphrase", "right horse",
		"--quick-gen-key", "Test <t@t>", "ed25519", "sign", "never").CombinedOutput(); err != nil {
		t.Fatalf("a key: %s", out)
	}

	f := newFixture(t)
	f.store.CommitPassphrase = true
	repo := filepath.Dir(f.store.ModelsDir)
	git := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	for k, v := range map[string]string{"user.name": "t", "user.email": "t@t", "user.signingKey": "t@t", "commit.gpgSign": "true"} {
		git("config", k, v)
	}
	git("-c", "commit.gpgSign=false", "commit", "-q", "--allow-empty", "-m", "start")
	edit := func(description string) {
		m := f.model(t, "qwen")
		m.Description = description
		if err := f.store.SaveModel("qwen", m); err != nil {
			t.Fatal(err)
		}
	}

	edit("one")
	if st, _ := f.store.Git(); !st.SignPassphrase {
		t.Fatal("the page is not told to ask for the passphrase")
	}
	if _, err := f.store.Commit("no passphrase", ""); !errors.Is(err, ErrGPGPassphrase) {
		t.Fatalf("without a passphrase: %v", err)
	}
	if _, err := f.store.Commit("wrong", "wrong horse"); err == nil || !strings.Contains(err.Error(), "Bad passphrase") {
		t.Fatalf("with the wrong passphrase: %v", err)
	}
	if n := git("rev-list", "--count", "HEAD"); n != "1" {
		t.Fatalf("%s commits after two refused", n)
	}
	if _, err := f.store.Commit("qwen: one", "right horse"); err != nil {
		t.Fatal(err)
	}
	if sig := git("log", "-1", "--format=%G? %s"); sig != "G qwen: one" {
		t.Fatalf("the commit: %q", sig)
	}
}
