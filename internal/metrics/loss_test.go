package metrics

import "testing"

// The regression this guards against: a closed lid producing hours of
// "100% packet loss" because local send failures were counted as network loss.
func TestSendErrExcludedFromLoss(t *testing.T) {
	var tl Tally
	for i := 0; i < 500; i++ {
		tl.Add(OutcomeSendErr)
	}
	if tl.Sent != 0 {
		t.Errorf("Sent = %d, want 0: send errors never reached the wire", tl.Sent)
	}
	if got := tl.LossRatio(); got != 0 {
		t.Errorf("LossRatio = %v, want 0 for an interface that was down", got)
	}
	if tl.SendErr != 500 {
		t.Errorf("SendErr = %d, want 500", tl.SendErr)
	}
}

func TestTallyCounts(t *testing.T) {
	var tl Tally
	for i := 0; i < 90; i++ {
		tl.Add(OutcomeOK)
	}
	for i := 0; i < 5; i++ {
		tl.Add(OutcomeLate)
	}
	for i := 0; i < 5; i++ {
		tl.Add(OutcomeNoReply)
	}

	if tl.Sent != 100 {
		t.Errorf("Sent = %d, want 100", tl.Sent)
	}
	if tl.Recv != 95 {
		t.Errorf("Recv = %d, want 95: late replies still arrived", tl.Recv)
	}
	if tl.Lost != 5 {
		t.Errorf("Lost = %d, want 5", tl.Lost)
	}
	if got := tl.LossRatio(); got != 0.05 {
		t.Errorf("LossRatio = %v, want 0.05", got)
	}
}

func TestWilsonWidensWithFewSamples(t *testing.T) {
	// Same 5% point estimate, two sample sizes. The small sample must produce
	// a visibly wider interval - that is the whole reason it is drawn.
	loSmall, hiSmall := WilsonInterval(1, 20, 1.96)
	loBig, hiBig := WilsonInterval(50, 1000, 1.96)

	small := hiSmall - loSmall
	big := hiBig - loBig
	if small <= big {
		t.Errorf("interval at n=20 (%.3f) not wider than at n=1000 (%.3f)", small, big)
	}
	if loSmall < 0 || hiSmall > 1 || loBig < 0 || hiBig > 1 {
		t.Error("interval escaped [0,1]")
	}
	if loBig > 0.05 || hiBig < 0.05 {
		t.Errorf("interval [%.3f,%.3f] excludes the point estimate 0.05", loBig, hiBig)
	}
}

func TestWilsonZeroObservations(t *testing.T) {
	lo, hi := WilsonInterval(0, 0, 1.96)
	if lo != 0 || hi != 0 {
		t.Errorf("got [%v,%v], want [0,0] when nothing was sent", lo, hi)
	}
}

func TestIPDVIgnoresGaps(t *testing.T) {
	var j IPDV
	j.Observe(20_000)
	j.Observe(21_000)
	// A packet was lost here; pairing across it would fabricate a 979ms swing.
	j.Reset()
	j.Observe(1_000_000)
	j.Observe(1_001_000)

	if j.N != 2 {
		t.Errorf("N = %d, want 2 consecutive pairs", j.N)
	}
	if j.MaxUS != 1000 {
		t.Errorf("MaxUS = %d, want 1000: the gap must not be measured", j.MaxUS)
	}
}
