package probe

import (
	"context"
	"net"
	"time"

	"github.com/kokorhekkus/netwatch/internal/metrics"
)

// TCPConnectTimeout is deliberately as generous as the ICMP hard deadline, for
// the same reason: a tight deadline turns slowness into fabricated failure.
const TCPConnectTimeout = 3 * time.Second

// TCPConnect measures the time to complete a TCP handshake.
//
// This exists to inoculate the whole dataset against the first objection
// anyone raises about ICMP graphs, including an ISP's support desk: routers
// deprioritise and rate-limit ICMP to their control plane, so ICMP RTT is not
// necessarily what real traffic experiences. A TCP handshake is forwarded in
// the data plane exactly like a user's packets, so when the two series agree
// the ICMP data is corroborated, and when they diverge that divergence is
// itself the finding.
func TCPConnect(ctx context.Context, addr string) Result {
	ctx, cancel := context.WithTimeout(ctx, TCPConnectTimeout)
	defer cancel()

	var d net.Dialer
	start := time.Now()
	conn, err := d.DialContext(ctx, "tcp", addr)
	elapsed := time.Since(start)

	if err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			return Result{Outcome: metrics.OutcomeNoReply}
		}
		// A refused connection still proves the path works end to end; only
		// the port is shut. Treat it as a successful round trip.
		if isRefused(err) {
			return Result{RTT: elapsed, Outcome: outcomeFor(elapsed)}
		}
		return Result{Outcome: metrics.OutcomeSendErr}
	}
	conn.Close()

	return Result{RTT: elapsed, Outcome: outcomeFor(elapsed)}
}

func outcomeFor(d time.Duration) metrics.Outcome {
	if d > SoftTimeout {
		return metrics.OutcomeLate
	}
	return metrics.OutcomeOK
}

func isRefused(err error) bool {
	var oe *net.OpError
	if !asOpError(err, &oe) {
		return false
	}
	return oe.Err != nil && oe.Err.Error() == "connect: connection refused"
}

func asOpError(err error, target **net.OpError) bool {
	for err != nil {
		if oe, ok := err.(*net.OpError); ok {
			*target = oe
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
