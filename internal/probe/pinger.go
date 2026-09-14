// Package probe measures the network.
package probe

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"

	"github.com/kokorhekkus/netwatch/internal/metrics"
)

// Timeouts. The hard deadline is deliberately generous: with a 1s deadline on
// a link whose tail latency reaches 900ms you manufacture packet loss out of
// slowness. Replies arriving after the soft threshold are recorded as `late`,
// which counts as received but is tracked separately.
const (
	SoftTimeout = 1 * time.Second
	HardTimeout = 3 * time.Second
)

// payloadMagic tags our packets. See Pinger for why this is not optional.
const payloadLen = 16 // 8 magic + 2 seq + 6 padding

type Result struct {
	RTT     time.Duration
	Outcome metrics.Outcome
}

type inflight struct {
	sentAt time.Time
	ch     chan Result
}

// Pinger sends ICMP echoes over one shared unprivileged datagram socket.
//
// Darwin delivers every ICMP reply to every open datagram ICMP socket on the
// machine, not just the one that sent the matching request. This was verified
// directly: two sockets each sent one echo, and both received both replies;
// a socket that sent nothing at all still received another socket's
// TimeExceeded. So a reply arriving here may belong to another process
// entirely - a `ping` running in a terminal, or another copy of this daemon.
//
// Two consequences shape this type:
//
//   - Filtering on our own echo ID *and* a per-process payload magic is
//     required for correctness, not merely good hygiene. Without it the
//     prober counts strangers' packets as its own samples.
//   - An unmatched reply is completely normal and must be discarded in
//     silence. It is not an error, not a duplicate, and must never reach a
//     counter or a log line.
//
// Because delivery is promiscuous anyway, one shared socket for all targets is
// both simpler and no less correct than one socket each.
type Pinger struct {
	conn  *icmp.PacketConn
	id    int
	magic [8]byte

	seq atomic.Uint32

	mu       sync.Mutex
	inflight map[uint16]*inflight

	closeOnce sync.Once
	done      chan struct{}
}

func NewPinger() (*Pinger, error) {
	conn, err := icmp.ListenPacket("udp4", "0.0.0.0")
	if err != nil {
		return nil, fmt.Errorf("open icmp socket: %w", err)
	}

	p := &Pinger{
		conn:     conn,
		id:       int(uint16(time.Now().UnixNano())),
		inflight: make(map[uint16]*inflight),
		done:     make(chan struct{}),
	}
	if _, err := rand.Read(p.magic[:]); err != nil {
		conn.Close()
		return nil, err
	}

	go p.readLoop()
	return p, nil
}

func (p *Pinger) Close() error {
	var err error
	p.closeOnce.Do(func() {
		close(p.done)
		err = p.conn.Close()
	})
	return err
}

// Ping sends one echo request and waits for its reply.
func (p *Pinger) Ping(ctx context.Context, dst net.IP) Result {
	seq := uint16(p.seq.Add(1))

	payload := make([]byte, payloadLen)
	copy(payload, p.magic[:])
	binary.BigEndian.PutUint16(payload[8:], seq)

	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{ID: p.id, Seq: int(seq), Data: payload},
	}
	wire, err := msg.Marshal(nil)
	if err != nil {
		return Result{Outcome: metrics.OutcomeMalformed}
	}

	ch := make(chan Result, 1)
	p.mu.Lock()
	p.inflight[seq] = &inflight{sentAt: time.Now(), ch: ch}
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		delete(p.inflight, seq)
		p.mu.Unlock()
	}()

	if _, err := p.conn.WriteTo(wire, &net.UDPAddr{IP: dst}); err != nil {
		// The packet never reached the wire. This is local state - the
		// interface is down, or there is no route - and says nothing about
		// the network, so it must not be counted as loss.
		return Result{Outcome: metrics.OutcomeSendErr}
	}

	timer := time.NewTimer(HardTimeout)
	defer timer.Stop()

	select {
	case r := <-ch:
		return r
	case <-timer.C:
		return Result{Outcome: metrics.OutcomeNoReply}
	case <-ctx.Done():
		return Result{Outcome: metrics.OutcomeSendErr}
	case <-p.done:
		return Result{Outcome: metrics.OutcomeSendErr}
	}
}

func (p *Pinger) readLoop() {
	buf := make([]byte, 1500)
	for {
		select {
		case <-p.done:
			return
		default:
		}

		// A read deadline keeps this loop responsive to Close.
		_ = p.conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, _, err := p.conn.ReadFrom(buf)
		if err != nil {
			var nerr net.Error
			if errors.As(err, &nerr) && nerr.Timeout() {
				continue
			}
			return // socket closed
		}
		p.dispatch(buf[:n])
	}
}

// dispatch matches one received packet to a waiting request, or drops it.
//
// x/net/icmp strips the IPv4 header that Darwin prepends to datagram ICMP
// reads, so the buffer starts at the ICMP type and ParseMessage can be called
// directly. (A hand-rolled unix.Socket would need to skip IHL*4 bytes first.)
func (p *Pinger) dispatch(b []byte) {
	msg, err := icmp.ParseMessage(ipv4.ICMPTypeEchoReply.Protocol(), b)
	if err != nil {
		return
	}
	echo, ok := msg.Body.(*icmp.Echo)
	if !ok {
		return
	}
	if msg.Type != ipv4.ICMPTypeEchoReply {
		return
	}
	// Not ours: another socket's reply, delivered here by the kernel.
	if echo.ID != p.id {
		return
	}
	if len(echo.Data) < payloadLen {
		return
	}
	if string(echo.Data[:8]) != string(p.magic[:]) {
		return
	}
	seq := binary.BigEndian.Uint16(echo.Data[8:])

	p.mu.Lock()
	f := p.inflight[seq]
	if f != nil {
		delete(p.inflight, seq)
	}
	p.mu.Unlock()

	if f == nil {
		// Already timed out, or a duplicate of one we have answered.
		return
	}

	rtt := time.Since(f.sentAt)
	outcome := metrics.OutcomeOK
	if rtt > SoftTimeout {
		outcome = metrics.OutcomeLate
	}
	select {
	case f.ch <- Result{RTT: rtt, Outcome: outcome}:
	default:
	}
}
