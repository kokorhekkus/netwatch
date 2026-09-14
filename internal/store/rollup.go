package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/kokorhekkus/netwatch/internal/metrics"
)

// Grains are the bucket widths, in seconds.
const (
	Grain1m = 60
	Grain1h = 3600
	Grain1d = 86400
)

// Retention per tier. The hour and day tiers are never deleted: at roughly a
// dozen megabytes a year they are cheap enough to keep for the life of the
// machine, which is what makes a multi-year "is it getting worse?" question
// answerable at all.
const (
	RetainRaw = 14 * 24 * time.Hour
	Retain1m  = 180 * 24 * time.Hour
)

// RollupRaw builds 1-minute buckets from raw samples.
//
// Only buckets that have completely closed are built, so a bucket is never
// written twice with different contents.
func (d *DB) RollupRaw(ctx context.Context, now time.Time) (int, error) {
	cutoff := now.Truncate(time.Minute).Unix()

	rows, err := d.r.QueryContext(ctx, `
		SELECT target_id, load, ts_us/1000000/60*60 AS bucket_s, rtt_us, outcome
		FROM icmp_raw
		WHERE ts_us < ?*1000000
		ORDER BY target_id, load, bucket_s, ts_us`, cutoff)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type key struct {
		target int64
		load   int
		bucket int64
	}
	type acc struct {
		tally metrics.Tally
		hist  metrics.Hist
		ipdv  metrics.IPDV
		min   sql.NullInt64
		max   sql.NullInt64
		sum   int64
		sumsq int64
	}

	buckets := map[key]*acc{}
	var last key
	var haveLast bool

	for rows.Next() {
		var targetID int64
		var load int
		var bucket int64
		var rtt sql.NullInt64
		var outcome int
		if err := rows.Scan(&targetID, &load, &bucket, &rtt, &outcome); err != nil {
			return 0, err
		}

		k := key{targetID, load, bucket}
		a := buckets[k]
		if a == nil {
			a = &acc{}
			buckets[k] = a
		}
		// Jitter is only meaningful between consecutive samples of the same
		// series; crossing into a different target or bucket must break it.
		if !haveLast || last != k {
			a.ipdv.Reset()
			last, haveLast = k, true
		}

		o := metrics.Outcome(outcome)
		a.tally.Add(o)

		if rtt.Valid && (o == metrics.OutcomeOK || o == metrics.OutcomeLate) {
			v := rtt.Int64
			a.hist.Observe(v)
			a.ipdv.Observe(v)
			a.sum += v
			a.sumsq += v * v
			if !a.min.Valid || v < a.min.Int64 {
				a.min = sql.NullInt64{Int64: v, Valid: true}
			}
			if !a.max.Valid || v > a.max.Int64 {
				a.max = sql.NullInt64{Int64: v, Valid: true}
			}
		} else {
			// A lost packet breaks the consecutive-sample chain.
			a.ipdv.Reset()
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(buckets) == 0 {
		return 0, nil
	}

	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO icmp_agg
		(grain, target_id, bucket_s, load, sent, recv, late, lost, send_err,
		 min_us, max_us, sum_us, sumsq_us, ipdv_sum_us, ipdv_n, ipdv_max_us, hist)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	for k, a := range buckets {
		if _, err := stmt.ExecContext(ctx, Grain1m, k.target, k.bucket, k.load,
			a.tally.Sent, a.tally.Recv, a.tally.Late, a.tally.Lost, a.tally.SendErr,
			a.min, a.max, a.sum, a.sumsq,
			a.ipdv.SumUS, a.ipdv.N, a.ipdv.MaxUS, a.hist.Encode()); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(buckets), nil
}

// Coarsen merges finer buckets into a coarser grain.
//
// This is where the histogram earns its place: merging is element-wise
// addition, so the coarse row answers percentile queries exactly as if it had
// been computed from the original samples.
func (d *DB) Coarsen(ctx context.Context, from, to int, now time.Time) (int, error) {
	if to <= from {
		return 0, fmt.Errorf("coarsen: to grain %d must exceed from grain %d", to, from)
	}
	cutoff := now.Unix() / int64(to) * int64(to)

	rows, err := d.r.QueryContext(ctx, `
		SELECT target_id, load, bucket_s/?*? AS coarse,
		       sent, recv, late, lost, send_err, min_us, max_us, sum_us, sumsq_us,
		       ipdv_sum_us, ipdv_n, ipdv_max_us, hist
		FROM icmp_agg
		WHERE grain = ? AND bucket_s < ?`, to, to, from, cutoff)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type key struct {
		target int64
		load   int
		bucket int64
	}
	type acc struct {
		sent, recv, late, lost, sendErr int64
		min, max                        sql.NullInt64
		sum, sumsq                      int64
		ipdvSum, ipdvN, ipdvMax         int64
		hist                            metrics.Hist
	}

	out := map[key]*acc{}
	for rows.Next() {
		var k key
		var sent, recv, late, lost, sendErr int64
		var min, max sql.NullInt64
		var sum, sumsq, ipdvSum, ipdvN, ipdvMax int64
		var blob []byte
		if err := rows.Scan(&k.target, &k.load, &k.bucket,
			&sent, &recv, &late, &lost, &sendErr, &min, &max, &sum, &sumsq,
			&ipdvSum, &ipdvN, &ipdvMax, &blob); err != nil {
			return 0, err
		}
		h, err := metrics.Decode(blob)
		if err != nil {
			return 0, fmt.Errorf("bucket %d target %d: %w", k.bucket, k.target, err)
		}

		a := out[k]
		if a == nil {
			a = &acc{}
			out[k] = a
		}
		a.sent += sent
		a.recv += recv
		a.late += late
		a.lost += lost
		a.sendErr += sendErr
		a.sum += sum
		a.sumsq += sumsq
		a.ipdvSum += ipdvSum
		a.ipdvN += ipdvN
		if ipdvMax > a.ipdvMax {
			a.ipdvMax = ipdvMax
		}
		if min.Valid && (!a.min.Valid || min.Int64 < a.min.Int64) {
			a.min = min
		}
		if max.Valid && (!a.max.Valid || max.Int64 > a.max.Int64) {
			a.max = max
		}
		a.hist.Merge(h)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(out) == 0 {
		return 0, nil
	}

	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO icmp_agg
		(grain, target_id, bucket_s, load, sent, recv, late, lost, send_err,
		 min_us, max_us, sum_us, sumsq_us, ipdv_sum_us, ipdv_n, ipdv_max_us, hist)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	for k, a := range out {
		if _, err := stmt.ExecContext(ctx, to, k.target, k.bucket, k.load,
			a.sent, a.recv, a.late, a.lost, a.sendErr, a.min, a.max, a.sum, a.sumsq,
			a.ipdvSum, a.ipdvN, a.ipdvMax, a.hist.Encode()); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(out), nil
}

// Prune drops data that has already been rolled up into a coarser tier.
func (d *DB) Prune(ctx context.Context, now time.Time) error {
	rawCutoff := now.Add(-RetainRaw).UnixMicro()
	if _, err := d.w.ExecContext(ctx,
		`DELETE FROM icmp_raw WHERE ts_us < ?`, rawCutoff); err != nil {
		return err
	}
	if _, err := d.w.ExecContext(ctx,
		`DELETE FROM tcp_raw WHERE ts_us < ?`, rawCutoff); err != nil {
		return err
	}
	minuteCutoff := now.Add(-Retain1m).Unix()
	if _, err := d.w.ExecContext(ctx,
		`DELETE FROM icmp_agg WHERE grain = ? AND bucket_s < ?`,
		Grain1m, minuteCutoff); err != nil {
		return err
	}
	// Reclaim incrementally rather than with a full VACUUM, which would lock
	// the database for as long as it takes to rewrite the file.
	_, err := d.w.ExecContext(ctx, `PRAGMA incremental_vacuum(2000)`)
	return err
}

// Maintain runs the full rollup and retention cycle.
func (d *DB) Maintain(ctx context.Context, now time.Time) error {
	if _, err := d.RollupRaw(ctx, now); err != nil {
		return fmt.Errorf("rollup 1m: %w", err)
	}
	if _, err := d.Coarsen(ctx, Grain1m, Grain1h, now); err != nil {
		return fmt.Errorf("coarsen 1h: %w", err)
	}
	if _, err := d.Coarsen(ctx, Grain1h, Grain1d, now); err != nil {
		return fmt.Errorf("coarsen 1d: %w", err)
	}
	return d.Prune(ctx, now)
}
