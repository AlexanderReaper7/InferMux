package placement

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

// Without env the part loads without its models and clouds that read the
// environment, whatever type the field they sit in has; with it, as in
// llama-swap (0022, 11).
func TestLoadsWithLeavesWhatReadsTheEnvironmentToTheDaemon(t *testing.T) {
	base := filepath.Join(t.TempDir(), "base.yaml")
	if err := os.WriteFile(base, []byte("macros:\n  runtime: run\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	part := &Part{LlamaSwap: []byte("models:\n  m:\n    cmd: ${runtime} --port ${PORT}\n    ttl: ${env.INFERMUX_TEST_TTL}\n" +
		"  n:\n    cmd: ${runtime} --port ${PORT}\n" +
		"groups:\n  gpu:\n    members: [m, n]\n" +
		"peers:\n  c:\n    proxy: https://c.example\n    apiKey: ${env.INFERMUX_TEST_UNSET_PART}\n    models: [x]\n")}
	if err := part.LoadsWith(base, false); err != nil {
		t.Fatalf("without env: %v", err)
	}
	t.Setenv("INFERMUX_TEST_TTL", "30")
	t.Setenv("INFERMUX_TEST_UNSET_PART", "k")
	if err := part.LoadsWith(base, true); err != nil {
		t.Fatalf("with env: %v", err)
	}
	t.Setenv("INFERMUX_TEST_TTL", "soon")
	if err := part.LoadsWith(base, true); err == nil {
		t.Fatal("with env, a ttl that is no number loaded")
	}

	// A name reads it too: llama-swap substitutes in keys as in values.
	part.LlamaSwap = []byte("models:\n  n:\n    cmd: ${runtime} --port ${PORT}\ngroups:\n  ${env.INFERMUX_TEST_UNSET_GROUP}:\n    members: [n]\n")
	if err := part.LoadsWith(base, false); err != nil {
		t.Fatalf("a group named from the environment: %v", err)
	}

	// What reads no environment is still loaded.
	part.LlamaSwap = []byte("models:\n  m:\n    cmd: ${elsewhere} --port ${PORT}\n  n:\n    cmd: ${runtime} --port ${PORT}\n    ttl: ${env.INFERMUX_TEST_TTL}\n")
	if err := part.LoadsWith(base, false); err == nil || !strings.Contains(err.Error(), "unknown macro '${elsewhere}'") {
		t.Fatalf("an unknown macro: %v", err)
	}
	// A base config that reads the environment leaves the whole load to the
	// daemon; one in a comment is not read.
	if err := os.WriteFile(base, []byte("# ${env.INFERMUX_TEST_TTL}\nmacros:\n  runtime: run\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := part.LoadsWith(base, false); err == nil {
		t.Fatal("a comment in the base config stopped the load")
	}
	if err := os.WriteFile(base, []byte("macros:\n  runtime: run --key ${env.INFERMUX_TEST_UNSET_BASE}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := part.LoadsWith(base, false); err != nil {
		t.Fatalf("a base config that reads the environment: %v", err)
	}
}

// An entry with an alias into one left out is left out too, so every entry
// kept loads with each anchor it names (sol, reviews 13 and 14).
func TestLoadsWithLeavesOutWhatAnAliasTiesToAnEntryLeftOut(t *testing.T) {
	base := filepath.Join(t.TempDir(), "base.yaml")
	if err := os.WriteFile(base, []byte("macros:\n  runtime: run\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, part, want string }{
		// b reads the environment through a's anchor, so it is left out with a.
		{"an alias to what reads the environment",
			"models:\n  a:\n    cmd: ${runtime} --port ${PORT}\n    ttl: &ttl ${env.INFERMUX_TEST_UNSET_ALIAS}\n" +
				"  b:\n    cmd: ${runtime} --port ${PORT}\n    ttl: *ttl\n", ""},
		// b names an anchor in a, which is left out, so b is too and the
		// daemon checks its macro (sol, review 14).
		{"an alias to an anchor in an entry left out",
			"models:\n  a:\n    cmd: ${runtime} --port ${PORT}\n    ttl: ${env.INFERMUX_TEST_UNSET_ALIAS}\n    metadata: &meta\n      k: v\n" +
				"  b:\n    cmd: ${elsewhere} --port ${PORT}\n    metadata: *meta\n", ""},
		// c names an anchor in b, which names one in a: each round leaves out
		// one more, until c.
		{"a chain of aliases into an entry left out",
			"models:\n  a:\n    cmd: ${runtime} --port ${PORT}\n    ttl: ${env.INFERMUX_TEST_UNSET_ALIAS}\n    metadata: &x\n      k: v\n" +
				"  b:\n    cmd: ${runtime} --port ${PORT}\n    metadata: *x\n    aliases: &y [b2]\n" +
				"  c:\n    cmd: ${elsewhere} --port ${PORT}\n    aliases: *y\n", ""},
		// An entry no alias ties to one left out is kept and checked, though
		// one left out names the same anchor.
		{"an alias between entries kept",
			"models:\n  b:\n    cmd: ${runtime} --port ${PORT}\n    metadata: &meta\n      k: v\n" +
				"  a:\n    cmd: ${runtime} --port ${PORT}\n    ttl: ${env.INFERMUX_TEST_UNSET_ALIAS}\n    metadata: *meta\n" +
				"  c:\n    cmd: ${elsewhere} --port ${PORT}\n    metadata: *meta\n", "unknown macro '${elsewhere}'"},
		// llama-swap refuses it.
		{"an anchor that holds itself",
			"models:\n  a:\n    cmd: ${runtime} --port ${PORT}\n    metadata: &m\n      self: *m\n", "contains itself"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := (&Part{LlamaSwap: []byte(c.part)}).LoadsWith(base, false)
			if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
		})
	}
}

// An alias is never followed, so no anchor makes the check loop or recurse
// without end: sol's review 14 found an anchor reached again through an
// overridden merge key, which llama-swap's own decode never follows, and
// the check overflowed its stack on it. Ten levels of ten aliases behind
// such merge keys are a billion nodes to follow. Each runs in a child with a
// 1 MiB stack and a deadline, since a stack overflow kills the process and
// a loop never returns.
func TestLoadsWithFollowsNoAlias(t *testing.T) {
	var laughs strings.Builder
	laughs.WriteString("models:\n  m:\n    cmd: run --port ${PORT}\n    metadata:\n      l0: &l0 [x, x, x, x, x, x, x, x, x, x]\n")
	for i := 1; i <= 9; i++ {
		fmt.Fprintf(&laughs, "      l%d: &l%d {k: v, <<: {k: [%s]}}\n", i, i, strings.TrimSuffix(strings.Repeat(fmt.Sprintf("*l%d, ", i-1), 10), ", "))
	}
	parts := map[string]string{
		"an anchor reached again through a merge key": "models:\n  m:\n    cmd: run --port ${PORT}\n    metadata: &m\n      k: v\n      <<: {k: *m}\n",
		"a billion aliases behind merge keys":         laughs.String(),
	}
	if name := os.Getenv("INFERMUX_TEST_LOADS_CHILD"); name != "" {
		debug.SetMaxStack(1 << 20)
		if parts[name] == "" {
			t.Fatalf("no part %q", name)
		}
		part := &Part{LlamaSwap: []byte(parts[name])}
		// llama-swap loads both, and so does the check.
		if err := part.LoadsWith("", true); err != nil {
			t.Fatalf("with the environment: %v", err)
		}
		if err := part.LoadsWith("", false); err != nil {
			t.Fatalf("without it: %v", err)
		}
		fmt.Println("both loaded")
		return
	}
	for name := range parts {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLoadsWithFollowsNoAlias$", "-test.count=1")
			cmd.Env = append(os.Environ(), "INFERMUX_TEST_LOADS_CHILD="+name)
			out, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("still loading after 20 s:\n%.2000s", out)
			}
			if err != nil || !strings.Contains(string(out), "both loaded") {
				t.Fatalf("%v:\n%.2000s", err, out)
			}
		})
	}
}
