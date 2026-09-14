package app

import (
	"context"
	"database/sql"
	"math/rand"
	"time"

	"github.com/kokorhekkus/netwatch/internal/metrics"
	"github.com/kokorhekkus/netwatch/internal/probe"
	"github.com/kokorhekkus/netwatch/internal/store"
	"github.com/kokorhekkus/netwatch/internal/wifi"
)

const (
	dnsInterval  = 30 * time.Second
	httpInterval = 60 * time.Second
)

// httpTargets are chosen to be tiny and purpose-built for probing.
//
// generate_204 returns an empty 204, so the measurement is setup cost rather
// than transfer; the Cloudflare trace endpoint is a few hundred bytes and
// also reports which PoP served it.
var httpTargets = []string{
	"https://www.gstatic.com/generate_204",
	"https://1.1.1.1/cdn-cgi/trace",
}

// runDNSProbe times resolution against the router's resolver and two public
// ones.
//
// Comparing them is the point. A slow router resolver next to fast public
// ones says the problem is the router, not the line - a distinction that
// latency graphs alone cannot make, and one that has a five-minute fix.
func (a *App) runDNSProbe(ctx context.Context) {
	a.mu.RLock()
	gw := a.network.GatewayIP
	a.mu.RUnlock()

	resolvers := []string{"1.1.1.1", "8.8.8.8"}
	if gw != "" {
		resolvers = append([]string{gw}, resolvers...)
	}

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	ticker := time.NewTicker(dnsInterval)
	defer ticker.Stop()

	warm := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// Alternate cold and warm so both are sampled evenly without
		// doubling the query rate.
		warm = !warm
		cold := !warm

		for _, r := range resolvers {
			res := probe.QueryDNS(ctx, r, cold, rng)

			a.mu.RLock()
			epochID := a.epochID
			a.mu.RUnlock()

			if err := a.db.InsertDNS(ctx, store.DNSSample{
				Resolver:   r,
				EpochID:    epochID,
				Cold:       cold,
				LatencyUS:  nullIfZero(res.Latency.Microseconds()),
				Rcode:      sql.NullInt64{Int64: int64(res.Rcode), Valid: res.Outcome != metrics.OutcomeSendErr},
				Answers:    sql.NullInt64{Int64: int64(res.Answers), Valid: true},
				AnswerHash: res.AnswerHash,
				Outcome:    res.Outcome,
			}); err != nil {
				a.log.Warn("store dns sample", "err", err)
			}
		}
	}
}

// runHTTPProbe measures a full request broken into phases.
func (a *App) runHTTPProbe(ctx context.Context) {
	ticker := time.NewTicker(httpInterval)
	defer ticker.Stop()

	i := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		url := httpTargets[i%len(httpTargets)]
		i++

		res := probe.FetchHTTP(ctx, url)

		a.mu.RLock()
		epochID := a.epochID
		a.mu.RUnlock()

		if err := a.db.InsertHTTP(ctx, store.HTTPSample{
			URL:       url,
			EpochID:   epochID,
			DNSUS:     nullIfZero(res.DNS.Microseconds()),
			ConnectUS: nullIfZero(res.Connect.Microseconds()),
			TLSUS:     nullIfZero(res.TLS.Microseconds()),
			TTFBUS:    nullIfZero(res.TTFB.Microseconds()),
			TotalUS:   nullIfZero(res.Total.Microseconds()),
			Status:    nullIfZero(int64(res.Status)),
			ServerIP:  res.ServerIP,
			Outcome:   res.Outcome,
		}); err != nil {
			a.log.Warn("store http sample", "err", err)
		}
	}
}

const wifiInterval = 30 * time.Second

// runWiFiProbe records the radio's state alongside the latency samples.
//
// Nothing else in the tool can distinguish "my Wi-Fi is bad" from "my line is
// bad", and on this machine the link is Wi-Fi, so most degradation is at least
// a candidate for being the radio's fault. Ethernet and a powered-off radio
// simply record nothing rather than zeroes.
func (a *App) runWiFiProbe(ctx context.Context) {
	ticker := time.NewTicker(wifiInterval)
	defer ticker.Stop()

	logged := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		s := wifi.Read()
		if !s.Supported {
			if !logged {
				a.log.Info("no Wi-Fi radio telemetry; skipping (wired, radio off, or built without cgo)")
				logged = true
			}
			continue
		}

		a.mu.RLock()
		epochID := a.epochID
		a.mu.RUnlock()

		if err := a.db.InsertWiFi(ctx, store.WiFiSample{
			EpochID:  epochID,
			RSSI:     s.RSSI,
			Noise:    s.Noise,
			TxRate:   s.TxRate,
			Channel:  s.Channel,
			WidthMHz: s.WidthMHz,
			Band:     s.Band,
			PHYMode:  s.PHYMode,
		}); err != nil {
			a.log.Warn("store wifi sample", "err", err)
		}
	}
}

// runPoPWatch notices when an anycast resolver starts answering from a
// different site.
//
// An RTT step to 1.1.1.1 usually means routing changed, not that the
// connection degraded. Recording which instance answered turns a mysterious
// jump in the latency chart into a labelled event.
func (a *App) runPoPWatch(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	last := map[string]string{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for _, r := range []string{"1.1.1.1", "8.8.8.8"} {
			id := probe.ServerID(ctx, r)
			if id == "" {
				continue
			}
			if prev, ok := last[r]; ok && prev != id {
				a.log.Info("anycast site changed", "resolver", r, "from", prev, "to", id)
				_ = a.db.RecordEvent(ctx, "pop_change", r+": "+prev+" → "+id)
			}
			last[r] = id
		}
	}
}
