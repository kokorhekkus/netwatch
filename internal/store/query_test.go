package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/kokorhekkus/netwatch/internal/metrics"
)

// Coarse tiers are built only from closed buckets, so they always lag. Picking
// one for a short range leaves the most recent part of the chart blank - which
// is exactly the part someone opening the dashboard cares about.
func TestPickGrainKeepsRecentDataVisible(t *testing.T) {
	hour := int64(3600)
	for _, tc := range []struct {
		name     string
		rangeSec int64
		want     int
	}{
		{"1h", hour, Grain1m},
		{"6h", 6 * hour, Grain1m},
		{"24h", 24 * hour, Grain1m},
		{"7d", 7 * 24 * hour, Grain1m},
		{"30d", 30 * 24 * hour, Grain1h},
		{"1y", 365 * 24 * hour, Grain1h},
		{"5y", 5 * 365 * 24 * hour, Grain1d},
	} {
		if got := PickGrain(tc.rangeSec, 1000); got != tc.want {
			t.Errorf("PickGrain(%s) = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// End-to-end: minutes of data written now must appear in a six-hour query.
// The original implementation returned an empty series here.
func TestQuerySeriesReturnsFreshData(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if _, err := db.UpsertTarget(ctx, "gateway", 4, "10.0.0.1", "Router"); err != nil {
		t.Fatalf("UpsertTarget: %v", err)
	}

	now := time.Now()
	base := now.Add(-5 * time.Minute).Truncate(time.Minute)
	for minute := 0; minute < 4; minute++ {
		for i := 0; i < 20; i++ {
			ts := base.Add(time.Duration(minute)*time.Minute + time.Duration(i)*time.Second)
			var v = int64(20_000 + i*100)
			if err := db.InsertICMPRaw(ctx, Sample{
				TargetID: 1, EpochID: 1, TSMicro: ts.UnixMicro(),
				ValueUS: nullInt(v), Outcome: metrics.OutcomeOK,
			}); err != nil {
				t.Fatalf("insert: %v", err)
			}
		}
	}
	if _, err := db.RollupRaw(ctx, now); err != nil {
		t.Fatalf("RollupRaw: %v", err)
	}

	series, err := db.QuerySeries(ctx, now.Add(-6*time.Hour), now, 1000, 0)
	if err != nil {
		t.Fatalf("QuerySeries: %v", err)
	}
	if len(series) != 1 {
		t.Fatalf("got %d series, want 1 — recent data was invisible", len(series))
	}
	if len(series[0].Buckets) == 0 {
		t.Fatal("series has no buckets")
	}

	var sent int64
	for _, b := range series[0].Buckets {
		sent += b.Sent
		if b.P50 <= 0 {
			t.Errorf("bucket at %d has no p50", b.TS)
		}
	}
	if sent != 80 {
		t.Errorf("total sent = %d, want 80", sent)
	}
}

// A long range must be downsampled to roughly the requested point budget
// rather than returning every stored bucket.
func TestQuerySeriesRespectsPointBudget(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	db.UpsertTarget(ctx, "gateway", 4, "10.0.0.1", "Router")

	now := time.Now()
	base := now.Add(-24 * time.Hour).Truncate(time.Minute)
	for minute := 0; minute < 600; minute++ {
		ts := base.Add(time.Duration(minute) * time.Minute)
		db.InsertICMPRaw(ctx, Sample{
			TargetID: 1, EpochID: 1, TSMicro: ts.UnixMicro(),
			ValueUS: nullInt(20_000), Outcome: metrics.OutcomeOK,
		})
	}
	if _, err := db.RollupRaw(ctx, now); err != nil {
		t.Fatalf("RollupRaw: %v", err)
	}

	const points = 50
	// Query from the truncated base, not now-24h: Truncate rounds down, so
	// the first bucket starts just before the window and a half-open range
	// would legitimately exclude it.
	series, err := db.QuerySeries(ctx, base, now, points, 0)
	if err != nil {
		t.Fatalf("QuerySeries: %v", err)
	}
	if len(series) != 1 {
		t.Fatalf("got %d series, want 1", len(series))
	}
	if n := len(series[0].Buckets); n > points+2 {
		t.Errorf("returned %d buckets for a budget of %d", n, points)
	}

	// Downsampling must not lose samples.
	var sent int64
	for _, b := range series[0].Buckets {
		sent += b.Sent
	}
	if sent != 600 {
		t.Errorf("sent = %d after downsampling, want 600", sent)
	}
}

func TestZeroLossReportsCleanZero(t *testing.T) {
	lo, _ := metrics.WilsonInterval(0, 500, 1.96)
	if lo != 0 {
		t.Errorf("lower bound = %v, want exactly 0 for zero observed losses", lo)
	}
}

func nullInt(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }
