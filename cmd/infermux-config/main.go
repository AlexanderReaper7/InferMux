// infermux-config makes and signs InferMux's one config (0022) from a
// terminal: the key, the first signature of a hand-written config, a check of
// a bundle against the trusted keys, and a host's part as it would run. The
// UI's Publish does the signing in normal use.
//
//	infermux-config keygen -out config-key.age
//	infermux-config sign -key config-key.age -host strix <dir>
//	infermux-config verify -trusted-keys <file> <dir>
//	infermux-config derive -host strix <dir>
//
// The passphrase is read from INFERMUX_CONFIG_PASSPHRASE, or the terminal.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"golang.org/x/term"

	"github.com/mostlygeek/llama-swap/internal/configstore"
	"github.com/mostlygeek/llama-swap/internal/placement"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen(os.Args[2:])
	case "sign":
		err = sign(os.Args[2:])
	case "verify":
		err = verify(os.Args[2:])
	case "derive":
		err = derive(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "infermux-config:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: infermux-config keygen|sign|verify|derive [flags] [dir]")
	os.Exit(2)
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	out := fs.String("out", "", "the encrypted private key to write; it must not exist")
	fs.Parse(args)
	if *out == "" {
		return errors.New("-out is required")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	pass, err := passphrase(true)
	if err != nil {
		return err
	}
	encrypted, err := configstore.EncryptKey(priv, pass)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(encrypted); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Println(configstore.PublicKeyString(pub))
	return nil
}

// sign checks the config as a daemon would, then signs it in place. The
// manifest already in the directory, if any, is the parent: the revision
// goes up by one.
func sign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	keyFile := fs.String("key", "", "the encrypted private key")
	host := fs.String("host", "", "the host publishing, recorded in the manifest")
	fs.Parse(args)
	if *keyFile == "" || *host == "" || fs.NArg() != 1 {
		return errors.New("sign -key <file> -host <name> <dir>")
	}
	dir := fs.Arg(0)
	files, err := placement.ReadDir(dir)
	if err != nil {
		return err
	}
	_, warnings, err := placement.Parse(files)
	if err != nil {
		return fmt.Errorf("not signed: %w", err)
	}
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	var parent *configstore.Bundle
	if manifest, err := os.ReadFile(dir + "/" + configstore.ManifestFile); err == nil {
		parent = &configstore.Bundle{Manifest: manifest}
	}
	encrypted, err := os.ReadFile(*keyFile)
	if err != nil {
		return err
	}
	pass, err := passphrase(false)
	if err != nil {
		return err
	}
	key, err := configstore.DecryptKey(encrypted, pass)
	if err != nil {
		return err
	}
	b, err := configstore.Sign(files, key, *host, parent, time.Now())
	if err != nil {
		return err
	}
	if err := configstore.WriteBundle(dir, b); err != nil {
		return err
	}
	m := b.Info()
	fmt.Printf("revision %d, %s, %d files, %s\n", m.Revision, m.Time.Format(time.RFC3339), len(m.Files), b.Hash())
	return nil
}

func verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	keys := fs.String("trusted-keys", "", "the public keys, one a line")
	fs.Parse(args)
	if *keys == "" || fs.NArg() != 1 {
		return errors.New("verify -trusted-keys <file> <dir>")
	}
	trusted, err := configstore.ReadKeys(*keys)
	if err != nil {
		return err
	}
	b, err := configstore.ReadBundle(fs.Arg(0))
	if err != nil {
		return err
	}
	m, err := b.Verify(trusted, time.Now())
	if err != nil {
		return err
	}
	if _, _, err := placement.Parse(b.Files); err != nil {
		return fmt.Errorf("signed, but does not load: %w", err)
	}
	fmt.Printf("revision %d by %s at %s: verified\n", m.Revision, m.Host, m.Time.Format(time.RFC3339))
	return nil
}

func derive(args []string) error {
	fs := flag.NewFlagSet("derive", flag.ExitOnError)
	host := fs.String("host", "", "the host whose part to print")
	routes := fs.Bool("routes", false, "print the routes as JSON instead of llama-swap's config")
	fs.Parse(args)
	if *host == "" || fs.NArg() != 1 {
		return errors.New("derive -host <name> [-routes] <dir>")
	}
	cfg, warnings, err := placement.Load(fs.Arg(0))
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	part, err := cfg.Derive(*host)
	if err != nil {
		return err
	}
	if !*routes {
		os.Stdout.Write(part.LlamaSwap)
		return nil
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(part.Routes)
}

func passphrase(twice bool) (string, error) {
	if p := os.Getenv("INFERMUX_CONFIG_PASSPHRASE"); p != "" {
		return p, nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", errors.New("no terminal to ask for the passphrase, and INFERMUX_CONFIG_PASSPHRASE is empty")
	}
	fmt.Fprint(os.Stderr, "passphrase: ")
	p, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if twice {
		fmt.Fprint(os.Stderr, "again: ")
		again, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		if string(again) != string(p) {
			return "", errors.New("the passphrases differ")
		}
	}
	if len(p) == 0 {
		return "", errors.New("an empty passphrase")
	}
	return string(p), nil
}
