package muxui

import (
	"os"
	"path/filepath"
	"testing"
)

const routing = `# The 9B may stay loaded beside whatever else runs.
routing:
  router:
    use: group
    settings:
      groups:
        alone:
          swap: true
          exclusive: false
          members: [qwen]
`

func TestTheRoutingFileIsEditedAsTextAndCheckedByLlamaSwap(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(f.store.ModelsDir, RoutingFile)

	if text, err := f.store.Routing(); err != nil || text != "" {
		t.Fatalf("no file: %q, %v", text, err)
	}
	if err := f.store.SaveRouting(routing); err != nil {
		t.Fatal(err)
	}
	if text, _ := f.store.Routing(); text != routing {
		t.Fatalf("read back %q", text)
	}

	// llama-swap refuses a group with a model it does not know, and both
	// routing styles at once; neither is written.
	for _, bad := range []string{
		"routing:\n  router:\n    settings:\n      groups:\n        g:\n          members: [nobody]\n",
		routing + "groups:\n  old:\n    members: [qwen]\n",
	} {
		if err := f.store.SaveRouting(bad); err == nil {
			t.Fatalf("taken:\n%s", bad)
		}
	}
	if raw, _ := os.ReadFile(path); string(raw) != routing {
		t.Fatalf("a refused text was written:\n%s", raw)
	}

	// The models are still read beside it, and one a group names cannot go.
	if m := f.model(t, "qwen"); m.Name != "qwen" {
		t.Fatal(m)
	}
	if err := f.store.DeleteModel("qwen"); err == nil {
		t.Fatal("a model the routing names was deleted")
	}

	if err := f.store.SaveRouting(" \n"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("blank text left the file: %v", err)
	}
	if err := f.store.SaveRouting(""); err != nil {
		t.Fatalf("blank text with no file: %v", err)
	}
}
