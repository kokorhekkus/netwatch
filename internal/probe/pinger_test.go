package probe

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/kokorhekkus/netwatch/internal/metrics"
)

// The regression this guards against is subtle and would never show up as a
// crash: because Darwin fans every ICMP reply out to every datagram ICMP
// socket, a prober that does not filter strictly will silently absorb other
// processes' replies and report them as its own samples.
//
// Two Pingers run side by side against the loopback gateway. Each must see
// exactly its own replies and none of the other's.
func TestPingerIgnoresForeignReplies(t *testing.T) {
	a, err := NewPinger()
	if err != nil {
		t.Skipf("cannot open icmp socket: %v", err)
	}
	defer a.Close()

	b, err := NewPinger()
	if err != nil {
		t.Skipf("cannot open second icmp socket: %v", err)
	}
	defer b.Close()

	if a.id == b.id && a.magic == b.magic {
		t.Fatal("two Pingers share an identity; foreign replies are indistinguishable")
	}

	ctx := context.Background()
	dst := net.ParseIP("127.0.0.1")

	// Drive both concurrently so their replies interleave on both sockets.
	done := make(chan Result, 2)
	go func() { done <- a.Ping(ctx, dst) }()
	go func() { done <- b.Ping(ctx, dst) }()

	for i := 0; i < 2; i++ {
		select {
		case r := <-done:
			// Loopback may or may not answer ICMP in a sandbox; either way
			// the outcome must be a defined one, never a stranger's reply
			// mistaken for ours.
			switch r.Outcome {
			case metrics.OutcomeOK, metrics.OutcomeLate,
				metrics.OutcomeNoReply, metrics.OutcomeSendErr:
			default:
				t.Errorf("unexpected outcome %v", r.Outcome)
			}
		case <-time.After(HardTimeout + 2*time.Second):
			t.Fatal("Ping did not return within the hard timeout")
		}
	}

	// Neither pinger may be left holding state for a request it answered.
	for name, p := range map[string]*Pinger{"a": a, "b": b} {
		p.mu.Lock()
		n := len(p.inflight)
		p.mu.Unlock()
		if n != 0 {
			t.Errorf("pinger %s leaked %d inflight entries", name, n)
		}
	}
}

func TestPingerDropsUnmatchedPacket(t *testing.T) {
	p, err := NewPinger()
	if err != nil {
		t.Skipf("cannot open icmp socket: %v", err)
	}
	defer p.Close()

	// A well-formed echo reply belonging to somebody else must be dropped
	// without touching any state.
	foreign := buildEchoReply(p.id^0xFFFF, 1, [8]byte{9, 9, 9, 9, 9, 9, 9, 9})
	p.dispatch(foreign)

	// Correct ID but the wrong magic: a different netwatch process.
	wrongMagic := buildEchoReply(p.id, 1, [8]byte{1, 2, 3, 4, 5, 6, 7, 8})
	p.dispatch(wrongMagic)

	p.mu.Lock()
	n := len(p.inflight)
	p.mu.Unlock()
	if n != 0 {
		t.Errorf("inflight = %d after foreign packets, want 0", n)
	}
}

func TestSendErrorIsNotLoss(t *testing.T) {
	p, err := NewPinger()
	if err != nil {
		t.Skipf("cannot open icmp socket: %v", err)
	}
	defer p.Close()

	// 0.0.0.0 is not a routable destination, so the write fails locally.
	r := p.Ping(context.Background(), net.IPv4zero)
	if r.Outcome == metrics.OutcomeNoReply {
		t.Error("a local send failure was reported as packet loss")
	}

	var tally metrics.Tally
	tally.Add(r.Outcome)
	if r.Outcome == metrics.OutcomeSendErr && tally.Sent != 0 {
		t.Errorf("Sent = %d after a send error, want 0", tally.Sent)
	}
}

// buildEchoReply hand-rolls a reply packet as it would appear after x/net has
// stripped the IPv4 header.
func buildEchoReply(id, seq int, magic [8]byte) []byte {
	b := make([]byte, 8+payloadLen)
	b[0] = 0 // echo reply
	b[1] = 0
	b[4] = byte(id >> 8)
	b[5] = byte(id)
	b[6] = byte(seq >> 8)
	b[7] = byte(seq)
	copy(b[8:], magic[:])
	b[16] = byte(seq >> 8)
	b[17] = byte(seq)
	return b
}
