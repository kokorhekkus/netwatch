package probe

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"time"

	"github.com/kokorhekkus/netwatch/internal/metrics"
)

const HTTPTimeout = 10 * time.Second

// HTTPResult breaks one request into the phases that fail independently.
//
// "The internet is slow" almost always means one specific phase is slow, and
// they have completely different causes: DNS points at the resolver, connect
// at the path, TLS at the server or a middlebox, TTFB at the far end.
type HTTPResult struct {
	DNS      time.Duration
	Connect  time.Duration
	TLS      time.Duration
	TTFB     time.Duration
	Total    time.Duration
	Status   int
	ServerIP string
	Outcome  metrics.Outcome
}

// FetchHTTP performs one traced HTTPS request.
//
// Connection reuse is disabled deliberately. With keep-alives on, every
// request after the first reports zero for DNS, connect and TLS - the phases
// this probe exists to measure - and the numbers look wonderful precisely
// because nothing was measured.
//
// Note this measures a CDN edge, not the ISP: it is a whole-stack check, and
// blaming the ISP for a slow TTFB here would be unfounded.
func FetchHTTP(ctx context.Context, url string) HTTPResult {
	ctx, cancel := context.WithTimeout(ctx, HTTPTimeout)
	defer cancel()

	var (
		res                                   HTTPResult
		dnsStart, connStart, tlsStart, reqEnd time.Time
	)

	trace := &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone: func(httptrace.DNSDoneInfo) {
			if !dnsStart.IsZero() {
				res.DNS = time.Since(dnsStart)
			}
		},
		ConnectStart: func(string, string) { connStart = time.Now() },
		ConnectDone: func(_, addr string, err error) {
			if err == nil && !connStart.IsZero() {
				res.Connect = time.Since(connStart)
				res.ServerIP = addr
			}
		},
		TLSHandshakeStart: func() { tlsStart = time.Now() },
		TLSHandshakeDone: func(tls.ConnectionState, error) {
			if !tlsStart.IsZero() {
				res.TLS = time.Since(tlsStart)
			}
		},
		GotFirstResponseByte: func() { reqEnd = time.Now() },
	}

	req, err := http.NewRequestWithContext(
		httptrace.WithClientTrace(ctx, trace), http.MethodGet, url, nil)
	if err != nil {
		res.Outcome = metrics.OutcomeMalformed
		return res
	}
	req.Header.Set("User-Agent", UserAgent)

	transport := &http.Transport{
		DisableKeepAlives: true,
		ForceAttemptHTTP2: false,
		DialContext:       (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
	}
	defer transport.CloseIdleConnections()

	start := time.Now()
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		res.Total = time.Since(start)
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			res.Outcome = metrics.OutcomeNoReply
		} else {
			res.Outcome = metrics.OutcomeSendErr
		}
		return res
	}
	defer resp.Body.Close()

	// The body must be drained for Total to mean anything.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))

	res.Total = time.Since(start)
	if !reqEnd.IsZero() {
		res.TTFB = reqEnd.Sub(start)
	}
	res.Status = resp.StatusCode
	res.Outcome = outcomeFor(res.Total)
	if resp.StatusCode >= 500 {
		res.Outcome = metrics.OutcomeMalformed
	}
	return res
}
