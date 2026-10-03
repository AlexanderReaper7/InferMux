package muxui

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/mostlygeek/llama-swap/internal/warden"
)

// pass is the test identity's passphrase.
const pass = "correct horse"

// withSops gives the fixture a real sops file: an age identity of its own,
// protected by a passphrase as `age -p` does it, and a .sops.yaml one
// directory up from the secrets file, as in the user's repo. It returns the
// identity in the clear, which the test may plant where sops would look.
func withSops(t *testing.T, f fixture) string {
	t.Helper()
	dir := filepath.Dir(f.store.WardenFile)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	plain := "# created: test\n# public key: " + id.Recipient().String() + "\n" + id.String() + "\n"
	r, _ := age.NewScryptRecipient(pass)
	r.SetWorkFactor(10) // age -p uses 18; this keeps the suite fast
	var sealed bytes.Buffer
	w, _ := age.Encrypt(&sealed, r)
	w.Write([]byte(plain))
	w.Close()
	f.store.AgeIdentity = filepath.Join(dir, "age.txt")
	os.WriteFile(f.store.AgeIdentity, sealed.Bytes(), 0o600)

	os.WriteFile(filepath.Join(dir, ".sops.yaml"), []byte("creation_rules:\n  - path_regex: secrets/.*\\.yaml$\n    age: "+id.Recipient().String()+"\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "secrets"), 0o755)
	f.store.KeySecrets = filepath.Join(dir, "secrets", "infermux-keys.yaml")
	// Every command line sops is given, which any user can read in /proc,
	// and the environment it ran in.
	real, err := exec.LookPath("sops")
	if err != nil {
		t.Fatal(err)
	}
	f.store.Sops = filepath.Join(dir, "sops")
	os.WriteFile(f.store.Sops, []byte("#!/bin/sh\necho \"$@\" >> "+filepath.Join(dir, "sops-argv")+
		"\nset | grep -E '^(DISPLAY|WAYLAND_DISPLAY|DBUS_SESSION_BUS_ADDRESS|HOME|SOPS_AGE_KEY_FILE)=' >> "+filepath.Join(dir, "sops-env")+
		"\nexec "+real+" \"$@\"\n"), 0o755)
	return plain
}

func sopsArgv(t *testing.T, f fixture) string {
	t.Helper()
	return string(mustRead(t, filepath.Join(filepath.Dir(f.store.WardenFile), "sops-argv")))
}

func keysOnDisk(t *testing.T, f fixture) warden.KeyFile {
	t.Helper()
	kf, err := warden.LoadKeys(filepath.Join(filepath.Dir(f.store.WardenFile), "keys.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return kf
}

func TestANewKeyIsHashedForTheDaemonsAndReadableAgainFromSops(t *testing.T) {
	f := newFixture(t)
	withSops(t, f)
	// The first creates the file, which needs only the recipients.
	first, err := f.store.CreateKey("phone", warden.Key{Class: warden.Interactive, Allow: []string{"zbox/*"}, SHA256: "ignored"}, "")
	if err != nil {
		t.Fatal(err)
	}
	// The second goes into the file the first created.
	second, err := f.store.CreateKey("laptop", warden.Key{Class: warden.Batch}, pass)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || !strings.HasPrefix(first, "imx-") || len(first) != 4+64 {
		t.Fatalf("keys %q %q", first, second)
	}
	kf := keysOnDisk(t, f)
	if k := kf.Keys["phone"]; k.SHA256 != warden.HashKey(first) || k.Class != warden.Interactive || strings.Join(k.Allow, ",") != "zbox/*" {
		t.Fatalf("phone in keys.yaml: %+v", k)
	}
	if kf.Keys["laptop"].SHA256 != warden.HashKey(second) || kf.Keys["episteme-batch"].Class != warden.Batch {
		t.Fatalf("keys.yaml: %+v", kf)
	}
	raw, _ := os.ReadFile(f.store.KeySecrets)
	keysRaw, _ := os.ReadFile(filepath.Join(filepath.Dir(f.store.WardenFile), "keys.yaml"))
	if strings.Contains(string(raw)+string(keysRaw), first) || !strings.Contains(string(raw), "sops:") {
		t.Fatalf("a key in the clear:\n%s\n%s", raw, keysRaw)
	}
	if argv := sopsArgv(t, f); strings.Contains(argv, first) || strings.Contains(argv, second) || !strings.Contains(argv, "set") {
		t.Fatalf("a key on sops' command line:\n%s", argv)
	}
	for name, want := range map[string]string{"phone": first, "laptop": second} {
		if got, err := f.store.RevealKey(name, pass); err != nil || got != want {
			t.Fatalf("%s read back as %q, %v", name, got, err)
		}
	}
}

func TestARefusedKeyWritesNothing(t *testing.T) {
	f := newFixture(t)
	withSops(t, f)
	before := keysOnDisk(t, f)
	for name, k := range map[string]warden.Key{
		"episteme-batch": {Class: warden.Batch},       // taken
		"no spaces":      {Class: warden.Interactive}, // not a name
		"x":              {Class: "urgent"},
		"y":              {Class: warden.Batch, Allow: []string{"["}},
	} {
		if _, err := f.store.CreateKey(name, k, pass); err == nil {
			t.Errorf("%q %+v was made", name, k)
		}
	}
	if _, err := os.Stat(f.store.KeySecrets); err == nil {
		t.Error("a refused key reached the sops file")
	}
	if after := keysOnDisk(t, f); len(after.Keys) != len(before.Keys) {
		t.Errorf("keys.yaml changed: %+v", after)
	}

	f.store.KeySecrets = ""
	if _, err := f.store.CreateKey("phone", warden.Key{Class: warden.Interactive}, pass); err == nil || len(keysOnDisk(t, f).Keys) != 1 {
		t.Error("a key nobody could read again was made")
	}
}

func TestEditingAKeyKeepsItAndDeletingOneRevokesIt(t *testing.T) {
	f := newFixture(t)
	withSops(t, f)
	keysFile := filepath.Join(filepath.Dir(f.store.WardenFile), "keys.yaml")
	os.WriteFile(keysFile, append([]byte("# shared by both hosts\n"), mustRead(t, keysFile)...), 0o644)
	key, err := f.store.CreateKey("phone", warden.Key{Class: warden.Interactive}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.UpdateKey("phone", warden.Key{Class: warden.Batch, Allow: []string{"openrouter/*"}, SHA256: warden.HashKey("other")}); err != nil {
		t.Fatal(err)
	}
	if k := keysOnDisk(t, f).Keys["phone"]; k.SHA256 != warden.HashKey(key) || k.Class != warden.Batch || k.Allow[0] != "openrouter/*" {
		t.Fatalf("after the edit: %+v", k)
	}
	if err := f.store.UpdateKey("nobody", warden.Key{Class: warden.Batch}); err == nil {
		t.Fatal("edited a key that does not exist")
	}

	if err := f.store.DeleteKey("phone", pass); err != nil {
		t.Fatal(err)
	}
	if _, ok := keysOnDisk(t, f).Keys["phone"]; ok {
		t.Fatal("still in keys.yaml")
	}
	if _, err := f.store.RevealKey("phone", pass); err == nil {
		t.Fatal("its plaintext is still in the sops file")
	}
	// A key made before the sops file existed has no plaintext to remove.
	if err := f.store.DeleteKey("episteme-batch", pass); err != nil {
		t.Fatal(err)
	}
	if raw := mustRead(t, keysFile); !strings.HasPrefix(string(raw), "# shared by both hosts") {
		t.Fatalf("the comment is gone:\n%s", raw)
	}
}

// The passphrase is what opens the file. Without it, or with the wrong one,
// nothing is written, and the identity sops could find on its own (the
// user's keys.txt, here in the clear) is out of its reach.
func TestTheSopsFileOpensOnlyWithThePassphrase(t *testing.T) {
	f := newFixture(t)
	plain := withSops(t, f)
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".config", "sops", "age"), 0o700)
	os.WriteFile(filepath.Join(home, ".config", "sops", "age", "keys.txt"), []byte(plain), 0o600)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("SOPS_AGE_KEY_FILE", filepath.Join(home, ".config", "sops", "age", "keys.txt"))
	t.Setenv("WAYLAND_DISPLAY", "wayland-1")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/user/1000/bus")

	if _, err := f.store.CreateKey("phone", warden.Key{Class: warden.Interactive}, ""); err != nil {
		t.Fatal(err)
	}
	before := string(mustRead(t, f.store.KeySecrets))
	if _, err := f.store.RevealKey("phone", ""); !errors.Is(err, ErrPassphrase) {
		t.Errorf("no passphrase: %v, not one that says so", err)
	}
	for _, p := range []string{"", "wrong"} {
		if _, err := f.store.CreateKey("laptop", warden.Key{Class: warden.Batch}, p); err == nil {
			t.Errorf("a second key with passphrase %q was made", p)
		}
		if _, err := f.store.RevealKey("phone", p); err == nil {
			t.Errorf("revealed with passphrase %q", p)
		}
		if err := f.store.DeleteKey("phone", p); err == nil {
			t.Errorf("revoked with passphrase %q", p)
		}
	}
	if _, ok := keysOnDisk(t, f).Keys["laptop"]; ok || string(mustRead(t, f.store.KeySecrets)) != before {
		t.Fatal("a refused operation wrote")
	}
	if _, ok := keysOnDisk(t, f).Keys["phone"]; !ok {
		t.Fatal("a refused revoke removed the hash")
	}
	if env := string(mustRead(t, filepath.Join(filepath.Dir(f.store.WardenFile), "sops-env"))); strings.Contains(env, home) ||
		strings.Contains(env, "WAYLAND") || strings.Contains(env, "DBUS") || strings.Contains(env, "SOPS_AGE_KEY_FILE") {
		t.Fatalf("sops ran with the user's environment:\n%s", env)
	}
}

func TestAKeyMadeBeforeTheSopsFileIsDeletedAll(t *testing.T) {
	f := newFixture(t)
	withSops(t, f)
	if err := f.store.DeleteKey("episteme-batch", pass); err != nil {
		t.Fatal(err)
	}
}

func TestWithoutAKeysFileThereAreNoKeys(t *testing.T) {
	f := newFixture(t)
	os.WriteFile(f.store.WardenFile, []byte("host: here\n"), 0o644)
	if _, err := f.store.Keys(); err != ErrNoKeysFile {
		t.Fatalf("%v", err)
	}
}

func TestTheKeysPageOverHTTP(t *testing.T) {
	u := newUI(t)
	withSops(t, u.f)
	code, out := u.call("POST", "/api/keys/phone", map[string]any{"class": "interactive", "passphrase": pass})
	u.want(code, 200, out, "create")
	key := out["key"].(string)

	code, out = u.call("GET", "/api/keys", nil)
	u.want(code, 200, out, "list")
	if listed := out["keys"].(map[string]any); len(listed) != 2 || strings.Contains(stringify(out), key) {
		t.Fatalf("the list: %v", out)
	}
	code, out = u.call("POST", "/api/keys/phone/reveal", map[string]any{"passphrase": "wrong"})
	u.want(code, http.StatusUnprocessableEntity, out, "reveal with the wrong passphrase")
	code, out = u.call("POST", "/api/keys/phone/reveal", map[string]any{"passphrase": pass})
	if code != 200 || out["key"] != key {
		t.Fatalf("reveal: %d %v", code, out)
	}
	code, out = u.call("POST", "/api/keys/phone", map[string]any{"class": "interactive"})
	u.want(code, http.StatusUnprocessableEntity, out, "the same name twice")
	code, out = u.call("PUT", "/api/keys/nobody", map[string]any{"class": "batch"})
	u.want(code, http.StatusNotFound, out, "edit a key that is not there")

	// Another page cannot read a key: the guard wants the UI's own origin.
	req := httptest.NewRequest("POST", "/api/keys/phone/reveal", nil)
	req.Host = "127.0.0.1:5010"
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("X-InferMux", "1")
	rec := httptest.NewRecorder()
	u.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), key) {
		t.Fatalf("a cross-origin reveal: %d %s", rec.Code, rec.Body)
	}

	code, out = u.call("DELETE", "/api/keys/phone", map[string]any{"passphrase": pass})
	u.want(code, 200, out, "delete")
}

// allow: [] is a key that may use no model, a host's key for discovery. An
// absent list is every model, so the two must not collapse into each other
// on their way through the page and back into the file.
func TestAKeyAllowedNoModelStaysThatWay(t *testing.T) {
	u := newUI(t)
	code, out := u.call("PUT", "/api/keys/episteme-batch", map[string]any{"class": "batch", "allow": []string{}})
	u.want(code, 200, out, "edit to allow nothing")
	if k := keysOnDisk(t, u.f).Keys["episteme-batch"]; k.Allow == nil {
		t.Fatalf("on disk the key may use every model: %+v", k)
	}
	code, out = u.call("GET", "/api/keys", nil)
	u.want(code, 200, out, "list")
	listed := out["keys"].(map[string]any)["episteme-batch"].(map[string]any)
	if allow, ok := listed["allow"].([]any); !ok || len(allow) != 0 {
		t.Fatalf("the page is told it may use every model: %v", listed)
	}
	code, out = u.call("PUT", "/api/keys/episteme-batch", map[string]any{"class": "batch"})
	u.want(code, 200, out, "edit back to every model")
	if k := keysOnDisk(t, u.f).Keys["episteme-batch"]; k.Allow != nil {
		t.Fatalf("on disk the key still may use only %v", k.Allow)
	}
}

func TestTheKeysFilesAreCommittedWithTheRest(t *testing.T) {
	f := newFixture(t)
	withSops(t, f)
	repo := filepath.Dir(f.store.ModelsDir)
	for _, args := range [][]string{{"init", "-q"}, {"add", "models", "warden.yaml", "base.yaml", "gguf"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "start"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	if _, err := f.store.CreateKey("phone", warden.Key{Class: warden.Interactive}, ""); err != nil {
		t.Fatal(err)
	}
	st, err := f.store.Git()
	if err != nil {
		t.Fatal(err)
	}
	changes := strings.Join(st.Changes, "\n")
	if !strings.Contains(changes, "keys.yaml") || !strings.Contains(changes, "secrets/infermux-keys.yaml") || strings.Contains(changes, "age.txt") {
		t.Fatalf("changes: %s", changes)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func stringify(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}
