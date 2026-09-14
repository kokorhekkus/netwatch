package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"sort"
	"time"

	"github.com/kokorhekkus/netwatch/internal/metrics"
	"github.com/kokorhekkus/netwatch/internal/netid"
	"github.com/kokorhekkus/netwatch/internal/probe"
)

// cmdProbe runs a one-shot measurement and prints it in a form directly
// comparable with ping(8), so the prober can be checked against a known-good
// implementation rather than trusted.
func cmdProbe(args []string) error {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	count := fs.Int("n", 20, "packets per target")
	interval := fs.Duration("i", 200*time.Millisecond, "interval between packets")
	host := fs.String("host", "", "probe only this address")
	fs.Parse(args)

	ctx := context.Background()

	var targets []struct{ label, addr string }
	if *host != "" {
		targets = append(targets, struct{ label, addr string }{*host, *host})
	} else {
		n := netid.Snapshot(ctx)
		if !n.Online() {
			return fmt.Errorf("no default route")
		}
		fmt.Printf("network: %s  fingerprint %s\n", n.Label(), n.Fingerprint())
		if n.GatewayMAC != "" {
			fmt.Printf("gateway: %s at %s (%s)\n\n", n.GatewayIP, n.GatewayMAC, n.Kind)
		}
		targets = append(targets,
			struct{ label, addr string }{"Router", n.GatewayIP},
			struct{ label, addr string }{"Cloudflare", "1.1.1.1"},
			struct{ label, addr string }{"Google", "8.8.8.8"},
		)
	}

	p, err := probe.NewPinger()
	if err != nil {
		return err
	}
	defer p.Close()

	fmt.Printf("%-14s %6s %8s %8s %8s %8s %8s\n",
		"TARGET", "LOSS", "min", "p50", "p95", "max", "jitter")

	for _, t := range targets {
		ip := net.ParseIP(t.addr)
		if ip == nil {
			fmt.Fprintf(os.Stderr, "skipping %q: not an IP\n", t.addr)
			continue
		}

		var rtts []float64
		var tally metrics.Tally
		var jitter metrics.IPDV

		for i := 0; i < *count; i++ {
			r := p.Ping(ctx, ip)
			tally.Add(r.Outcome)
			if r.Outcome == metrics.OutcomeOK || r.Outcome == metrics.OutcomeLate {
				us := float64(r.RTT.Microseconds())
				rtts = append(rtts, us)
				jitter.Observe(r.RTT.Microseconds())
			} else {
				jitter.Reset()
			}
			if i < *count-1 {
				time.Sleep(*interval)
			}
		}

		sort.Float64s(rtts)
		fmt.Printf("%-14s %5.1f%% %8s %8s %8s %8s %8s\n",
			t.label,
			tally.LossRatio()*100,
			fmtUS(pick(rtts, 0.0)),
			fmtUS(pick(rtts, 0.50)),
			fmtUS(pick(rtts, 0.95)),
			fmtUS(pick(rtts, 1.0)),
			fmtUS(jitter.Mean()),
		)
		if tally.SendErr > 0 {
			fmt.Printf("  (%d local send failures, excluded from loss)\n", tally.SendErr)
		}
	}
	return nil
}

func pick(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(q * float64(len(sorted)-1))
	return sorted[i]
}

func fmtUS(us float64) string {
	if us <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.2fms", us/1000)
}
