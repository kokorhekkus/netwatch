package metrics

// IPDV accumulates inter-packet delay variation per RFC 3393: the absolute
// difference in one-way delay between *consecutive* packets.
//
// Standard deviation of RTT is the usual shortcut and it is a poor proxy for
// what a call actually experiences. A link that alternates 20ms/200ms every
// packet and one that sits at 20ms then steps to 200ms for a minute have
// similar standard deviations but feel entirely different. IPDV separates them.
type IPDV struct {
	SumUS int64
	N     int64
	MaxUS int64

	hasPrev bool
	prevUS  int64
}

// Observe records the next RTT in sequence.
//
// Gaps must be signalled with Reset: pairing samples across a lost packet or a
// sleep would invent a delay variation that never happened.
func (j *IPDV) Observe(rttUS int64) {
	if j.hasPrev {
		d := rttUS - j.prevUS
		if d < 0 {
			d = -d
		}
		j.SumUS += d
		j.N++
		if d > j.MaxUS {
			j.MaxUS = d
		}
	}
	j.prevUS = rttUS
	j.hasPrev = true
}

// Reset breaks the chain of consecutive samples.
func (j *IPDV) Reset() { j.hasPrev = false }

func (j *IPDV) Mean() float64 {
	if j.N == 0 {
		return 0
	}
	return float64(j.SumUS) / float64(j.N)
}
