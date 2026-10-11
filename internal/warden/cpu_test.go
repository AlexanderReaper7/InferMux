package warden

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A cgroup tree and a /proc in a temporary directory, moved by hand.
type fakeHost struct {
	t          *testing.T
	root, proc string
}

func newFakeHost(t *testing.T) *fakeHost {
	dir := t.TempDir()
	h := &fakeHost{t: t, root: filepath.Join(dir, "cgroup"), proc: filepath.Join(dir, "proc")}
	h.write(filepath.Join(h.proc, "stat"), "cpu  1 2 3\ncpu0 1 1 1\ncpu1 1 1 1\ncpu2 1 1 1\ncpu3 1 1 1\nintr 5\n")
	h.ram(60000)
	h.usage("", 0)
	h.usage("system.slice/infermux.service", 0)
	h.usage("system.slice/other.service", 0)
	h.usage("idle.slice/infermux-cpu.service", 0)
	return h
}

func (h *fakeHost) write(path, text string) {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *fakeHost) usage(cgroup string, usec uint64) {
	h.write(filepath.Join(h.root, cgroup, "cpu.stat"), "usage_usec "+strconv.FormatUint(usec, 10)+"\nuser_usec 0\n")
}

func (h *fakeHost) ram(availableMB int) {
	h.write(filepath.Join(h.proc, "meminfo"), "MemTotal:       96000000 kB\nMemFree:  1 kB\nMemAvailable:   "+strconv.Itoa(availableMB*1024)+" kB\n")
}

// process writes a process with CPU time ticks in unit.
func (h *fakeHost) process(pid int, name, unit string, ticks int) {
	dir := filepath.Join(h.proc, strconv.Itoa(pid))
	fields := make([]string, 20)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0] = "S"
	fields[11] = strconv.Itoa(ticks) // utime
	h.write(filepath.Join(dir, "stat"), strconv.Itoa(pid)+" (a (b) c) "+strings.Join(fields, " ")+"\n")
	h.write(filepath.Join(dir, "cmdline"), name+"\x00--flag\x00")
	h.write(filepath.Join(dir, "cgroup"), "0::/"+unit+"\n")
}

func (h *fakeHost) meter(mutate func(*Config)) (*cpuMeter, *time.Time) {
	cfg := DefaultConfig()
	cfg.OurUnits = []string{"infermux.service", "infermux-cpu.service"}
	if mutate != nil {
		mutate(&cfg)
	}
	m := newCPUMeter(cfg, h.root, h.proc)
	clock := t0
	m.now = func() time.Time { return clock }
	return m, &clock
}

func TestTheCPUOutsideOurCgroupsIsTheRootsLessOurs(t *testing.T) {
	h := newFakeHost(t)
	m, clock := h.meter(nil)
	if r, err := m.sample(); err != nil || r.ForeignPercent != nil || r.Cores != 4 {
		t.Fatalf("first reading: %+v %v", r, err)
	}
	// 5 s on 4 cores is 20 s of CPU time: 10 s used, 6 s of it ours.
	*clock = t0.Add(5 * time.Second)
	h.usage("", 10_000_000)
	h.usage("system.slice/infermux.service", 4_000_000)
	h.usage("idle.slice/infermux-cpu.service", 2_000_000)
	h.usage("system.slice/other.service", 4_000_000)
	r, err := m.sample()
	if err != nil || r.ForeignPercent == nil || *r.ForeignPercent != 20 || *r.OurPercent != 30 || r.WindowSeconds != 5 {
		t.Fatalf("%+v %v", r, err)
	}
	if r.RAMAvailableMB != 60000 || r.RAMTotalMB != 93750 {
		t.Fatalf("RAM %d of %d", r.RAMAvailableMB, r.RAMTotalMB)
	}
}

func TestTheWindowAveragesAndForgets(t *testing.T) {
	h := newFakeHost(t)
	m, clock := h.meter(func(c *Config) { c.CPU.WindowSeconds = 10 })
	m.sample()
	// 5 s all busy outside, then 5 s idle: 50% over the window.
	*clock = t0.Add(5 * time.Second)
	h.usage("", 20_000_000)
	m.sample()
	*clock = t0.Add(10 * time.Second)
	r, _ := m.sample()
	if *r.ForeignPercent != 50 {
		t.Fatalf("over the window: %v", *r.ForeignPercent)
	}
	// The busy step is older than the window now.
	*clock = t0.Add(15 * time.Second)
	r, _ = m.sample()
	if *r.ForeignPercent != 0 || r.WindowSeconds != 10 {
		t.Fatalf("after the window: %v over %v", *r.ForeignPercent, r.WindowSeconds)
	}
}

func TestAUnitStartedAgainIsNotNegativeTime(t *testing.T) {
	h := newFakeHost(t)
	m, clock := h.meter(nil)
	h.usage("system.slice/infermux.service", 50_000_000)
	h.usage("", 50_000_000)
	m.sample()
	// Restarted: its counter starts from 0 and has 2 s in it, beside 4 s
	// of someone else's.
	*clock = t0.Add(5 * time.Second)
	h.usage("system.slice/infermux.service", 2_000_000)
	h.usage("", 56_000_000)
	r, _ := m.sample()
	if *r.ForeignPercent != 20 || *r.OurPercent != 10 {
		t.Fatalf("%v foreign, %v ours", *r.ForeignPercent, *r.OurPercent)
	}
}

func TestAUnitInsideAnotherOfOursIsCountedOnce(t *testing.T) {
	h := newFakeHost(t)
	m, clock := h.meter(func(c *Config) { c.OurUnits = []string{"idle.slice", "infermux-cpu.service"} })
	h.usage("idle.slice", 0)
	m.sample()
	*clock = t0.Add(5 * time.Second)
	h.usage("idle.slice", 2_000_000)
	h.usage("idle.slice/infermux-cpu.service", 2_000_000)
	h.usage("", 6_000_000)
	r, _ := m.sample()
	if *r.OurPercent != 10 || *r.ForeignPercent != 20 {
		t.Fatalf("%v ours, %v foreign", *r.OurPercent, *r.ForeignPercent)
	}
}

func TestAUnitStartedLaterIsFoundOnTheNextWalk(t *testing.T) {
	h := newFakeHost(t)
	os.RemoveAll(filepath.Join(h.root, "idle.slice"))
	m, clock := h.meter(nil)
	m.sample()
	h.usage("idle.slice/infermux-cpu.service", 0)
	*clock = t0.Add(time.Minute)
	m.sample()
	*clock = t0.Add(time.Minute + 5*time.Second)
	h.usage("idle.slice/infermux-cpu.service", 20_000_000)
	h.usage("", 20_000_000)
	r, _ := m.sample()
	if *r.ForeignPercent != 0 {
		t.Fatalf("the runner's time read as foreign: %v", *r.ForeignPercent)
	}
}

func TestTheBusiestOutsideProcessesAreNamedAndOursAreNot(t *testing.T) {
	h := newFakeHost(t)
	h.process(10, "/nix/store/x/bin/rustc", "user.slice/app.scope", 100)
	h.process(11, "/nix/store/x/bin/rustc", "user.slice/app.scope", 100)
	h.process(12, "firefox", "user.slice/ff.scope", 100)
	h.process(13, "llama-server", "infermux-cpu.service", 100)
	m, clock := h.meter(nil)
	m.sample()
	*clock = t0.Add(5 * time.Second)
	h.process(10, "/nix/store/x/bin/rustc", "user.slice/app.scope", 300)
	h.process(11, "/nix/store/x/bin/rustc", "user.slice/app.scope", 250)
	h.process(12, "firefox", "user.slice/ff.scope", 400)
	h.process(13, "llama-server", "infermux-cpu.service", 9000)
	h.process(14, "cc1", "user.slice/app.scope", 10) // started since: all of it is new
	r, _ := m.sample()
	// rustc 350 between its two, firefox 300, cc1 10.
	if !slices.Equal(r.Culprits, []string{"rustc", "firefox", "cc1"}) {
		t.Fatalf("culprits %v", r.Culprits)
	}
}

func TestAPriorityProcessOnTheCPUIsNamedWhileItRuns(t *testing.T) {
	h := newFakeHost(t)
	h.process(20, "obs", "user.slice/obs.scope", 0)
	h.process(21, "obs", "infermux.service", 0) // ours, whatever its name
	m, _ := h.meter(func(c *Config) { c.CPU.PriorityProcesses = []string{"obs"} })
	r, _ := m.sample()
	if !slices.Equal(r.Priority, []string{"obs"}) {
		t.Fatalf("priority %v", r.Priority)
	}
	os.RemoveAll(filepath.Join(h.proc, "20"))
	if r, _ := m.sample(); len(r.Priority) != 0 {
		t.Fatalf("priority after it exited: %v", r.Priority)
	}
}

func TestAnUnreadableReadingIsAnError(t *testing.T) {
	h := newFakeHost(t)
	os.Remove(filepath.Join(h.proc, "meminfo"))
	m, _ := h.meter(nil)
	if _, err := m.sample(); err == nil {
		t.Fatal("no meminfo, no error")
	}
}

func TestTheCPUsReasons(t *testing.T) {
	p := DefaultConfig().CPU
	for _, c := range []struct {
		r                          CPUResources
		contended, short, priority bool
		why                        string
	}{
		{CPUResources{ForeignPercent: f64(5), RAMAvailableMB: 60000}, false, false, false, "CPU is free"},
		{CPUResources{RAMAvailableMB: 60000}, false, false, false, "CPU is free"},
		{CPUResources{ForeignPercent: f64(25), WindowSeconds: 30, RAMAvailableMB: 60000, Culprits: []string{"rustc"}}, true, false, false, "foreign CPU load 25% >= 25% over 30s (rustc)"},
		{CPUResources{ForeignPercent: f64(5), RAMAvailableMB: 60000, Priority: []string{"obs"}}, true, false, true, "obs needs the CPU (priority process)"},
		{CPUResources{ForeignPercent: f64(80), RAMAvailableMB: 1000, Priority: []string{"obs"}}, true, true, true, "only 1000 MB RAM available, under the floor of 4096 MB"},
	} {
		got := IsCPUContended(c.r, p)
		if got.contended != c.contended || got.ramShort != c.short || got.priority != c.priority || got.why != c.why {
			t.Errorf("%+v: %+v", c.r, got)
		}
	}
}
