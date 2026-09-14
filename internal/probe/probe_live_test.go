package probe

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"github.com/kokorhekkus/netwatch/internal/metrics"
)

// These touch the real network and the real machine. They are skipped in
// -short mode so an offline `go test ./...` stays green.

func TestInterfaceBytesReadsCounters(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the local interface")
	}
	a, err := interfaceBytes("en0")
	if err != nil {
		t.Skipf("en0 unavailable: %v", err)
	}
	if a.rx <= 0 || a.tx <= 0 {
		t.Fatalf("counters look wrong: rx=%d tx=%d", a.rx, a.tx)
	}

	// Counters are cumulative, so a later reading can never be smaller.
	// Getting the column indices wrong would show up here as nonsense.
	time.Sleep(1500 * time.Millisecond)
	b, err := interfaceBytes("en0")
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if b.rx < a.rx || b.tx < a.tx {
		t.Errorf("counters went backwards: %+v then %+v", a, b)
	}
	if b.rx-a.rx > 10<<30 {
		t.Errorf("implausible delta %d bytes in 1.5s — wrong column?", b.rx-a.rx)
	}
}

func TestDNSColdAndWarm(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the network")
	}
	ctx := context.Background()
	rng := rand.New(rand.NewSource(1))

	warm := QueryDNS(ctx, "1.1.1.1", false, rng)
	if warm.Outcome == metrics.OutcomeSendErr {
		t.Skip("no DNS reachability")
	}
	if warm.Latency <= 0 {
		t.Error("warm lookup reported no latency")
	}
	if warm.AnswerHash == "" {
		t.Error("warm lookup of a popular name returned no A records to hash")
	}

	cold := QueryDNS(ctx, "1.1.1.1", true, rng)
	if cold.Outcome == metrics.OutcomeMalformed {
		t.Errorf("cold lookup gave an unexpected rcode %d", cold.Rcode)
	}
	if cold.Latency <= 0 {
		t.Error("cold lookup reported no latency")
	}
	// A random label under a real domain should not resolve; if it does, the
	// domain has a wildcard and is a poor choice for cache-busting.
	if cold.Answers > 0 {
		t.Logf("note: cold lookup returned %d answers (wildcard domain?)", cold.Answers)
	}
}

func TestDNSDistinguishesCacheHit(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the network")
	}
	ctx := context.Background()
	rng := rand.New(rand.NewSource(2))

	// Prime, then time a warm lookup against a cold one. The cold path must
	// travel to an authoritative server, so it should not be faster.
	QueryDNS(ctx, "1.1.1.1", false, rng)
	warm := QueryDNS(ctx, "1.1.1.1", false, rng)
	cold := QueryDNS(ctx, "1.1.1.1", true, rng)

	if warm.Outcome == metrics.OutcomeSendErr || cold.Outcome == metrics.OutcomeSendErr {
		t.Skip("no DNS reachability")
	}
	t.Logf("warm=%v cold=%v", warm.Latency, cold.Latency)
	if cold.Latency < warm.Latency/4 {
		t.Errorf("cold lookup (%v) implausibly faster than warm (%v) — is the random label being cached?",
			cold.Latency, warm.Latency)
	}
}

func TestHTTPPhasesArePopulated(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the network")
	}
	res := FetchHTTP(context.Background(), "https://www.gstatic.com/generate_204")
	if res.Outcome == metrics.OutcomeSendErr {
		t.Skip("no HTTP reachability")
	}

	if res.Status != 204 {
		t.Errorf("status = %d, want 204", res.Status)
	}
	// The whole reason keep-alives are disabled: these must be non-zero.
	if res.Connect <= 0 {
		t.Error("connect phase is zero — is connection reuse enabled?")
	}
	if res.TLS <= 0 {
		t.Error("TLS phase is zero — is connection reuse enabled?")
	}
	if res.TTFB <= 0 {
		t.Error("TTFB is zero")
	}
	if res.ServerIP == "" {
		t.Error("no server IP captured")
	}
	if res.TTFB > res.Total {
		t.Errorf("TTFB %v exceeds total %v", res.TTFB, res.Total)
	}
}

func TestServerIDIdentifiesPoP(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the network")
	}
	id := ServerID(context.Background(), "1.1.1.1")
	if id == "" {
		t.Skip("resolver did not answer id.server")
	}
	t.Logf("1.1.1.1 answered from %q", id)
}
