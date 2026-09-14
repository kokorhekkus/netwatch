package app

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/kokorhekkus/netwatch/internal/netid"
	"github.com/kokorhekkus/netwatch/internal/probe"
	"github.com/kokorhekkus/netwatch/internal/store"
)

const (
	// Twice a day. At ~315 MB a run, hourly testing would move roughly 15 GB
	// a day to measure a number that changes on the timescale of an ISP
	// engineer visit.
	heavyMinInterval = 12 * time.Hour

	// Monthly ceiling for capacity tests: 25 GB leaves ample room for two
	// runs a day plus manual ones, and stops a runaway loop well short of
	// anything that would show up on a bill.
	heavyMonthlyBudget = 25 << 30

	heavyCheckInterval = 5 * time.Minute
)

// Reasons a capacity test was declined, phrased for the dashboard.
type HeavyGate struct {
	Allowed bool
	Reason  string
}

// heavyLease ensures only one heavy probe runs at a time. Two capacity tests
// at once would each measure the other rather than the link.
type heavyLease struct {
	mu   sync.Mutex
	held bool
}

func (l *heavyLease) acquire() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held {
		return false
	}
	l.held = true
	return true
}

func (l *heavyLease) release() {
	l.mu.Lock()
	l.held = false
	l.mu.Unlock()
}

// canRunHeavy applies every gate in turn and explains the first refusal.
//
// The whole policy is default-deny: an unrecognised network never gets a
// capacity test, because the cost of being wrong is somebody's cellular bill.
func (a *App) canRunHeavy(ctx context.Context, manual bool) HeavyGate {
	a.mu.RLock()
	netID := a.netID
	idle := a.idle
	a.mu.RUnlock()

	if netID == 0 {
		return HeavyGate{Reason: "no network"}
	}

	allow, err := a.db.AllowHeavy(ctx, netID)
	if err != nil {
		return HeavyGate{Reason: "cannot read network settings"}
	}
	if !allow {
		return HeavyGate{Reason: "this network is not marked as trusted — run `netwatch trust` on it first"}
	}

	month, used, limit := a.budget.State()
	if !a.budget.Allows(probe.MeasuredCostBytes) {
		return HeavyGate{Reason: fmt.Sprintf(
			"monthly data budget spent (%s: %.1f of %.0f GB)",
			month, float64(used)/(1<<30), float64(limit)/(1<<30))}
	}

	// A manual request skips the scheduling gates but never the budget or
	// trust gates: the user asking for a test is not a reason to spend data
	// on a network they have not vouched for.
	if manual {
		return HeavyGate{Allowed: true}
	}

	last, err := a.db.LastThroughputAt(ctx)
	if err == nil && !last.IsZero() && time.Since(last) < heavyMinInterval {
		return HeavyGate{Reason: fmt.Sprintf("last test was %s ago",
			time.Since(last).Round(time.Minute))}
	}
	if idle == nil || !idle.Idle() {
		return HeavyGate{Reason: "link is in use — waiting for it to go quiet"}
	}
	return HeavyGate{Allowed: true}
}

// RunCapacity performs a capacity test if the gates allow it.
func (a *App) RunCapacity(ctx context.Context, manual bool) (probe.CapacityResult, error) {
	gate := a.canRunHeavy(ctx, manual)
	if !gate.Allowed {
		return probe.CapacityResult{}, fmt.Errorf("%s", gate.Reason)
	}
	if !a.lease.acquire() {
		return probe.CapacityResult{}, fmt.Errorf("a capacity test is already running")
	}
	defer a.lease.release()

	a.mu.RLock()
	iface := a.network.Iface
	epochID := a.epochID
	a.mu.RUnlock()

	// Latency probes keep running throughout and are tagged as loaded, so the
	// gap between loaded and unloaded percentiles becomes the bufferbloat
	// measurement. Suppressing them here would discard the most useful thing
	// a capacity test produces.
	a.setLoad(LoadSelfDown)
	defer a.setLoad(LoadIdle)

	a.log.Info("running capacity test", "engine", "networkQuality", "manual", manual)
	res, runErr := probe.RunNetworkQuality(ctx, iface)

	// Charge the budget even on failure: an aborted run still moved data.
	spent := res.BytesDown + res.BytesUp
	if spent == 0 && runErr != nil {
		spent = probe.MeasuredCostBytes / 4 // conservative guess for a partial run
	}
	a.budget.Charge(spent)

	if err := a.db.InsertThroughput(ctx, store.ThroughputSample{
		EpochID:     epochID,
		Engine:      res.Engine,
		DownMbps:    res.DownMbps,
		UpMbps:      res.UpMbps,
		DownRPM:     res.RPM,
		UpRPM:       res.UpRPM,
		BaseRTTUS:   res.BaseRTT.Microseconds(),
		LoadedP50US: res.LoadedRTTP50.Microseconds(),
		LoadedP95US: res.LoadedRTTP95.Microseconds(),
		BytesDown:   res.BytesDown,
		BytesUp:     res.BytesUp,
		Aborted:     res.Aborted,
		RawJSON:     res.RawJSON,
	}); err != nil {
		a.log.Warn("store throughput", "err", err)
	}

	if runErr != nil {
		return res, runErr
	}

	bloat := res.Bufferbloat()
	a.log.Info("capacity test complete",
		"down_mbps", fmt.Sprintf("%.1f", res.DownMbps),
		"up_mbps", fmt.Sprintf("%.1f", res.UpMbps),
		"base_rtt", res.BaseRTT.Round(time.Millisecond),
		"loaded_p95", res.LoadedRTTP95.Round(time.Millisecond),
		"rpm", res.RPM,
		"bufferbloat", bloat.Round(time.Millisecond),
		"cost_mb", spent>>20)

	if bloat > 200*time.Millisecond {
		_ = a.db.RecordEvent(ctx, "bufferbloat", fmt.Sprintf(
			"latency rose from %s to %s under load (+%s)",
			res.BaseRTT.Round(time.Millisecond),
			res.LoadedRTTP95.Round(time.Millisecond),
			bloat.Round(time.Millisecond)))
	}
	return res, nil
}

// Standalone prepares an App for a one-shot command run outside the daemon,
// so `netwatch speedtest` obeys exactly the same trust and budget gates the
// background agent does.
func Standalone(ctx context.Context, db *store.DB, log *slog.Logger) (*App, error) {
	a := New(db, log, "cli")

	n := netid.Snapshot(ctx)
	if !n.Online() {
		return nil, fmt.Errorf("no default route")
	}
	netID, err := db.UpsertNetwork(ctx, n)
	if err != nil {
		return nil, err
	}

	a.mu.Lock()
	a.network, a.netID = n, netID
	a.mu.Unlock()

	a.restoreBudget(ctx)
	return a, nil
}

// Network returns the current attachment.
func (a *App) Network() (netid.Network, int64) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.network, a.netID
}

// BudgetState exposes this month's heavy-probe spend.
func (a *App) BudgetState() (month string, used, limit int64) {
	return a.budget.State()
}

func (a *App) setLoad(state int) {
	a.mu.Lock()
	a.loadState = state
	a.mu.Unlock()
}

// runHeavyScheduler periodically checks whether a capacity test may run.
func (a *App) runHeavyScheduler(ctx context.Context) {
	ticker := time.NewTicker(heavyCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if gate := a.canRunHeavy(ctx, false); !gate.Allowed {
			continue
		}
		if _, err := a.RunCapacity(ctx, false); err != nil {
			a.log.Warn("scheduled capacity test failed", "err", err)
		}
	}
}

// restoreBudget seeds this month's spend from what is already on disk.
func (a *App) restoreBudget(ctx context.Context) {
	month, _, _ := a.budget.State()
	used, err := a.db.MonthlyHeavyBytes(ctx, month)
	if err != nil {
		a.log.Warn("restore budget", "err", err)
		return
	}
	a.budget.Restore(month, used)
	if used > 0 {
		a.log.Info("restored data budget", "month", month, "used_mb", used>>20)
	}
}

func nullIfZero(v int64) sql.NullInt64 {
	if v == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: v, Valid: true}
}
