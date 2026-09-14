package clock

import (
	"testing"
	"time"
)

func at(wallSec int64, uptime time.Duration) Reading {
	return Reading{Wall: time.Unix(wallSec, 0), Uptime: uptime}
}

func TestOrdinaryTickIsNotADiscontinuity(t *testing.T) {
	from := at(1000, 10*time.Second)
	// Both clocks advanced together, with a little scheduling jitter.
	to := at(1002, 12*time.Second+300*time.Millisecond)

	d := Compare(from, to, 5*time.Second)
	if d.IsSleep || d.IsStep {
		t.Errorf("ordinary tick flagged as discontinuity: %+v", d)
	}
}

// An overnight suspend: wall time advanced eight hours, uptime did not.
// Getting this wrong turns one gap into eight hours of fabricated packet loss.
func TestSleepDetected(t *testing.T) {
	from := at(1000, 10*time.Second)
	to := at(1000+8*3600, 12*time.Second)

	d := Compare(from, to, 5*time.Second)
	if !d.IsSleep {
		t.Fatal("suspend not detected")
	}
	if d.IsStep {
		t.Error("suspend also reported as a clock step")
	}
	// Slept should be close to the eight hours the wall clock gained.
	want := 8*time.Hour - 2*time.Second
	if d.Slept != want {
		t.Errorf("Slept = %v, want %v", d.Slept, want)
	}
	if d.Elapsed != 2*time.Second {
		t.Errorf("Elapsed = %v, want 2s of real running time", d.Elapsed)
	}
}

// NTP correcting the clock backwards must not be mistaken for anything else,
// and must never produce a negative-duration gap record.
func TestBackwardStepDetected(t *testing.T) {
	from := at(1000, 10*time.Second)
	to := at(970, 12*time.Second)

	d := Compare(from, to, 5*time.Second)
	if !d.IsStep {
		t.Fatal("backward clock step not detected")
	}
	if d.IsSleep {
		t.Error("backward step reported as sleep")
	}
	if d.Stepped >= 0 {
		t.Errorf("Stepped = %v, want a negative duration", d.Stepped)
	}
}

func TestToleranceAbsorbsJitter(t *testing.T) {
	from := at(1000, 10*time.Second)
	// Wall ran 3s ahead of uptime: real but within tolerance.
	to := at(1005, 12*time.Second)

	if d := Compare(from, to, 5*time.Second); d.IsSleep || d.IsStep {
		t.Errorf("3s skew exceeded 5s tolerance: %+v", d)
	}
	if d := Compare(from, to, time.Second); !d.IsSleep {
		t.Error("3s skew not flagged at 1s tolerance")
	}
}

// The real clocks must behave as the detector assumes: both advance, and
// neither jumps while the process is simply running.
func TestNowAdvancesBothClocks(t *testing.T) {
	a := Now()
	time.Sleep(20 * time.Millisecond)
	b := Now()

	if !b.Wall.After(a.Wall) {
		t.Error("CLOCK_REALTIME did not advance")
	}
	if b.Uptime <= a.Uptime {
		t.Error("CLOCK_UPTIME_RAW did not advance")
	}
	if d := Compare(a, b, time.Second); d.IsSleep || d.IsStep {
		t.Errorf("a 20ms sleep looked like a discontinuity: %+v", d)
	}
}
