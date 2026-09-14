// Package clock separates wall-clock time from elapsed time so that machine
// sleep and NTP steps are observable rather than silently corrupting samples.
package clock

import (
	"time"

	"golang.org/x/sys/unix"
)

// Reading pairs the two clocks that matter on Darwin.
//
// CLOCK_REALTIME advances across sleep and can jump when NTP corrects it.
// CLOCK_UPTIME_RAW does not advance while the machine is asleep and is never
// stepped. Comparing how much each one moved between two readings is what
// separates "the laptop was suspended" from "time was corrected" from "the
// process was just slow".
type Reading struct {
	Wall   time.Time
	Uptime time.Duration
}

func Now() Reading {
	var wall, up unix.Timespec
	// Errors here would mean the kernel lacks clocks we already depend on.
	_ = unix.ClockGettime(unix.CLOCK_REALTIME, &wall)
	_ = unix.ClockGettime(unix.CLOCK_UPTIME_RAW, &up)
	return Reading{
		Wall:   time.Unix(wall.Sec, wall.Nsec),
		Uptime: time.Duration(up.Sec)*time.Second + time.Duration(up.Nsec),
	}
}

// Discontinuity describes what happened between two Readings.
type Discontinuity struct {
	Slept   time.Duration // wall advanced while uptime did not
	Stepped time.Duration // wall moved without matching uptime, in either direction
	Elapsed time.Duration // real elapsed time, from the monotonic clock
	IsSleep bool
	IsStep  bool
}

// Compare reports what happened between two readings.
//
// tolerance absorbs ordinary scheduling jitter; anything beyond it is treated
// as a real discontinuity. Sleep and a forward NTP step look identical in the
// wall clock alone, which is precisely why both clocks are read.
func Compare(from, to Reading, tolerance time.Duration) Discontinuity {
	elapsed := to.Uptime - from.Uptime
	wallDelta := to.Wall.Sub(from.Wall)
	skew := wallDelta - elapsed

	d := Discontinuity{Elapsed: elapsed}
	switch {
	case skew > tolerance:
		// Wall time ran ahead of elapsed time: the machine was suspended, or
		// the clock was stepped forward. Darwin does not advance UPTIME_RAW
		// during sleep, so we attribute a positive skew to sleep.
		d.Slept = skew
		d.IsSleep = true
	case skew < -tolerance:
		// Wall time went backwards relative to elapsed time. Only an NTP
		// correction does this.
		d.Stepped = skew
		d.IsStep = true
	}
	return d
}
