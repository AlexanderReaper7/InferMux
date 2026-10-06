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
		nil, 9000, 10240, f64(100), a,
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
	r := Summarise(nil, map[uint32]float64{1: 22, 2: 14, 3: 4}, nil, 4000, 10240, nil, a)
	if *r.ForeignGPUPercent != 4 || r.DesktopGPUPercent != 36 {
		t.Fatalf("foreign %v desktop %v", *r.ForeignGPUPercent, r.DesktopGPUPercent)
	}
	if len(r.Culprits) != 1 || r.Culprits[0] != "firefox" {
		t.Fatalf("culprits %v", r.Culprits)
	}
}

func TestMemoryWithoutWorkIsNotACulprit(t *testing.T) {
	a := attribution(map[uint32]string{1: "steam"}, map[uint32]string{1: "app.scope"}, nil, nil)
	r := Summarise(map[uint32]int{1: 500}, nil, nil, 4000, 10240, nil, a)
	if len(r.Culprits) != 0 {
		t.Fatalf("culprits %v", r.Culprits)
	}
}

// OBS with NVIDIA's green screen, measured 2026-10-06: graphics only at 34 MB
// with the filter off, in the compute list at 360 MB with it on (0017).
func TestAPriorityProcessTakesTheCardOnlyWithACUDAContext(t *testing.T) {
	a := attribution(map[uint32]string{1: "obs", 2: "obs"}, map[uint32]string{1: "app-obs.scope", 2: "app-obs.scope"}, nil, nil)
	a.Priority = map[string]bool{"obs": true}

	r := Summarise(map[uint32]int{1: 34}, map[uint32]float64{1: 3}, nil, 4000, 10240, nil, a)
	if len(r.Priority) != 0 || r.Processes[0].Priority {
		t.Fatalf("graphics only: priority %v", r.Priority)
	}

	r = Summarise(map[uint32]int{1: 360, 2: 360}, map[uint32]float64{1: 7}, map[uint32]bool{1: true, 2: true}, 4000, 10240, nil, a)
	if len(r.Priority) != 1 || r.Priority[0] != "obs" {
		t.Fatalf("with CUDA: priority %v", r.Priority)
	}
}

// The process list is cut to ten for display. A priority process idle enough
// to sort last is still named.
func TestAPriorityProcessPastTheDisplayCapIsStillNamed(t *testing.T) {
	names, units := map[uint32]string{}, map[uint32]string{}
	util := map[uint32]float64{}
	for pid := uint32(1); pid <= 12; pid++ {
		names[pid], units[pid] = "game", "app.scope"
		util[pid] = float64(pid)
	}
	names[13], units[13] = "obs", "app-obs.scope"
	a := attribution(names, units, nil, nil)
	a.Priority = map[string]bool{"obs": true}
	r := Summarise(map[uint32]int{13: 360}, util, map[uint32]bool{13: true}, 4000, 10240, nil, a)
	if len(r.Processes) != 10 || len(r.Priority) != 1 {
		t.Fatalf("%d processes shown, priority %v", len(r.Processes), r.Priority)
	}
}

// A priority process that is ours is the models' own work, not a claim on the card.
func TestOurOwnProcessIsNeverPriority(t *testing.T) {
	a := attribution(map[uint32]string{1: "llama-server"}, map[uint32]string{1: "infermux.service"}, []string{"infermux.service"}, nil)
	a.Priority = map[string]bool{"llama-server": true}
	r := Summarise(map[uint32]int{1: 6000}, map[uint32]float64{1: 90}, map[uint32]bool{1: true}, 9000, 10240, nil, a)
	if len(r.Priority) != 0 {
		t.Fatalf("priority %v", r.Priority)
	}
}
