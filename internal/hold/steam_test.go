package hold

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// The test binary, copied to a Steam root's ubuntu12_32/reaper, is a game's
// reaper there: it runs until its stdin closes.
func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == "reaper" {
		io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// steamRoot is a Steam root whose reaper is this test binary.
func steamRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "ubuntu12_32"), 0o755); err != nil {
		t.Fatal(err)
	}
	installReaper(t, root)
	return root
}

// installReaper writes this test binary to root's reaper, a new file renamed
// over the one before.
func installReaper(t *testing.T, root string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "ubuntu12_32")
	if err := os.WriteFile(filepath.Join(dir, "reaper.new"), b, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "reaper.new"), filepath.Join(dir, "reaper")); err != nil {
		t.Fatal(err)
	}
}

// reaper is a start of a root's reaper, as Steam's client makes one.
type reaper struct {
	cmd *exec.Cmd
	in  io.Closer
}

func launch(t *testing.T, root string, args ...string) *reaper {
	t.Helper()
	cmd := exec.Command(filepath.Join(root, "ubuntu12_32", "reaper"), args...)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		in.Close()
		cmd.Wait()
	})
	return &reaper{cmd: cmd, in: in}
}

// exit ends the reaper, which stays a zombie until the test ends.
func (r *reaper) exit() { r.in.Close() }

func (r *reaper) pid() int { return r.cmd.Process.Pid }

type watcher struct {
	games  chan Game
	done   chan error
	cancel context.CancelFunc
}

// watch runs WatchSteam on root.
func watch(t *testing.T, root string) *watcher {
	ctx, cancel := context.WithCancel(context.Background())
	w := &watcher{games: make(chan Game, 16), done: make(chan error, 1), cancel: cancel}
	go func() { w.done <- WatchSteam(ctx, root, func(g Game) { w.games <- g }) }()
	t.Cleanup(cancel)
	return w
}

// next is the next game reported.
func (w *watcher) next(t *testing.T) Game {
	t.Helper()
	select {
	case g := <-w.games:
		return g
	case err := <-w.done:
		t.Fatalf("WatchSteam returned: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("no game reported in 2 s")
	}
	return Game{}
}

// stop ends the watch and returns the games reported and not yet taken.
func (w *watcher) stop(t *testing.T) []Game {
	t.Helper()
	w.cancel()
	if err := <-w.done; err != nil {
		t.Fatalf("WatchSteam: %v", err)
	}
	var rest []Game
	for {
		select {
		case g := <-w.games:
			rest = append(rest, g)
		default:
			return rest
		}
	}
}

// A game already running is reported at once, and one that starts as its
// reaper starts. A game is reported once, however many scans see it.
func TestWatchSteam_ReportsTheGamesRunningAndEachThatStarts(t *testing.T) {
	root := steamRoot(t)
	running := launch(t, root, "SteamLaunch", "AppId=1", "--", "game")
	w := watch(t, root)
	// Found by the first scan, so the watch is in place.
	if g := w.next(t); g.PID != running.pid() || g.AppID != "1" {
		t.Fatalf("%v, want app 1 with reaper %d", g, running.pid())
	}
	// Nothing reads /proc again but an open of the reaper.
	started := launch(t, root, "SteamLaunch", "AppId=1783560", "--", "game")
	if g := w.next(t); g.PID != started.pid() || g.AppID != "1783560" {
		t.Fatalf("%v, want app 1783560 with reaper %d", g, started.pid())
	}
	if rest := w.stop(t); len(rest) != 0 {
		t.Fatalf("also reported: %v", rest)
	}
}

// Only this Steam's launches are games: neither another install's reaper
// nor this one's without SteamLaunch and an app ID before "--".
func TestWatchSteam_OnlyThisSteamsLaunchesAreGames(t *testing.T) {
	root, other := steamRoot(t), steamRoot(t)
	w := watch(t, root)
	launch(t, other, "SteamLaunch", "AppId=7", "--", "game")
	launch(t, root, "AppId=8", "--", "game")
	launch(t, root, "SteamLaunch", "--", "AppId=9")
	launch(t, root, "SteamLaunch", "AppId=", "--", "game")
	// Every scan that finds this one sees the others too.
	game := launch(t, root, "SteamLaunch", "AppId=42", "--", "game")
	if g := w.next(t); g.PID != game.pid() {
		t.Fatalf("%v, want reaper %d", g, game.pid())
	}
	if rest := w.stop(t); len(rest) != 0 {
		t.Fatalf("also reported: %v", rest)
	}
}

// A reaper written anew, as by an update of Steam, is a new file, and the
// one watched from then on.
func TestWatchSteam_AReaperWrittenAnewIsWatched(t *testing.T) {
	root := steamRoot(t)
	launch(t, root, "SteamLaunch", "AppId=1", "--", "game")
	w := watch(t, root)
	w.next(t)
	installReaper(t, root)
	game := launch(t, root, "SteamLaunch", "AppId=2", "--", "game")
	if g := w.next(t); g.PID != game.pid() {
		t.Fatalf("%v, want reaper %d", g, game.pid())
	}
}

// With no reaper's directory to watch, or once it is gone, WatchSteam
// returns an error.
func TestWatchSteam_NoDirectoryIsAnError(t *testing.T) {
	if err := WatchSteam(context.Background(), t.TempDir(), func(Game) {}); err == nil {
		t.Fatal("no error for a root without ubuntu12_32")
	}
	exitPoll = 10 * time.Millisecond
	t.Cleanup(func() { exitPoll = time.Second })
	root := steamRoot(t)
	r := launch(t, root, "SteamLaunch", "AppId=1", "--", "game")
	w := watch(t, root)
	g := w.next(t)
	// The kernel reports a directory's removal once no process holds a file
	// in it, and a reaper holds its own until it exits.
	r.exit()
	g.Wait(context.Background())
	if err := os.RemoveAll(filepath.Join(root, "ubuntu12_32")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-w.done:
		if err == nil {
			t.Fatal("nil once the directory went away")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("still watching a directory that went away")
	}
}

// Wait returns once the reaper has exited, while it is a zombie too.
func TestGame_WaitReturnsOnceTheReaperExits(t *testing.T) {
	exitPoll = 10 * time.Millisecond
	t.Cleanup(func() { exitPoll = time.Second })
	root := steamRoot(t)
	r := launch(t, root, "SteamLaunch", "AppId=1", "--", "game")
	games := steamGames(filepath.Join(root, "ubuntu12_32", "reaper"))
	if len(games) != 1 {
		t.Fatalf("%v", games)
	}
	waited := make(chan struct{})
	go func() {
		games[0].Wait(context.Background())
		close(waited)
	}()
	select {
	case <-waited:
		t.Fatal("Wait returned while the reaper runs")
	case <-time.After(100 * time.Millisecond):
	}
	r.exit()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait still waits for a reaper that exited")
	}
}

// A PID that another process has taken is not the game's.
func TestGame_APIDTakenAgainIsNotTheGame(t *testing.T) {
	_, start, err := procStat(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	g := Game{PID: os.Getpid(), start: start + "0"}
	if g.running() {
		t.Fatal("a process with another start time is taken for the game")
	}
	g.start = start
	if !g.running() {
		t.Fatal("the game's own process is not running")
	}
}

// Wait returns when ctx ends, the game still running.
func TestGame_WaitEndsWithItsContext(t *testing.T) {
	_, start, err := procStat(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	waited := make(chan struct{})
	go func() {
		Game{PID: os.Getpid(), start: start}.Wait(ctx)
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait outlived its context")
	}
}
