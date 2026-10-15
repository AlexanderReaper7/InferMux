package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/configstore"
)

// The built daemon, under the one config, moves a derived file that no
// longer loads with -config aside before llama-swap loads, and serves,
// where it exited and systemd restarted it without end. It is main's call
// that does it, which no test in this process reaches (0022, 11; the user,
// 2026-10-15, for a test that builds the binary and runs it).
func TestTheBuiltDaemonMovesADerivedFileThatDoesNotLoadAside(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "infermux")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	base := write("base.yaml", "performance:\n  disabled: true\nmacros:\n  other: /bin/llama-server\n")
	derived := filepath.Join(dir, "derived")
	if err := os.Mkdir(derived, 0o700); err != nil {
		t.Fatal(err)
	}
	file := write(filepath.Join("derived", derivedFile), "models:\n  m:\n    cmd: ${server} --port ${PORT}\n")
	if _, err := config.LoadConfigSources(base, derived); err == nil {
		t.Fatal("the derived file loads with -config; the test proves nothing")
	}
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	trusted := write("trusted", configstore.PublicKeyString(pub)+"\n")

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	logPath := filepath.Join(dir, "log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	daemon := exec.Command(bin, "-config", base, "-config-dir", derived, "-config-trusted-keys", trusted, "-listen", addr)
	// No PATH: upstream's hardware description runs nvidia-smi when it finds
	// one, and no test reads the host's GPU. No -warden-config, so no warden
	// probes the host.
	daemon.Env = []string{"HOME=" + dir}
	daemon.Stdout, daemon.Stderr = logFile, logFile
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- daemon.Wait() }()
	defer func() {
		daemon.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			daemon.Process.Kill()
			<-exited
		}
	}()
	log := func() string {
		raw, _ := os.ReadFile(logPath)
		return string(raw)
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		select {
		case err := <-exited:
			exited <- err
			t.Fatalf("the daemon exited: %v\n%s", err, log())
		default:
		}
		if resp, err := http.Get("http://" + addr + "/health"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the daemon did not answer /health in 30 s\n%s", log())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("the derived file is still in -config-dir: %v\n%s", err, log())
	}
	if raw, err := os.ReadFile(file + refusedSuffix); err != nil || !strings.Contains(string(raw), "${server}") {
		t.Fatalf("the file moved aside: %q %v", raw, err)
	}
}
