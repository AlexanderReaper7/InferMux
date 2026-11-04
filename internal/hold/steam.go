package hold

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Steam's client starts every game under its reaper, as
//
//	<root>/ubuntu12_32/reaper SteamLaunch AppId=1783560 -- <the game's command>
//
// The reaper is the launch's first program, 1.7 s before the game's own
// (The Last Caretaker, 2026-11-04), and lives until the game's last process
// has exited. Steam has no launch option for every game, so WatchSteam
// watches the reaper's directory instead: an exec opens the file, and
// inotify reports the open to a watcher of the same user, from inside
// Steam's bwrap too (0024, 6).

// Game is a game Steam started, known by its reaper.
type Game struct {
	PID   int
	AppID string
	// start is the reaper's start time, so that a PID used again is
	// another process.
	start string
}

func (g Game) String() string {
	return fmt.Sprintf("Steam app %s (reaper %d)", g.AppID, g.PID)
}

// rescans are when /proc is read after an open of the reaper: the open
// comes while the exec is under way, before /proc shows the new program.
var rescans = []time.Duration{10 * time.Millisecond, 100 * time.Millisecond, time.Second}

// exitPoll is how often Wait reads whether a game's reaper still runs.
var exitPoll = time.Second

// WatchSteam calls started once for each game Steam starts under root,
// ~/.steam/root, and for each one already running, until ctx ends; started
// must not block. WatchSteam returns nil then, or with why root's games
// cannot be watched: no such directory, or one that went away.
func WatchSteam(ctx context.Context, root string, started func(Game)) error {
	dir := filepath.Join(root, "ubuntu12_32")
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC | syscall.IN_NONBLOCK)
	if err != nil {
		return fmt.Errorf("inotify: %w", err)
	}
	// Non-blocking, so the runtime polls it and Close ends a Read.
	events := os.NewFile(uintptr(fd), "inotify")
	defer events.Close()
	// The watch before the first scan: a reaper that starts between them is
	// found by both and reported once, never by neither.
	if _, err := syscall.InotifyAddWatch(fd, dir, syscall.IN_OPEN|syscall.IN_DELETE_SELF|syscall.IN_MOVE_SELF); err != nil {
		return fmt.Errorf("watch %s: %w", dir, err)
	}
	opened := make(chan struct{}, 1)
	failed := make(chan error, 1)
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, err := events.Read(buf)
			if err != nil {
				failed <- err
				return
			}
			reaper, gone := readEvents(buf[:n])
			if gone {
				failed <- fmt.Errorf("%s went away", dir)
				return
			}
			if reaper {
				select {
				case opened <- struct{}{}:
				default:
				}
			}
		}
	}()

	reaper := filepath.Join(dir, "reaper")
	seen := map[Game]bool{}
	scan := func() {
		now := map[Game]bool{}
		for _, g := range steamGames(reaper) {
			now[g] = true
			if !seen[g] {
				started(g)
			}
		}
		seen = now
	}
	scan()
	var due []time.Time
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failed:
			return err
		case <-opened:
			now := time.Now()
			for _, d := range rescans {
				due = append(due, now.Add(d))
			}
		case <-timer.C:
			scan()
			now := time.Now()
			due = slices.DeleteFunc(due, func(t time.Time) bool { return !t.After(now) })
		}
		if len(due) > 0 {
			timer.Reset(time.Until(slices.MinFunc(due, time.Time.Compare)))
		}
	}
}

// readEvents says whether buf, inotify's events, has an open of the reaper,
// or the end of the directory's watch. A queue that overflowed may have
// lost an open, so it counts as one.
func readEvents(buf []byte) (reaper, gone bool) {
	const header = 16 // wd, mask, cookie and len, four bytes each
	for len(buf) >= header {
		mask := binary.NativeEndian.Uint32(buf[4:])
		n := header + int(binary.NativeEndian.Uint32(buf[12:]))
		if n > len(buf) {
			break
		}
		name := string(bytes.TrimRight(buf[header:n], "\x00"))
		buf = buf[n:]
		switch {
		case mask&(syscall.IN_DELETE_SELF|syscall.IN_MOVE_SELF|syscall.IN_IGNORED|syscall.IN_UNMOUNT) != 0:
			gone = true
		case mask&syscall.IN_Q_OVERFLOW != 0, mask&syscall.IN_OPEN != 0 && name == "reaper":
			reaper = true
		}
	}
	return reaper, gone
}

// steamGames are the games running under reaper: each process that runs
// that file with SteamLaunch and AppId=<id> among its arguments before
// "--". The file, not its path, so that the reaper of another Steam
// install is not taken for this one's.
func steamGames(reaper string) []Game {
	want, err := os.Stat(reaper)
	if err != nil {
		return nil
	}
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var games []Game
	for _, p := range procs {
		pid, err := strconv.Atoi(p.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile("/proc/" + p.Name() + "/cmdline")
		if err != nil {
			continue
		}
		app, ok := steamLaunch(strings.Split(string(cmdline), "\x00"))
		if !ok {
			continue
		}
		exe, err := os.Stat("/proc/" + p.Name() + "/exe")
		if err != nil || !os.SameFile(exe, want) {
			continue
		}
		state, start, err := procStat(pid)
		if err != nil || state == "Z" {
			continue
		}
		games = append(games, Game{PID: pid, AppID: app, start: start})
	}
	return games
}

// steamLaunch is the app ID of a reaper's command line, argv, if it is one
// of Steam's launches.
func steamLaunch(argv []string) (app string, ok bool) {
	if len(argv) < 2 {
		return "", false
	}
	launch := false
	for _, a := range argv[1:] {
		if a == "--" {
			break
		}
		if a == "SteamLaunch" {
			launch = true
		}
		if id, found := strings.CutPrefix(a, "AppId="); found {
			app = id
		}
	}
	return app, launch && app != ""
}

// Wait returns once the game's reaper has exited, which it does after the
// game's last process, or once ctx ends.
func (g Game) Wait(ctx context.Context) {
	for g.running() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(exitPoll):
		}
	}
}

// running is whether the reaper is still the process with g's PID and has
// not exited. A zombie has: its parent has only not waited for it yet.
func (g Game) running() bool {
	state, start, err := procStat(g.PID)
	return err == nil && start == g.start && state != "Z" && state != "X"
}

// procStat is a process's state and start time, fields 3 and 22 of
// /proc/<pid>/stat. The name before them is in parentheses and may hold any
// character, so they are counted from the last ')'.
func procStat(pid int) (state, start string, err error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", "", err
	}
	f := strings.Fields(string(b[bytes.LastIndexByte(b, ')')+1:]))
	if len(f) < 20 {
		return "", "", fmt.Errorf("/proc/%d/stat has %d fields after the name", pid, len(f))
	}
	return f[0], f[19], nil
}
