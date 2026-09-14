package metrics

import "math"

// Outcome is why a probe ended the way it did. Collapsing these into a boolean
// is the classic way a monitor lies: a closed lid and a dropped packet are not
// the same event, and only one of them is the network's fault.
type Outcome uint8

const (
	OutcomeOK Outcome = iota
	OutcomeLate
	OutcomeNoReply
	OutcomeSendErr // local: interface down, no route. NOT packet loss.
	OutcomeUnreachable
	OutcomeTTLExceeded
	OutcomeDup
	OutcomeMalformed
)

func (o Outcome) String() string {
	switch o {
	case OutcomeOK:
		return "ok"
	case OutcomeLate:
		return "late"
	case OutcomeNoReply:
		return "no_reply"
	case OutcomeSendErr:
		return "send_err"
	case OutcomeUnreachable:
		return "unreachable"
	case OutcomeTTLExceeded:
		return "ttl_exceeded"
	case OutcomeDup:
		return "dup"
	case OutcomeMalformed:
		return "malformed"
	}
	return "unknown"
}

// Tally accumulates outcomes for one bucket.
type Tally struct {
	Sent    uint64 // attempts that actually left the machine
	Recv    uint64
	Late    uint64
	Lost    uint64
	SendErr uint64
}

func (t *Tally) Add(o Outcome) {
	switch o {
	case OutcomeSendErr:
		// Never counted as sent: the packet did not reach the wire, so it
		// cannot be evidence about the network. Without this, an overnight
		// suspend reads as hours of 100% packet loss.
		t.SendErr++
	case OutcomeOK:
		t.Sent++
		t.Recv++
	case OutcomeLate:
		t.Sent++
		t.Recv++
		t.Late++
	case OutcomeNoReply, OutcomeUnreachable:
		t.Sent++
		t.Lost++
	default:
		t.Sent++
	}
}

// LossRatio is lost/sent, with send errors already excluded by construction.
func (t *Tally) LossRatio() float64 {
	if t.Sent == 0 {
		return 0
	}
	return float64(t.Lost) / float64(t.Sent)
}

// WilsonInterval returns a confidence interval for the loss ratio.
//
// At twenty samples a minute, a raw loss percentage is a 0/5/10% sawtooth that
// looks like a failing connection but is only binomial noise. Rendering the
// interval instead of the point estimate is what keeps the dashboard honest at
// fine time resolution.
//
// z is the standard score for the desired confidence: 1.96 for 95%.
func WilsonInterval(lost, sent uint64, z float64) (lo, hi float64) {
	if sent == 0 {
		return 0, 0
	}
	n := float64(sent)
	p := float64(lost) / n
	z2 := z * z
	denom := 1 + z2/n
	centre := p + z2/(2*n)
	spread := z * math.Sqrt(p*(1-p)/n+z2/(4*n*n))
	lo = (centre - spread) / denom
	hi = (centre + spread) / denom

	// With zero observed losses the lower bound is algebraically zero but
	// lands on floating-point dust (~1e-18), which serialises into JSON as a
	// startling "4.6e-18" loss rate. Snap it.
	if lo < 1e-12 {
		lo = 0
	}
	return math.Max(0, lo), math.Min(1, hi)
}
