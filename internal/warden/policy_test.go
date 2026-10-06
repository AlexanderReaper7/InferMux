package warden

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func f64(x float64) *float64 { return &x }
func ip(x int) *int          { return &x }

func quiet() Resources {
	return Resources{ForeignGPUPercent: f64(0), VRAMFreeMB: ip(8000)}
}

func busy(percent float64, culprits ...string) Resources {
	r := quiet()
	r.ForeignGPUPercent = f64(percent)
	r.Culprits = culprits
	return r
}

func TestAGameOnTheCardIsContentionAndSaysWhichOne(t *testing.T) {
	ok, why := IsContended(busy(80, "blender"), true, DefaultConfig().Policy)
	if !ok || !strings.Contains(why, "blender") || !strings.Contains(why, "80%") {
		t.Fatalf("got %v %q", ok, why)
	}
}

func TestLoadBelowTheThresholdIsNotContention(t *testing.T) {
	if ok, why := IsContended(busy(24), true, DefaultConfig().Policy); ok {
		t.Fatalf("contended: %q", why)
	}
}

func TestFreeVRAMIsOnlyReadWhenNoModelIsLoaded(t *testing.T) {
	r := quiet()
	r.VRAMFreeMB = ip(1000)
	p := DefaultConfig().Policy
	if ok, _ := IsContended(r, true, p); ok {
		t.Fatal("low free VRAM counted while our model holds it")
	}
	if ok, why := IsContended(r, false, p); !ok || !strings.Contains(why, "1000 MB") {
		t.Fatalf("got %v %q", ok, why)
	}
}

func TestAMissingReadingIsNotEvidenceOfContention(t *testing.T) {
	if ok, why := IsContended(Resources{}, false, DefaultConfig().Policy); ok {
		t.Fatalf("contended: %q", why)
	}
}

func TestAQueuedComfyUIJobIsContentionBeforeItDrawsAnyPower(t *testing.T) {
	r := quiet()
	r.ComfyUIJobs = ip(1)
	ok, why := IsContended(r, true, DefaultConfig().Policy)
	if !ok || why != "ComfyUI has 1 job queued" {
		t.Fatalf("got %v %q", ok, why)
	}
}

func TestYieldingIsImmediate(t *testing.T) {
	v := Decide(initialVerdict(), busy(50), t0, true, DefaultConfig().Policy)
	if !v.Yielded || !v.Since.Equal(t0) {
		t.Fatalf("got %+v", v)
	}
}

func TestResumingWaitsOutTheQuietWindow(t *testing.T) {
	p := DefaultConfig().Policy
	v := Decide(initialVerdict(), busy(50), t0, true, p)
	v = Decide(v, quiet(), t0.Add(299*time.Second), true, p)
	if !v.Yielded || !strings.Contains(v.Reason, "only for 299s of 300s") {
		t.Fatalf("resumed early: %+v", v)
	}
	v = Decide(v, quiet(), t0.Add(300*time.Second), true, p)
	if v.Yielded {
		t.Fatalf("did not resume: %+v", v)
	}
}

func TestTheWindowIsMeasuredFromTheLastBusySampleNotFromThePause(t *testing.T) {
	p := DefaultConfig().Policy
	v := Decide(initialVerdict(), busy(50), t0, true, p)
	v = Decide(v, busy(50), t0.Add(250*time.Second), true, p)
	v = Decide(v, quiet(), t0.Add(320*time.Second), true, p)
	if !v.Yielded {
		t.Fatal("resumed 70s after the last busy sample")
	}
	if !v.Since.Equal(t0) {
		t.Fatalf("since moved to %v", v.Since)
	}
}

func TestAPauseWithNoObservationBehindItCanStillLift(t *testing.T) {
	v := Decide(Verdict{Yielded: true}, quiet(), t0, true, DefaultConfig().Policy)
	if v.Yielded {
		t.Fatal("stuck paused")
	}
}

func TestComfyUIIsFreedOnceAfterTheIdleWindow(t *testing.T) {
	p := DefaultConfig().Policy
	state := ComfyIdle{BusyAt: t0}
	state, due := ComfyUIFreeDue(state, ip(0), t0.Add(599*time.Second), p)
	if due {
		t.Fatal("freed early")
	}
	state, due = ComfyUIFreeDue(state, ip(0), t0.Add(600*time.Second), p)
	if !due || !state.Freed {
		t.Fatal("not freed after the window")
	}
	if _, due = ComfyUIFreeDue(state, ip(0), t0.Add(2000*time.Second), p); due {
		t.Fatal("freed twice in one idle stretch")
	}
}

func TestAJobRestartsTheIdleClock(t *testing.T) {
	p := DefaultConfig().Policy
	state := ComfyIdle{BusyAt: t0, Freed: true}
	state, _ = ComfyUIFreeDue(state, ip(2), t0.Add(1000*time.Second), p)
	if state.Freed || !state.BusyAt.Equal(t0.Add(1000*time.Second)) {
		t.Fatalf("got %+v", state)
	}
}

func TestAnUnreadableQueueMovesNothing(t *testing.T) {
	state := ComfyIdle{BusyAt: t0}
	after, due := ComfyUIFreeDue(state, nil, t0.Add(time.Hour), DefaultConfig().Policy)
	if due || after != state {
		t.Fatalf("got %+v %v", after, due)
	}
}

func obsWithCUDA() Resources {
	r := busy(7, "obs")
	r.Priority = []string{"obs"}
	return r
}

// OBS's green screen draws 6-8% SM, far under gpu_busy_percent (0017).
func TestAPriorityProcessIsContentionAtAnyLoad(t *testing.T) {
	ok, why := IsContended(obsWithCUDA(), true, DefaultConfig().Policy)
	if !ok || !strings.Contains(why, "obs") {
		t.Fatalf("got %v %q", ok, why)
	}
	v := Decide(initialVerdict(), obsWithCUDA(), t0, true, DefaultConfig().Policy)
	if !v.Yielded || !v.Priority {
		t.Fatalf("got %+v", v)
	}
}

func TestThePriorityEndsWithItsProcessButThePauseWaitsItsWindow(t *testing.T) {
	p := DefaultConfig().Policy
	v := Decide(initialVerdict(), obsWithCUDA(), t0, true, p)
	v = Decide(v, quiet(), t0.Add(time.Minute), true, p)
	if !v.Yielded || v.Priority {
		t.Fatalf("got %+v", v)
	}
}
