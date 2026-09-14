package probe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/kokorhekkus/netwatch/internal/metrics"
)

const DNSTimeout = 3 * time.Second

// Domains used for cold lookups. A random label is prefixed to each, so the
// query must travel all the way to the authoritative server and back; the
// answer is almost always NXDOMAIN, which is fine, because the thing being
// measured is the round trip, not the record.
//
// These are large, well-provisioned zones and the probe runs twice a minute,
// which is nothing to them. Rotating between several avoids leaning on any
// single operator.
var coldDomains = []string{
	"cloudflare.com",
	"wikipedia.org",
	"github.com",
	"bbc.co.uk",
}

// warmName is looked up repeatedly on purpose: after the first query every
// resolver should answer it from cache, so its timing measures the resolver's
// own responsiveness rather than the wider internet's.
const warmName = "www.google.com."

type DNSResult struct {
	Latency    time.Duration
	Outcome    metrics.Outcome
	Rcode      int
	Answers    int
	AnswerHash string
}

// QueryDNS times a single lookup against one specific resolver.
//
// The system resolver is deliberately bypassed. macOS routes DNS through
// mDNSResponder with per-domain resolvers and possibly a DoH profile, so
// net.Resolver would measure whatever the system decided to do rather than a
// named server, and could not attribute a slow lookup to anything.
func QueryDNS(ctx context.Context, server string, cold bool, rng *rand.Rand) DNSResult {
	name := warmName
	if cold {
		domain := coldDomains[rng.Intn(len(coldDomains))]
		name = fmt.Sprintf("nw-%08x.%s.", rng.Uint32(), domain)
	}

	m := new(dns.Msg)
	m.SetQuestion(name, dns.TypeA)
	m.RecursionDesired = true

	c := &dns.Client{Timeout: DNSTimeout}
	addr := net.JoinHostPort(server, "53")

	start := time.Now()
	resp, _, err := c.ExchangeContext(ctx, m, addr)
	elapsed := time.Since(start)

	if err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			return DNSResult{Outcome: metrics.OutcomeNoReply}
		}
		return DNSResult{Outcome: metrics.OutcomeSendErr}
	}

	res := DNSResult{
		Latency: elapsed,
		Outcome: outcomeFor(elapsed),
		Rcode:   resp.Rcode,
		Answers: len(resp.Answer),
	}
	// NXDOMAIN is the expected answer for a random label and is not a fault.
	// Anything other than NXDOMAIN or NOERROR is.
	if resp.Rcode != dns.RcodeSuccess && resp.Rcode != dns.RcodeNameError {
		res.Outcome = metrics.OutcomeMalformed
	}
	res.AnswerHash = hashAnswers(resp.Answer)
	return res
}

// hashAnswers fingerprints the A records returned.
//
// Comparing this across resolvers is how DNS hijacking or a captive portal
// shows up: the same name resolving to different addresses depending on who
// you ask.
func hashAnswers(rrs []dns.RR) string {
	var addrs []string
	for _, rr := range rrs {
		if a, ok := rr.(*dns.A); ok {
			addrs = append(addrs, a.A.String())
		}
	}
	if len(addrs) == 0 {
		return ""
	}
	sort.Strings(addrs)
	sum := sha256.Sum256([]byte(strings.Join(addrs, ",")))
	return hex.EncodeToString(sum[:])[:12]
}

// ServerID asks a resolver which of its instances answered, via the
// conventional `id.server` CHAOS TXT query.
//
// This is the anycast PoP detector. Reply TTL would have been the cheaper
// signal but Darwin does not expose it on a datagram ICMP socket, and this
// returns something far more specific anyway - "lhr21" rather than a region.
// A sudden change here explains an RTT step that is routing, not degradation.
func ServerID(ctx context.Context, server string) string {
	m := new(dns.Msg)
	m.Question = []dns.Question{{
		Name:   "id.server.",
		Qtype:  dns.TypeTXT,
		Qclass: dns.ClassCHAOS,
	}}

	c := &dns.Client{Timeout: DNSTimeout}
	resp, _, err := c.ExchangeContext(ctx, m, net.JoinHostPort(server, "53"))
	if err != nil || resp == nil {
		return ""
	}
	for _, rr := range resp.Answer {
		if txt, ok := rr.(*dns.TXT); ok && len(txt.Txt) > 0 {
			return txt.Txt[0]
		}
	}
	return ""
}
