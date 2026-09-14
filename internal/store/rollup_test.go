package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/kokorhekkus/netwatch/internal/metrics"
)

func testDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func insertRaw(t *testing.T, db *DB, targetID int64, ts time.Time, rttUS int64, o metrics.Outcome) {
	t.Helper()
	var v sql.NullInt64
	if o == metrics.OutcomeOK || o == metrics.OutcomeLate {
		v = sql.NullInt64{Int64: rttUS, Valid: true}
	}
	_, err := db.w.Exec(`INSERT OR REPLACE INTO icmp_raw
		(target_id, ts_us, epoch_id, rtt_us, outcome, load) VALUES (?,?,?,?,?,0)`,
		targetID, ts.UnixMicro(), 1, v, int(o))
	if err != nil {
		t.Fatalf("insert raw: %v", err)
	}
}

func TestRollupProducesCorrectTally(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	base := time.Now().Add(-2 * time.Hour).Truncate(time.Minute)
	// One minute: 50 good, 5 lost, 3 local send errors.
	for i := 0; i < 50; i++ {
		insertRaw(t, db, 1, base.Add(time.Duration(i)*time.Second/60), 20_000, metrics.OutcomeOK)
	}
	for i := 50; i < 55; i++ {
		insertRaw(t, db, 1, base.Add(time.Duration(i)*time.Second/60), 0, metrics.OutcomeNoReply)
	}
	for i := 55; i < 58; i++ {
		insertRaw(t, db, 1, base.Add(time.Duration(i)*time.Second/60), 0, metrics.OutcomeSendErr)
	}

	n, err := db.RollupRaw(ctx, time.Now())
	if err != nil {
		t.Fatalf("RollupRaw: %v", err)
	}
	if n != 1 {
		t.Fatalf("built %d buckets, want 1", n)
	}

	var sent, recv, lost, sendErr int64
	err = db.r.QueryRow(`SELECT sent, recv, lost, send_err FROM icmp_agg
		WHERE grain = ? AND target_id = 1`, Grain1m).Scan(&sent, &recv, &lost, &sendErr)
	if err != nil {
		t.Fatalf("read agg: %v", err)
	}

	if sent != 55 {
		t.Errorf("sent = %d, want 55 (send errors must not count as sent)", sent)
	}
	if recv != 50 {
		t.Errorf("recv = %d, want 50", recv)
	}
	if lost != 5 {
		t.Errorf("lost = %d, want 5", lost)
	}
	if sendErr != 3 {
		t.Errorf("send_err = %d, want 3", sendErr)
	}
}

// The property the retention design depends on: an hourly bucket built by
// coarsening minute buckets must answer percentile queries identically to one
// built from the raw samples.
func TestCoarsenPreservesDistribution(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	base := time.Now().Add(-3 * time.Hour).Truncate(time.Hour)
	var direct metrics.Hist

	for minute := 0; minute < 60; minute++ {
		for i := 0; i < 20; i++ {
			rtt := int64(15_000 + minute*300 + i*137)
			ts := base.Add(time.Duration(minute)*time.Minute + time.Duration(i)*time.Second)
			insertRaw(t, db, 1, ts, rtt, metrics.OutcomeOK)
			direct.Observe(rtt)
		}
	}

	if _, err := db.RollupRaw(ctx, time.Now()); err != nil {
		t.Fatalf("RollupRaw: %v", err)
	}
	if _, err := db.Coarsen(ctx, Grain1m, Grain1h, time.Now()); err != nil {
		t.Fatalf("Coarsen: %v", err)
	}

	var blob []byte
	var sent int64
	err := db.r.QueryRow(`SELECT hist, sent FROM icmp_agg
		WHERE grain = ? AND target_id = 1`, Grain1h).Scan(&blob, &sent)
	if err != nil {
		t.Fatalf("read hourly agg: %v", err)
	}
	if sent != 1200 {
		t.Errorf("sent = %d, want 1200", sent)
	}

	got, err := metrics.Decode(blob)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Total() != direct.Total() {
		t.Fatalf("total: coarsened=%d direct=%d", got.Total(), direct.Total())
	}
	for _, q := range []float64{0.5, 0.9, 0.99} {
		if got.Quantile(q) != direct.Quantile(q) {
			t.Errorf("p%.0f: coarsened=%.0f direct=%.0f",
				q*100, got.Quantile(q), direct.Quantile(q))
		}
	}
}

func TestRollupExcludesOpenBucket(t *testing.T) {
	db := testDB(t)
	now := time.Now()

	// A sample in the current, still-filling minute must not be rolled up,
	// or the bucket would be written before it is complete.
	insertRaw(t, db, 1, now, 20_000, metrics.OutcomeOK)

	n, err := db.RollupRaw(context.Background(), now)
	if err != nil {
		t.Fatalf("RollupRaw: %v", err)
	}
	if n != 0 {
		t.Errorf("rolled up %d buckets, want 0: the current minute is still open", n)
	}
}

func TestPruneKeepsCoarseTiers(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Now()

	old := now.Add(-30 * 24 * time.Hour).Truncate(time.Minute)
	for i := 0; i < 30; i++ {
		insertRaw(t, db, 1, old.Add(time.Duration(i)*time.Second), 20_000, metrics.OutcomeOK)
	}
	if _, err := db.RollupRaw(ctx, now); err != nil {
		t.Fatalf("RollupRaw: %v", err)
	}
	if _, err := db.Coarsen(ctx, Grain1m, Grain1h, now); err != nil {
		t.Fatalf("Coarsen: %v", err)
	}
	if err := db.Prune(ctx, now); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	var rawCount int
	db.r.QueryRow(`SELECT COUNT(*) FROM icmp_raw`).Scan(&rawCount)
	if rawCount != 0 {
		t.Errorf("raw rows = %d, want 0 after 30 days", rawCount)
	}

	var hourCount int
	db.r.QueryRow(`SELECT COUNT(*) FROM icmp_agg WHERE grain = ?`, Grain1h).Scan(&hourCount)
	if hourCount == 0 {
		t.Error("hourly rollups were pruned; they must be kept indefinitely")
	}
}

func TestOrphanEpochClosed(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	nid, err := db.w.Exec(`INSERT INTO network
		(fingerprint, iface, kind, gw_ip, gw_mac, cidr, dhcp_domain, label,
		 first_seen_ms, last_seen_ms, allow_heavy)
		VALUES ('fp','en0','wifi','10.0.0.1','aa:bb','10.0.0.0/24','lan','l',0,0,0)`)
	if err != nil {
		t.Fatalf("insert network: %v", err)
	}
	id, _ := nid.LastInsertId()

	epochID, err := db.OpenEpoch(ctx, id, "test")
	if err != nil {
		t.Fatalf("OpenEpoch: %v", err)
	}
	insertRaw(t, db, 1, time.Now().Add(-time.Hour), 20_000, metrics.OutcomeOK)
	if _, err := db.w.Exec(`UPDATE icmp_raw SET epoch_id = ?`, epochID); err != nil {
		t.Fatalf("set epoch: %v", err)
	}

	n, err := db.CloseOrphanEpochs(ctx)
	if err != nil {
		t.Fatalf("CloseOrphanEpochs: %v", err)
	}
	if n != 1 {
		t.Errorf("closed %d epochs, want 1", n)
	}

	var reason string
	db.r.QueryRow(`SELECT end_reason FROM epoch WHERE id = ?`, epochID).Scan(&reason)
	if reason != "crash" {
		t.Errorf("end_reason = %q, want \"crash\"", reason)
	}
}
