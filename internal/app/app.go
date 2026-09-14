package app

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/kokorhekkus/netwatch/internal/clock"
	"github.com/kokorhekkus/netwatch/internal/metrics"
	"github.com/kokorhekkus/netwatch/internal/netid"
	"github.com/kokorhekkus/netwatch/internal/probe"
	"github.com/kokorhekkus/netwatch/internal/store"
)

const (
	// How long to wait after waking before trusting the network again.
	// Reassociation, DHCP and DNS all take seconds; probing through that
	// window records failures that are ours, not the network's.
	wakeSettle = 8 * time.Second

	// Anything beyond this between two clock readings is a discontinuity
	// rather than scheduling jitter.
	clockTolerance = 5 * time.Second

	sleepCheckInterval = 2 * time.Second
	maintainInterval   = 60 * time.Second
	tcpProbeInterval   = 30 * time.Second
)

// Load states stamped onto each sample. Comparing unloaded against loaded
// latency is the bufferbloat measurement, so they must never be averaged
// together.
const (
	LoadIdle = 0
	LoadSelfDown
	LoadSelfUp
	LoadExternal
)

type App struct {
	db    *store.DB
	w     *store.Writer
	log   *slog.Logger
	build string

	mu        sync.RWMutex
	network   netid.Network
	netID     int64
	epochID   int64
	targets   []Target
	tcpAddr   string
	loadState int

	idle  *probe.IdleMonitor
	lease heavyLease

	budget *probe.Budget

	// Fingerprint of the previous attachment, to tell a real network change
	// from a restart. Only touched from the single Run loop.
	lastFingerprint string
}

func New(db *store.DB, log *slog.Logger, build string) *App {
	return &App{
		db:      db,
		w:       store.NewWriter(db, log),
		log:     log,
		build:   build,
		tcpAddr: "1.1.1.1:443",
		budget:  probe.NewBudget(heavyMonthlyBudget),
	}
}

func (a *App) Run(ctx context.Context) error {
	if n, err := a.db.CloseOrphanEpochs(ctx); err != nil {
		a.log.Warn("could not close orphan epochs", "err", err)
	} else if n > 0 {
		a.log.Info("closed epochs left open by an unclean shutdown", "count", n)
	}

	a.restoreBudget(ctx)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); a.w.Run(ctx) }()

	changes, err := netid.Watch(ctx, 2*time.Second)
	if err != nil {
		return fmt.Errorf("watch network: %w", err)
	}

	// Each network attachment gets its own probe goroutines, cancelled and
	// rebuilt when the network changes.
	var probeCancel context.CancelFunc
	var probeWG sync.WaitGroup

	stopProbes := func(reason string) {
		if probeCancel == nil {
			return
		}
		probeCancel()
		probeWG.Wait()
		probeCancel = nil

		a.mu.RLock()
		id := a.epochID
		a.mu.RUnlock()
		if id != 0 {
			if err := a.db.CloseEpoch(context.WithoutCancel(ctx), id, reason); err != nil {
				a.log.Warn("close epoch", "err", err)
			}
		}
	}

	wg.Add(1)
	go func() { defer wg.Done(); a.watchClock(ctx) }()

	wg.Add(1)
	go func() { defer wg.Done(); a.maintain(ctx) }()

	for {
		select {
		case <-ctx.Done():
			stopProbes("shutdown")
			a.w.Wait()
			wg.Wait()
			return nil

		case n, ok := <-changes:
			if !ok {
				stopProbes("shutdown")
				a.w.Wait()
				wg.Wait()
				return nil
			}

			stopProbes("net_change")

			if !n.Online() {
				a.log.Warn("no default route; probes paused")
				a.mu.Lock()
				a.network, a.targets, a.epochID = n, nil, 0
				a.mu.Unlock()
				_ = a.db.RecordEvent(ctx, "net_offline", "no default route")
				continue
			}

			pctx, cancel := context.WithCancel(ctx)
			probeCancel = cancel
			if err := a.startProbes(pctx, &probeWG, n); err != nil {
				a.log.Error("start probes", "err", err)
				cancel()
				probeCancel = nil
			}
		}
	}
}

func (a *App) startProbes(ctx context.Context, wg *sync.WaitGroup, n netid.Network) error {
	netID, err := a.db.UpsertNetwork(ctx, n)
	if err != nil {
		return fmt.Errorf("upsert network: %w", err)
	}
	epochID, err := a.db.OpenEpoch(ctx, netID, a.build)
	if err != nil {
		return fmt.Errorf("open epoch: %w", err)
	}

	// Reuse the hop already pinned for this network. Rediscovering would pick
	// a different address on an ECMP path and start a fresh, unrelated series.
	pinnedHop, err := a.db.ISPHop(ctx, netID)
	if err != nil {
		a.log.Warn("read pinned ISP hop", "err", err)
	}

	targets := buildTargets(ctx, n, pinnedHop)

	if pinnedHop == "" {
		for _, t := range targets {
			if t.Kind == "isp_hop" {
				if err := a.db.SetISPHop(ctx, netID, t.Addr); err != nil {
					a.log.Warn("pin ISP hop", "err", err)
				} else {
					a.log.Info("pinned ISP hop for this network", "addr", t.Addr)
				}
			}
		}
	}

	for i := range targets {
		id, err := a.db.UpsertTarget(ctx, targets[i].Kind, 4, targets[i].Addr, targets[i].Label)
		if err != nil {
			return fmt.Errorf("upsert target %s: %w", targets[i].Addr, err)
		}
		targets[i].ID = id
	}

	a.mu.Lock()
	a.network, a.netID, a.epochID, a.targets = n, netID, epochID, targets
	a.mu.Unlock()

	a.log.Info("probing",
		"network", n.Label(),
		"fingerprint", n.Fingerprint(),
		"targets", len(targets))

	// Only a genuine change is a change. Logging one on every daemon start
	// would fill the event list with claims that the network moved when all
	// that happened was a restart.
	kind := "start"
	if a.lastFingerprint != "" && a.lastFingerprint != n.Fingerprint() {
		kind = "net_change"
	}
	a.lastFingerprint = n.Fingerprint()
	_ = a.db.RecordEvent(ctx, kind, n.Label())

	pinger, err := probe.NewPinger()
	if err != nil {
		return fmt.Errorf("open pinger: %w", err)
	}
	go func() { <-ctx.Done(); pinger.Close() }()

	seed := time.Now().UnixNano()
	for i, t := range targets {
		if t.IP == nil {
			continue
		}
		t := t
		// Stagger targets so they never burst together.
		offset := time.Duration(i) * (probe.DefaultMeanInterval / time.Duration(len(targets)+1))
		rng := rand.New(rand.NewSource(seed + int64(i)))

		wg.Add(1)
		go func() {
			defer wg.Done()
			probe.Schedule(ctx, probe.DefaultMeanInterval, offset, rng, func(ctx context.Context) {
				res := pinger.Ping(ctx, t.IP)
				a.record(store.SampleICMP, t.ID, res)
			})
		}()
	}

	idle := probe.NewIdleMonitor(n.Iface)
	a.mu.Lock()
	a.idle = idle
	a.mu.Unlock()

	for _, fn := range []func(context.Context){
		a.runTCPProbe,
		a.runDNSProbe,
		a.runHTTPProbe,
		a.runWiFiProbe,
		a.runPoPWatch,
		idle.Run,
		a.runHeavyScheduler,
	} {
		fn := fn
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn(ctx)
		}()
	}

	return nil
}

func (a *App) runTCPProbe(ctx context.Context) {
	id, err := a.db.UpsertTarget(ctx, "control", 4, a.tcpAddr, "TCP handshake")
	if err != nil {
		a.log.Warn("tcp target", "err", err)
		return
	}
	ticker := time.NewTicker(tcpProbeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.record(store.SampleTCP, id, probe.TCPConnect(ctx, a.tcpAddr))
		}
	}
}

func (a *App) record(kind store.SampleKind, targetID int64, r probe.Result) {
	a.mu.RLock()
	epochID := a.epochID
	load := a.loadState
	a.mu.RUnlock()

	var v sql.NullInt64
	if r.Outcome == metrics.OutcomeOK || r.Outcome == metrics.OutcomeLate {
		v = sql.NullInt64{Int64: r.RTT.Microseconds(), Valid: true}
	}
	a.w.Submit(store.Sample{
		Kind:     kind,
		TargetID: targetID,
		EpochID:  epochID,
		TSMicro:  time.Now().UnixMicro(),
		ValueUS:  v,
		Outcome:  r.Outcome,
		Load:     load,
	})
}

// watchClock turns machine sleep and NTP steps into explicit records.
//
// Without this an overnight suspend appears as a single enormous gap in the
// data that is indistinguishable from an eight-hour outage.
func (a *App) watchClock(ctx context.Context) {
	prev := clock.Now()
	ticker := time.NewTicker(sleepCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		now := clock.Now()
		d := clock.Compare(prev, now, clockTolerance)
		prev = now

		switch {
		case d.IsSleep:
			from := now.Wall.Add(-d.Slept).UnixMilli()
			to := now.Wall.UnixMilli()
			if err := a.db.RecordGap(ctx, from, to, "sleep"); err != nil {
				a.log.Warn("record gap", "err", err)
			}
			a.log.Info("woke from sleep", "slept", d.Slept.Round(time.Second))
			_ = a.db.RecordEvent(ctx, "wake", d.Slept.Round(time.Second).String())

			// Let the network settle before believing anything it tells us.
			select {
			case <-time.After(wakeSettle):
			case <-ctx.Done():
				return
			}
			prev = clock.Now()

		case d.IsStep:
			a.log.Warn("clock stepped", "by", d.Stepped.Round(time.Millisecond))
			_ = a.db.RecordEvent(ctx, "clock_step", d.Stepped.String())
		}
	}
}

func (a *App) maintain(ctx context.Context) {
	ticker := time.NewTicker(maintainInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.db.Maintain(ctx, time.Now()); err != nil {
				a.log.Warn("maintenance", "err", err)
			}
			if err := a.db.PruneProbes(ctx, time.Now()); err != nil {
				a.log.Warn("prune probe tables", "err", err)
			}
			if d := a.w.Dropped(); d > 0 {
				a.log.Warn("samples dropped because the write queue was full", "count", d)
			}
		}
	}
}
