package warden

import (
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The measurement: who is using the GPU, how hard, and with how much memory.
//
// NVML, the library under nvidia-smi. Measured on the target box (RTX 3080,
// driver 615.71) on 2026-09-28, while Blender rendered with OptiX: a whole
// probe takes ~6 ms, per-process utilization attributed 98% SM to Blender, and
// per-process memory is attributable on Linux (it was not under WDDM).
//
// A process is ours when its cgroup is InferMux's own unit or one of
// Config.OurUnits. The models InferMux starts are its children, so they are
// ours without being listed. ComfyUI is deliberately not ours: its work
// competes for the same 10 GB, which is what makes it contention.
//
// /proc/<pid>/cmdline and /proc/<pid>/cgroup are world-readable, so this runs
// unprivileged. /proc/<pid>/exe is not, which is why names come from argv[0].

// Resources is one measurement, as the policy reads it. A nil pointer is a
// reading that was not taken, which is never evidence of contention.
type Resources struct {
	ForeignGPUPercent *float64  `json:"foreign_gpu_percent"`
	OurGPUPercent     float64   `json:"our_gpu_percent"`
	DesktopGPUPercent float64   `json:"desktop_gpu_percent"`
	OurVRAMMB         int       `json:"our_vram_mb"`
	VRAMUsedMB        int       `json:"vram_used_mb"`
	VRAMTotalMB       int       `json:"vram_total_mb"`
	VRAMFreeMB        *int      `json:"vram_free_mb"`
	GPUPercent        *float64  `json:"gpu_percent"`
	Culprits          []string  `json:"culprits"`
	Processes         []Process `json:"processes"`
	SampledAt         time.Time `json:"sampled_at"`
	ComfyUIJobs       *int      `json:"comfyui_jobs"`
}

type Process struct {
	PID     uint32  `json:"pid"`
	Name    string  `json:"name"`
	Unit    string  `json:"unit"`
	Percent float64 `json:"percent"`
	VRAMMB  int     `json:"vram_mb"`
	Ours    bool    `json:"ours"`
	Desktop bool    `json:"desktop"`
}

// Attribution decides whose a process is. Separate from the NVML calls so it is
// testable without a GPU.
type Attribution struct {
	OurUnits map[string]bool
	Desktop  map[string]bool
	Name     func(pid uint32) string
	Unit     func(pid uint32) string
}

func newAttribution(cfg Config, proc string) Attribution {
	ours := map[string]bool{}
	for _, u := range cfg.OurUnits {
		ours[u] = true
	}
	if self := ProcessUnit(proc, "self"); self != "" {
		ours[self] = true
	}
	desktop := map[string]bool{}
	for _, name := range cfg.DesktopProcesses {
		desktop[name] = true
	}
	return Attribution{
		OurUnits: ours,
		Desktop:  desktop,
		Name:     func(pid uint32) string { return ProcessName(proc, strconv.Itoa(int(pid))) },
		Unit:     func(pid uint32) string { return ProcessUnit(proc, strconv.Itoa(int(pid))) },
	}
}

// Summarise turns raw per-process readings into Resources.
func Summarise(vram map[uint32]int, util map[uint32]float64, usedMB, totalMB int, gpuPercent *float64, a Attribution) Resources {
	pids := map[uint32]bool{}
	for pid := range vram {
		pids[pid] = true
	}
	for pid := range util {
		pids[pid] = true
	}

	processes := make([]Process, 0, len(pids))
	for pid := range pids {
		unit := a.Unit(pid)
		name := a.Name(pid)
		ours := unit != "" && a.OurUnits[unit]
		processes = append(processes, Process{
			PID:     pid,
			Name:    name,
			Unit:    unit,
			Percent: util[pid],
			VRAMMB:  vram[pid],
			Ours:    ours,
			Desktop: !ours && a.Desktop[name],
		})
	}
	sort.Slice(processes, func(i, j int) bool {
		if processes[i].Percent != processes[j].Percent {
			return processes[i].Percent > processes[j].Percent
		}
		if processes[i].VRAMMB != processes[j].VRAMMB {
			return processes[i].VRAMMB > processes[j].VRAMMB
		}
		return processes[i].PID < processes[j].PID
	})

	var foreign, ours, desktop float64
	var ourVRAM int
	culprits := []string{}
	for _, p := range processes {
		switch {
		case p.Ours:
			ours += p.Percent
			ourVRAM += p.VRAMMB
		case p.Desktop:
			desktop += p.Percent
		default:
			foreign += p.Percent
			// Who to name in a verdict: the foreign processes doing work.
			if p.Percent > 0 && len(culprits) < 3 {
				culprits = append(culprits, p.Name)
			}
		}
	}
	foreign = round1(foreign)
	free := totalMB - usedMB
	// Capped: the reader wants who is using the GPU, not a census of every
	// compositor client on the box.
	if len(processes) > 10 {
		processes = processes[:10]
	}
	return Resources{
		ForeignGPUPercent: &foreign,
		OurGPUPercent:     round1(ours),
		DesktopGPUPercent: round1(desktop),
		OurVRAMMB:         ourVRAM,
		VRAMUsedMB:        usedMB,
		VRAMTotalMB:       totalMB,
		VRAMFreeMB:        &free,
		GPUPercent:        gpuPercent,
		Culprits:          culprits,
		Processes:         processes,
		SampledAt:         time.Now(),
	}
}

func round1(x float64) float64 { return math.Round(x*10) / 10 }

// ProcessName is argv[0]'s basename, with the nix wrapper's disguise taken off.
//
// makeWrapper renames the real binary .blender-wrapped, and comm cuts that to
// .blender-wrappe, so neither reads as the program's name.
//
// Chromium and Electron overwrite their argv with one space-joined string,
// "vesktop --gpu-preferences=...", so the name ends at the first " --" (seen
// live 2026-09-28). Splitting on the first space instead would cut
// "T3 Code (Alpha)" to "T3".
func ProcessName(proc, pid string) string {
	argv0 := ""
	if raw, err := os.ReadFile(filepath.Join(proc, pid, "cmdline")); err == nil {
		argv0, _, _ = strings.Cut(string(raw), "\x00")
	}
	if argv0 == "" {
		raw, err := os.ReadFile(filepath.Join(proc, pid, "comm"))
		if err != nil {
			return "unknown"
		}
		argv0 = strings.TrimSpace(string(raw))
	}
	head, _, _ := strings.Cut(argv0, " --")
	name := filepath.Base(head)
	name = strings.TrimPrefix(name, ".")
	for _, suffix := range []string{"-wrapped", "-wrappe"} {
		name = strings.TrimSuffix(name, suffix)
	}
	if name == "" || name == "/" || name == "." {
		return "unknown"
	}
	return name
}

// ProcessUnit is the last component of the process's cgroup path: a .service
// or a .scope. Empty when the process is gone or the file is unreadable.
func ProcessUnit(proc, pid string) string {
	raw, err := os.ReadFile(filepath.Join(proc, pid, "cgroup"))
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(raw), "\n")
	if i := strings.LastIndex(line, "/"); i >= 0 {
		return line[i+1:]
	}
	return ""
}
