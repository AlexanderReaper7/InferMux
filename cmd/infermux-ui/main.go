// infermux-ui is InferMux's web UI, a process of its own beside the daemon
// (0005). It runs as the user, so it can edit and commit the configuration in
// the user's repository, which the sandboxed daemon only reads.
package main

import (
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/muxui"
)

func main() {
	muxui.GPGShim()
	listen := flag.String("listen", "127.0.0.1:5010", "address to serve the UI on")
	daemon := flag.String("daemon", "http://127.0.0.1:5001", "the InferMux daemon")
	keySecrets := flag.String("key-secrets", "", "the sops file a new key's plaintext goes to, so it can be read again")
	sops := flag.String("sops", "sops", "the sops binary")
	ageIdentity := flag.String("age-identity", "", "the user's age identity, encrypted with a passphrase (age -p): what opens -key-secrets")
	daemonKeyFile := flag.String("daemon-key-file", "", "a file holding the UI's key for the daemon, when the daemon has a keys_file")
	modelsDir := flag.String("models-dir", "", "the daemon's -config-dir: one YAML file per model")
	wardenFile := flag.String("warden-config", "", "the daemon's -warden-config")
	baseConfig := flag.String("base-config", "", "the daemon's -config: the runtimes' macros and global settings")
	ggufDirs := flag.String("gguf-dirs", "", "comma-separated directories to look for .gguf files in")
	kvKernels := flag.String("kv-kernels", "", "a JSON file: per runtime macro, the K-V cache pairs with a FlashAttention kernel")
	prebuild := flag.String("prebuild", "", "a flake installable the UI may build ahead of a switch; empty hides the button")
	commitPassphrase := flag.Bool("commit-passphrase", false, "Commit asks for the passphrase of the user's GPG key and signs in gpg's loopback mode, for a host with no session to show a pinentry")
	gpg := flag.String("gpg", "gpg", "the gpg binary, for -commit-passphrase")
	hfDir := flag.String("hf-dir", "", "where a model's Hugging Face files are downloaded to; empty: models cannot name one")
	hfTokenFile := flag.String("hf-token-file", "", "a file holding a Hugging Face token, for gated and private repos")
	flag.Parse()

	if *modelsDir == "" || *wardenFile == "" || *baseConfig == "" {
		slog.Error("-models-dir, -warden-config and -base-config are required")
		os.Exit(2)
	}
	daemonURL, err := url.Parse(*daemon)
	if err != nil {
		slog.Error("bad -daemon", "error", err)
		os.Exit(2)
	}
	daemonKey := ""
	if *daemonKeyFile != "" {
		raw, err := os.ReadFile(*daemonKeyFile)
		if err != nil {
			slog.Error("bad -daemon-key-file", "error", err)
			os.Exit(2)
		}
		daemonKey = strings.TrimSpace(string(raw))
	}
	store := &muxui.Store{ModelsDir: *modelsDir, WardenFile: *wardenFile, BaseConfig: *baseConfig, KeySecrets: *keySecrets, Sops: *sops, AgeIdentity: *ageIdentity,
		CommitPassphrase: *commitPassphrase, GPG: *gpg}
	for _, d := range strings.Split(*ggufDirs, ",") {
		if d = strings.TrimSpace(d); d != "" {
			store.GGUFDirs = append(store.GGUFDirs, d)
		}
	}
	if *kvKernels != "" {
		data, err := os.ReadFile(*kvKernels)
		if err == nil {
			err = json.Unmarshal(data, &store.KVKernels)
		}
		if err != nil {
			slog.Error("bad -kv-kernels", "error", err)
			os.Exit(2)
		}
	}
	if *hfDir != "" {
		store.HF = &muxui.Downloads{Dir: *hfDir, TokenFile: *hfTokenFile}
	}
	build := &muxui.Builder{Installable: *prebuild, Prepare: store.IntentToAdd}
	slog.Info("infermux-ui listening", "address", "http://"+*listen, "daemon", *daemon, "models-dir", *modelsDir)
	if err := http.ListenAndServe(*listen, muxui.Handler(store, build, daemonURL, daemonKey)); err != nil {
		slog.Error("infermux-ui stopped", "error", err)
		os.Exit(1)
	}
}
