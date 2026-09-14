package app

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/kokorhekkus/netwatch/internal/metrics"
	"github.com/kokorhekkus/netwatch/internal/store"
)

func insertSample(db *store.DB, targetID int64, ts time.Time, rttUS int64, o metrics.Outcome) error {
	var v sql.NullInt64
	if o == metrics.OutcomeOK || o == metrics.OutcomeLate {
		v = sql.NullInt64{Int64: rttUS, Valid: true}
	}
	return db.InsertICMPRaw(context.Background(), store.Sample{
		Kind:     store.SampleICMP,
		TargetID: targetID,
		EpochID:  1,
		TSMicro:  ts.UnixMicro(),
		ValueUS:  v,
		Outcome:  o,
	})
}

// Two minutes of perfectly identical samples produce two rollup rows holding
// byte-identical histogram blobs. Aggregating those in SQL would collapse them
// into one group and lose half the data - silently, and only for the steadiest
// connections, which is the worst possible failure mode.
func TestStatusMergesIdenticalHistograms(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if _, err := db.UpsertTarget(ctx, "gateway", 4, "10.0.0.1", "Router"); err != nil {
		t.Fatalf("UpsertTarget: %v", err)
	}

	base := time.Now().Add(-30 * time.Minute).Truncate(time.Minute)
	const perMinute = 20
	for minute := 0; minute < 2; minute++ {
		for i := 0; i < perMinute; i++ {
			ts := base.Add(time.Duration(minute)*time.Minute + time.Duration(i)*time.Second)
			// Identical RTT everywhere, so both minutes serialise to the
			// same histogram bytes.
			if err := insertSample(db, 1, ts, 20_000, metrics.OutcomeOK); err != nil {
				t.Fatalf("insert: %v", err)
			}
		}
	}

	if _, err := db.RollupRaw(ctx, time.Now()); err != nil {
		t.Fatalf("RollupRaw: %v", err)
	}

	sts, err := Status(ctx, db, time.Hour)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(sts) != 1 {
		t.Fatalf("got %d targets, want 1", len(sts))
	}
	if sts[0].Sent != 2*perMinute {
		t.Errorf("Sent = %d, want %d", sts[0].Sent, 2*perMinute)
	}
	// The counts come from SUM() and survive a collapsed GROUP BY; the
	// histogram does not, so this is the assertion that actually bites.
	if sts[0].Observations != 2*perMinute {
		t.Errorf("Observations = %d, want %d: a histogram row was dropped",
			sts[0].Observations, 2*perMinute)
	}
	if sts[0].P50 <= 0 {
		t.Errorf("p50 = %v, want a positive latency", sts[0].P50)
	}
}

func TestStatusReportsLossInterval(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	db.UpsertTarget(ctx, "gateway", 4, "10.0.0.1", "Router")

	base := time.Now().Add(-10 * time.Minute).Truncate(time.Minute)
	for i := 0; i < 95; i++ {
		insertSample(db, 1, base.Add(time.Duration(i)*time.Millisecond*600), 20_000, metrics.OutcomeOK)
	}
	for i := 95; i < 100; i++ {
		insertSample(db, 1, base.Add(time.Duration(i)*time.Millisecond*600), 0, metrics.OutcomeNoReply)
	}
	// Local failures must not widen or shift the loss figure.
	for i := 100; i < 150; i++ {
		insertSample(db, 1, base.Add(time.Duration(i)*time.Millisecond*600), 0, metrics.OutcomeSendErr)
	}

	if _, err := db.RollupRaw(ctx, time.Now()); err != nil {
		t.Fatalf("RollupRaw: %v", err)
	}
	sts, err := Status(ctx, db, time.Hour)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(sts) != 1 {
		t.Fatalf("got %d targets, want 1", len(sts))
	}
	s := sts[0]

	if s.Sent != 100 {
		t.Errorf("Sent = %d, want 100 (send errors excluded)", s.Sent)
	}
	if s.SendErr != 50 {
		t.Errorf("SendErr = %d, want 50", s.SendErr)
	}
	// True rate is 5%; the interval must contain it and be narrower than the
	// full range now that there are 100 samples.
	if s.LossLo > 0.05 || s.LossHi < 0.05 {
		t.Errorf("loss interval [%.3f,%.3f] excludes the true 0.05", s.LossLo, s.LossHi)
	}
	if s.LossHi-s.LossLo > 0.15 {
		t.Errorf("loss interval [%.3f,%.3f] implausibly wide at n=100", s.LossLo, s.LossHi)
	}
}
