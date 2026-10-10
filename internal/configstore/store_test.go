package configstore

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func key(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

var t0 = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

func files(main string) map[string][]byte {
	return map[string][]byte{"infermux.yaml": []byte(main), "models/m.yaml": []byte("cmd: x\n")}
}

func TestVerify(t *testing.T) {
	pub, priv := key(t)
	other, _ := key(t)
	b, err := Sign(files("hosts: {}\n"), priv, "strix", nil, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Verify([]ed25519.PublicKey{other, pub}, t0); err != nil {
		t.Fatalf("a good bundle: %v", err)
	}
	for _, c := range []struct {
		name   string
		change func(b *Bundle)
		keys   []ed25519.PublicKey
		now    time.Time
		want   string
	}{
		{"untrusted", func(*Bundle) {}, []ed25519.PublicKey{other}, t0, "not from a trusted config key"},
		{"changed file", func(b *Bundle) { b.Files["models/m.yaml"] = []byte("cmd: y\n") }, nil, t0, "does not match the manifest"},
		{"extra file", func(b *Bundle) { b.Files["models/n.yaml"] = []byte("cmd: y\n") }, nil, t0, "not in the manifest"},
		{"missing file", func(b *Bundle) { delete(b.Files, "models/m.yaml") }, nil, t0, "is missing"},
		{"changed manifest", func(b *Bundle) { b.Manifest = []byte(strings.Replace(string(b.Manifest), "strix", "zbox", 1)) }, nil, t0, "not from a trusted config key"},
		{"from the future", func(*Bundle) {}, nil, t0.Add(-time.Hour), "ahead of this host's clock"},
	} {
		copy := *b
		copy.Files = map[string][]byte{}
		for k, v := range b.Files {
			copy.Files[k] = v
		}
		c.change(&copy)
		keys := c.keys
		if keys == nil {
			keys = []ed25519.PublicKey{pub}
		}
		if _, err := copy.Verify(keys, c.now); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	if _, err := Sign(map[string][]byte{"../etc/passwd": nil}, priv, "strix", nil, t0); err == nil {
		t.Error("signed a file outside the config")
	}
}

func TestReadWriteBundle(t *testing.T) {
	_, priv := key(t)
	b, err := Sign(files("hosts: {}\n"), priv, "strix", nil, t0)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "models"), 0o755)
	os.WriteFile(filepath.Join(dir, "models", "gone.yaml"), []byte("cmd: z\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("notes\n"), 0o644)
	if err := WriteBundle(dir, b); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "models", "gone.yaml")); !os.IsNotExist(err) {
		t.Error("a model the bundle does not have stayed")
	}
	if _, err := os.Stat(filepath.Join(dir, "README.md")); err != nil {
		t.Error("the README went")
	}
	back, err := ReadBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	if back.Hash() != b.Hash() || string(back.Files["models/m.yaml"]) != "cmd: x\n" || len(back.Files) != 2 {
		t.Errorf("read back %+v", back)
	}
}

func store(t *testing.T, dir string, pub ed25519.PublicKey, now time.Time) *Store {
	t.Helper()
	s, err := Open(dir, []ed25519.PublicKey{pub})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now }
	return s
}

func TestOffer(t *testing.T) {
	pub, priv := key(t)
	dir := t.TempDir()
	s := store(t, dir, pub, t0.Add(time.Hour))
	sign := func(main, host string, parent *Bundle, at time.Time) *Bundle {
		b, err := Sign(files(main), priv, host, parent, at)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	offer := func(b *Bundle, seed bool, want Result) {
		t.Helper()
		got, err := s.Offer(b, seed)
		if err != nil || got != want {
			t.Fatalf("offer: %v %v, want %v", got, err, want)
		}
	}
	r1 := sign("a: 1\n", "strix", nil, t0)
	offer(r1, true, Applied)
	offer(r1, false, Same)
	r2 := sign("a: 2\n", "strix", r1, t0.Add(time.Minute))
	offer(r2, false, Applied)
	offer(r1, true, Old)
	offer(r1, false, Old)
	if s.Conflict() != nil {
		t.Fatal("a conflict from a fast-forward")
	}

	// The zbox edits r2 a minute after strix did: its later time wins.
	strix := sign("a: strix\n", "strix", r2, t0.Add(2*time.Minute))
	zbox := sign("a: zbox\n", "zbox", r2, t0.Add(3*time.Minute))
	offer(strix, false, Applied)
	offer(zbox, false, Replaced)
	c := s.Conflict()
	if c == nil || c.Kept.Hash != zbox.Hash() || c.Lost.Hash != strix.Hash() || c.Kept.Host != "zbox" {
		t.Fatalf("conflict %+v", c)
	}
	if lost, err := s.Version(strix.Hash()); err != nil || string(lost.Files["infermux.yaml"]) != "a: strix\n" {
		t.Errorf("the lost version: %v", err)
	}
	if err := s.Dismiss(); err != nil || s.Conflict() != nil {
		t.Fatalf("dismiss: %v", err)
	}

	// An older edit made apart arrives late: kept as the loser.
	late := sign("a: late\n", "strix", r2, t0.Add(150*time.Second))
	offer(late, false, Lost)
	if c := s.Conflict(); c == nil || c.Lost.Hash != late.Hash() || c.Kept.Hash != zbox.Hash() {
		t.Fatalf("conflict %+v", c)
	}
	// An older seed is not a conflict.
	s.Dismiss()
	seed := sign("a: seed\n", "strix", r1, t0.Add(30*time.Second))
	offer(seed, true, Old)
	if s.Conflict() != nil {
		t.Error("an older seed raised a conflict")
	}
	if cur := s.Current(); cur.Hash() != zbox.Hash() {
		t.Errorf("current %s", cur.Info().Host)
	}

	// Reopened, the store has the same current version and history.
	s2 := store(t, dir, pub, t0.Add(time.Hour))
	if cur := s2.Current(); cur == nil || cur.Hash() != zbox.Hash() {
		t.Fatal("reopened without the current version")
	}
	if got, _ := s2.Offer(late, false); got != Old {
		t.Errorf("the settled loser offered again: %v", got)
	}

	// The trusted key changed with a rebuild: nothing runs.
	other, _ := key(t)
	s3, err := Open(dir, []ed25519.PublicKey{other})
	if err == nil || s3.Current() != nil {
		t.Errorf("a version signed by a key no longer trusted: %v", err)
	}
}

func TestTiesGoToTheGreaterHost(t *testing.T) {
	a := Manifest{Time: t0, Host: "strix"}
	b := Manifest{Time: t0, Host: "zbox"}
	if Newer(a, b) || !Newer(b, a) {
		t.Error("tie order")
	}
}

func TestReadKeys(t *testing.T) {
	pub, _ := key(t)
	file := filepath.Join(t.TempDir(), "keys")
	os.WriteFile(file, []byte("# the config key\n"+base64.StdEncoding.EncodeToString(pub)+" infermux-config\n"), 0o644)
	keys, err := ReadKeys(file)
	if err != nil || len(keys) != 1 || !keys[0].Equal(pub) {
		t.Fatalf("keys %v %v", keys, err)
	}
	os.WriteFile(file, []byte("# nothing\n"), 0o644)
	if _, err := ReadKeys(file); err == nil {
		t.Error("a file with no keys")
	}
}
