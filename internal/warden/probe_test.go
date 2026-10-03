package warden

import (
	"os"
	"path/filepath"
	"testing"
)

func fakeProc(t *testing.T, pid, cmdline, comm, cgroup string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"cmdline": cmdline, "comm": comm, "cgroup": cgroup} {
		if body == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestTheNixWrapperDisguiseIsTakenOff(t *testing.T) {
	proc := fakeProc(t, "7", "/nix/store/x-blender/bin/.blender-wrapped\x00--factory\x00", "", "")
	if got := ProcessName(proc, "7"); got != "blender" {
		t.Fatalf("got %q", got)
	}
}

func TestARewrittenChromiumArgvKeepsOnlyTheName(t *testing.T) {
	proc := fakeProc(t, "7", "/opt/T3 Code (Alpha)/electron --type=gpu-process --x=1\x00", "", "")
	if got := ProcessName(proc, "7"); got != "electron" {
		t.Fatalf("got %q", got)
	}
	proc = fakeProc(t, "8", "/opt/T3 Code (Alpha) --type=gpu-process\x00", "", "")
	if got := ProcessName(proc, "8"); got != "T3 Code (Alpha)" {
		t.Fatalf("got %q", got)
	}
}

func TestAProcessWithNoCmdlineFallsBackToComm(t *testing.T) {
	proc := fakeProc(t, "7", "", ".blender-wrappe\n", "")
	if got := ProcessName(proc, "7"); got != "blender" {
		t.Fatalf("got %q", got)
	}
}

func TestAVanishedProcessIsUnknownNotAnError(t *testing.T) {
	if got := ProcessName(t.TempDir(), "7"); got != "unknown" {
		t.Fatalf("got %q", got)
	}
	if got := ProcessUnit(t.TempDir(), "7"); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestTheUnitIsTheLastCgroupComponent(t *testing.T) {
	proc := fakeProc(t, "7", "", "", "0::/system.slice/infermux.service\n")
	if got := ProcessUnit(proc, "7"); got != "infermux.service" {
		t.Fatalf("got %q", got)
	}
}

func attribution(names, units map[uint32]string, ours, desktop []string) Attribution {
	a := Attribution{OurUnits: map[string]bool{}, Desktop: map[string]bool{}}
	for _, u := range ours {
		a.OurUnits[u] = true
	}
	for _, d := range desktop {
		a.Desktop[d] = true
	}
	a.Name = func(pid uint32) string { return names[pid] }
	a.Unit = func(pid uint32) string { return units[pid] }
	return a
}

func TestOurGenerationIsSplitFromEveryoneElses(t *testing.T) {
	a := attribution(
		map[uint32]string{1: "llama-server", 2: "blender", 3: "comfyui"},
		map[uint32]string{1: "infermux.service", 2: "app-blender.scope", 3: "comfyui.service"},
		[]string{"infermux.service"}, nil,
	)
	r := Summarise(
		map[uint32]int{1: 6000, 2: 2000, 3: 4000},
		map[uint32]float64{1: 90, 2: 30, 3: 5},
		9000, 10240, f64(100), a,
	)
	if *r.ForeignGPUPercent != 35 || r.OurGPUPercent != 90 || r.OurVRAMMB != 6000 {
		t.Fatalf("got foreign %v ours %v vram %v", *r.ForeignGPUPercent, r.OurGPUPercent, r.OurVRAMMB)
	}
	if len(r.Culprits) != 2 || r.Culprits[0] != "blender" || r.Culprits[1] != "comfyui" {
		t.Fatalf("culprits %v", r.Culprits)
	}
	if *r.VRAMFreeMB != 1240 {
		t.Fatalf("free %v", *r.VRAMFreeMB)
	}
}

// The 2026-10-02 incident: T3 Code drawing its own streamed reply, and the
// compositor, read as 36% foreign load and unloaded the model writing it.
func TestDesktopProcessesAreNeitherOursNorContention(t *testing.T) {
	a := attribution(
		map[uint32]string{1: "electron", 2: "cosmic-comp", 3: "firefox"},
		map[uint32]string{1: "app-t3.scope", 2: "session.scope", 3: "app-firefox.scope"},
		nil, []string{"electron", "cosmic-comp"},
	)
	r := Summarise(nil, map[uint32]float64{1: 22, 2: 14, 3: 4}, 4000, 10240, nil, a)
	if *r.ForeignGPUPercent != 4 || r.DesktopGPUPercent != 36 {
		t.Fatalf("foreign %v desktop %v", *r.ForeignGPUPercent, r.DesktopGPUPercent)
	}
	if len(r.Culprits) != 1 || r.Culprits[0] != "firefox" {
		t.Fatalf("culprits %v", r.Culprits)
	}
}

func TestMemoryWithoutWorkIsNotACulprit(t *testing.T) {
	a := attribution(map[uint32]string{1: "steam"}, map[uint32]string{1: "app.scope"}, nil, nil)
	r := Summarise(map[uint32]int{1: 500}, nil, 4000, 10240, nil, a)
	if len(r.Culprits) != 0 {
		t.Fatalf("culprits %v", r.Culprits)
	}
}
