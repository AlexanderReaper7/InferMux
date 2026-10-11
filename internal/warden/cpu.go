package warden

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The CPU's measurement (0022, the warden on every device): the CPU time used
// outside InferMux's cgroups, and the RAM available.
//
// The kernel charges every microsecond of CPU time to a cgroup, and the root
// cgroup's cpu.stat sums them all, so the share outside ours is exact and
// readable while the models run, unlike free VRAM (0002). A cgroup is ours
// when its directory is named for InferMux's own unit or one of
// Config.OurUnits, as a process is for the GPU. The processes are read from
// /proc only to name who was busy.

// CPUResources is one reading of the CPU and the RAM. A nil percent is a
// reading not taken yet, which is never evidence of contention.
type CPUResources struct {
	// ForeignPercent is the CPU time outside InferMux's cgroups over the
	// window, as a percent of every core.
	ForeignPercent *float64 `json:"foreign_percent"`
	OurPercent     *float64 `json:"our_percent"`
	WindowSeconds  float64  `json:"window_seconds"`
	Cores          int      `json:"cores"`
	RAMAvailableMB int      `json:"ram_available_mb"`
	RAMTotalMB     int      `json:"ram_total_mb"`
	// Culprits are the busiest processes outside InferMux since the last
	// reading, by name.
	Culprits []string `json:"culprits"`
	// Priority names the priority processes running.
	Priority  []string  `json:"priority"`
	SampledAt time.Time `json:"sampled_at"`
}

// IsCPUContended reads one CPU measurement against the policy. Available RAM
// under the floor comes first, since it is the one reason that unloads.
func IsCPUContended(r CPUResources, p CPUPolicy) contention {
	short := r.RAMAvailableMB < p.MinAvailableRAMMB
	priority := len(r.Priority) > 0
	c := contention{contended: true, priority: priority, ramShort: short}
	switch {
	case short:
		c.why = fmt.Sprintf("only %d MB RAM available, under the floor of %d MB", r.RAMAvailableMB, p.MinAvailableRAMMB)
	case priority:
		c.why = strings.Join(r.Priority, ", ") + " needs the CPU (priority process)"
	case r.ForeignPercent != nil && *r.ForeignPercent >= p.BusyPercent:
		detail := ""
		if len(r.Culprits) > 0 {
			detail = " (" + strings.Join(r.Culprits, ", ") + ")"
		}
		c.why = fmt.Sprintf("foreign CPU load %.0f%% >= %.0f%% over %.0fs%s", math.Round(*r.ForeignPercent), p.BusyPercent, r.WindowSeconds, detail)
	default:
		return contention{why: "CPU is free"}
	}
	return c
}

// cpuMeter keeps what a reading is a difference against: the cgroups' usage
// at the last reading, the steps within the window, and each process's CPU
// time.
type cpuMeter struct {
	mu       sync.Mutex
	root     string // /sys/fs/cgroup
	proc     string // /proc
	attr     Attribution
	priority map[string]bool
	window   time.Duration
	now      func() time.Time
	cores    int

	// paths are our units' cgroup directories, walked again when one cannot
	// be read and once a minute, so a unit started later is found.
	paths    []string
	walkedAt time.Time
	usage    map[string]uint64
	last     time.Time
	steps    []cpuStep
	ticks    map[string]uint64
}

// cpuStep is one reading's difference: its length, and the CPU time used in
// it, in all and in our cgroups.
type cpuStep struct {
	at          time.Time
	dt          time.Duration
	total, ours uint64
}

func newCPUMeter(cfg Config, root, proc string) *cpuMeter {
	priority := map[string]bool{}
	for _, name := range cfg.CPU.PriorityProcesses {
		priority[name] = true
	}
	return &cpuMeter{
		root:     root,
		proc:     proc,
		attr:     newAttribution(cfg, proc),
		priority: priority,
		window:   time.Duration(cfg.CPU.WindowSeconds) * time.Second,
		now:      time.Now,
		cores:    onlineCores(proc),
		usage:    map[string]uint64{},
	}
}

func newCPUProbe(cfg Config) func() (CPUResources, error) {
	return newCPUMeter(cfg, "/sys/fs/cgroup", "/proc").sample
}

func (m *cpuMeter) sample() (CPUResources, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if m.cores < 1 {
		return CPUResources{}, errors.New("no CPU in /proc/stat")
	}
	total, err := cgroupUsage(m.root)
	if err != nil {
		return CPUResources{}, err
	}
	available, ramTotal, err := meminfo(m.proc)
	if err != nil {
		return CPUResources{}, err
	}
	if m.paths == nil || now.Sub(m.walkedAt) >= time.Minute {
		m.paths, m.walkedAt = ourCgroups(m.root, m.attr.OurUnits), now
	}
	var ours uint64
	seen := map[string]bool{"": true}
	for _, p := range m.paths {
		u, err := cgroupUsage(p)
		if err != nil {
			// Stopped, or moved: walk again next time.
			m.walkedAt = time.Time{}
			continue
		}
		seen[p] = true
		if before, ok := m.usage[p]; ok {
			ours += since(before, u)
		}
		m.usage[p] = u
	}
	for p := range m.usage {
		if !seen[p] {
			delete(m.usage, p)
		}
	}
	if before, ok := m.usage[""]; ok {
		step := cpuStep{at: now, dt: now.Sub(m.last), total: since(before, total), ours: min(ours, since(before, total))}
		m.steps = append(m.steps, step)
	}
	m.usage[""], m.last = total, now
	for len(m.steps) > 1 && now.Sub(m.steps[0].at) >= m.window {
		m.steps = m.steps[1:]
	}

	res := CPUResources{Cores: m.cores, RAMAvailableMB: available, RAMTotalMB: ramTotal, SampledAt: now}
	var dt time.Duration
	var used, our uint64
	for _, s := range m.steps {
		dt += s.dt
		used += s.total
		our += s.ours
	}
	if dt > 0 {
		capacity := float64(dt.Microseconds()) * float64(m.cores)
		foreign := round1(max(0, float64(used-our)/capacity*100))
		mine := round1(float64(our) / capacity * 100)
		res.ForeignPercent, res.OurPercent, res.WindowSeconds = &foreign, &mine, round1(dt.Seconds())
	}
	res.Culprits, res.Priority = m.scan()
	return res, nil
}

// since is a counter's growth. A counter that went down belongs to a cgroup
// made again under the same name, and all of it is new.
func since(before, now uint64) uint64 {
	if now < before {
		return now
	}
	return now - before
}

// scan reads every process's CPU time, and names the busiest outside
// InferMux since the last scan and the priority processes running. A process
// not seen last time started since, so all of its time is new; on the first
// scan nothing is.
func (m *cpuMeter) scan() (culprits, priority []string) {
	entries, err := os.ReadDir(m.proc)
	if err != nil {
		return []string{}, []string{}
	}
	first := m.ticks == nil
	next := make(map[string]uint64, len(entries))
	busy := map[string]uint64{}
	priority = []string{}
	for _, e := range entries {
		pid := e.Name()
		if pid[0] < '0' || pid[0] > '9' {
			continue
		}
		t, ok := processTicks(m.proc, pid)
		if !ok {
			continue
		}
		next[pid] = t
		var used uint64
		if !first {
			used = since(m.ticks[pid], t)
		}
		if used == 0 && len(m.priority) == 0 {
			continue
		}
		n, _ := strconv.ParseUint(pid, 10, 32)
		if unit := m.attr.Unit(uint32(n)); unit != "" && m.attr.OurUnits[unit] {
			continue
		}
		name := m.attr.Name(uint32(n))
		if m.priority[name] && !slices.Contains(priority, name) {
			priority = append(priority, name)
		}
		if used > 0 {
			busy[name] += used
		}
	}
	m.ticks = next
	names := make([]string, 0, len(busy))
	for name := range busy {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if busy[names[i]] != busy[names[j]] {
			return busy[names[i]] > busy[names[j]]
		}
		return names[i] < names[j]
	})
	sort.Strings(priority)
	return names[:min(3, len(names))], priority
}

// processTicks is utime plus stime from /proc/<pid>/stat, every thread's.
// The name in parentheses may hold spaces and parentheses itself, so the
// fields are counted from the last ')'.
func processTicks(proc, pid string) (uint64, bool) {
	raw, err := os.ReadFile(filepath.Join(proc, pid, "stat"))
	if err != nil {
		return 0, false
	}
	i := bytes.LastIndexByte(raw, ')')
	if i < 0 {
		return 0, false
	}
	// After ")": state is field 3, utime 14 and stime 15.
	fields := strings.Fields(string(raw[i+1:]))
	if len(fields) < 13 {
		return 0, false
	}
	utime, err1 := strconv.ParseUint(fields[11], 10, 64)
	stime, err2 := strconv.ParseUint(fields[12], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return utime + stime, true
}

// cgroupUsage is usage_usec from a cgroup's cpu.stat.
func cgroupUsage(dir string) (uint64, error) {
	f, err := os.Open(filepath.Join(dir, "cpu.stat"))
	if err != nil {
		return 0, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if v, ok := strings.CutPrefix(s.Text(), "usage_usec "); ok {
			return strconv.ParseUint(v, 10, 64)
		}
	}
	return 0, fmt.Errorf("%s/cpu.stat has no usage_usec", dir)
}

// ourCgroups walks the cgroup tree for directories named for our units. One
// inside another is dropped, since the outer one's usage counts it already.
func ourCgroups(root string, units map[string]bool) []string {
	var found []string
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if path != root && units[d.Name()] {
			found = append(found, path)
			return filepath.SkipDir
		}
		return nil
	})
	return found
}

// meminfo is MemAvailable and MemTotal, in MB.
func meminfo(proc string) (available, total int, err error) {
	raw, err := os.ReadFile(filepath.Join(proc, "meminfo"))
	if err != nil {
		return 0, 0, err
	}
	got := 0
	for _, line := range strings.Split(string(raw), "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok || (key != "MemAvailable" && key != "MemTotal") {
			continue
		}
		kb, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(rest), " kB"))
		if err != nil {
			return 0, 0, fmt.Errorf("meminfo %s: %w", key, err)
		}
		if key == "MemAvailable" {
			available = kb / 1024
		} else {
			total = kb / 1024
		}
		got++
	}
	if got != 2 {
		return 0, 0, errors.New("meminfo has no MemAvailable or MemTotal")
	}
	return available, total, nil
}

// onlineCores counts the cpuN lines of /proc/stat.
func onlineCores(proc string) int {
	raw, err := os.ReadFile(filepath.Join(proc, "stat"))
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "cpu") && len(line) > 3 && line[3] >= '0' && line[3] <= '9' {
			n++
		}
	}
	return n
}
