package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/kokorhekkus/netwatch/internal/app"
	"github.com/kokorhekkus/netwatch/internal/probe"
	"github.com/kokorhekkus/netwatch/internal/store"
)

// cmdTrust marks the current network as one where expensive probes may run.
//
// This is opt-in rather than inferred. Guessing whether a network is a phone
// tether from its gateway address is guesswork, and being wrong means spending
// hundreds of megabytes of somebody's cellular allowance.
func cmdTrust(args []string) error {
	fs := flag.NewFlagSet("trust", flag.ExitOnError)
	dbPath := fs.String("db", store.DefaultPath(), "database path")
	off := fs.Bool("off", false, "revoke trust for this network")
	fs.Parse(args)

	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := context.Background()
	a, err := app.Standalone(ctx, db, quietLogger())
	if err != nil {
		return err
	}
	n, netID := a.Network()

	if err := db.SetAllowHeavy(ctx, netID, !*off); err != nil {
		return err
	}

	if *off {
		fmt.Printf("capacity tests disabled on %s (%s)\n", n.Label(), n.Fingerprint())
		return nil
	}
	fmt.Printf("capacity tests enabled on %s (%s)\n", n.Label(), n.Fingerprint())
	fmt.Printf("  gateway %s at %s\n", n.GatewayIP, n.GatewayMAC)
	fmt.Printf("\nEach run moves about %d MB and takes ~45s. The agent runs at most one\n",
		probe.MeasuredCostBytes>>20)
	fmt.Printf("every %s, only while the link is otherwise idle.\n", "12h")
	return nil
}

// cmdSpeedtest runs one capacity test now.
func cmdSpeedtest(args []string) error {
	fs := flag.NewFlagSet("speedtest", flag.ExitOnError)
	dbPath := fs.String("db", store.DefaultPath(), "database path")
	fs.Parse(args)

	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := context.Background()
	a, err := app.Standalone(ctx, db, quietLogger())
	if err != nil {
		return err
	}

	month, used, limit := a.BudgetState()
	fmt.Printf("Running a capacity test — about %d MB, ~45s.\n",
		probe.MeasuredCostBytes>>20)
	fmt.Printf("Budget for %s: %.1f of %.0f GB used.\n\n",
		month, float64(used)/(1<<30), float64(limit)/(1<<30))

	res, err := a.RunCapacity(ctx, true)
	if err != nil {
		return err
	}

	fmt.Printf("  download      %.1f Mbit/s\n", res.DownMbps)
	fmt.Printf("  upload        %.1f Mbit/s\n", res.UpMbps)
	fmt.Printf("  idle latency  %s\n", res.BaseRTT.Round(time.Millisecond))
	fmt.Printf("  under load    %s (p95)\n", res.LoadedRTTP95.Round(time.Millisecond))
	if res.RPM > 0 {
		fmt.Printf("  responsiveness %d RPM (%s)\n", res.RPM, rpmVerdict(res.RPM))
	}
	fmt.Printf("  data used     %d MB down, %d MB up\n",
		res.BytesDown>>20, res.BytesUp>>20)

	if bloat := res.Bufferbloat(); bloat > 0 {
		fmt.Printf("\n  bufferbloat   +%s under load", bloat.Round(time.Millisecond))
		if res.BaseRTT > 0 {
			fmt.Printf(" (%.0fx idle)", float64(res.LoadedRTTP95)/float64(res.BaseRTT))
		}
		fmt.Println()
		fmt.Println("\n  " + bufferbloatVerdict(bloat))
	}
	return nil
}

// bufferbloatVerdict translates added latency into what it means in practice.
//
// The number alone means nothing to most people; the experience it predicts
// is the useful part.
func bufferbloatVerdict(d time.Duration) string {
	ms := d.Milliseconds()
	switch {
	case ms < 30:
		return "Excellent — the link stays responsive while saturated."
	case ms < 100:
		return "Good. Calls should hold up during large transfers."
	case ms < 250:
		return "Noticeable. Video calls will stutter during big uploads or downloads."
	default:
		return "Severe. Any large transfer will make calls and browsing feel broken.\n" +
			"  This is a router queueing problem, not a line speed problem: SQM/fq_codel\n" +
			"  on the router is the fix, and it needs no change from your ISP."
	}
}

// rpmVerdict maps Apple's round-trips-per-minute score onto its published
// bands: below 300 is Low, 300-1000 Medium, above 1000 High.
func rpmVerdict(rpm int) string {
	switch {
	case rpm >= 1000:
		return "high"
	case rpm >= 300:
		return "medium"
	default:
		return "low"
	}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}
