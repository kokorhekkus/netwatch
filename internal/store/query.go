package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/kokorhekkus/netwatch/internal/metrics"
)

// Bucket is one point of a rendered series.
//
// Every latency figure is a histogram quantile rather than an average, and
// loss carries an interval rather than a bare ratio, so a chart drawn from
// these cannot imply more certainty than the samples support.
type Bucket struct {
	TS      int64   `json:"ts"` // unix seconds, bucket start
	Sent    int64   `json:"sent"`
	Lost    int64   `json:"lost"`
	SendErr int64   `json:"sendErr"`
	P50     float64 `json:"p50"` // microseconds
	P95     float64 `json:"p95"`
	P99     float64 `json:"p99"`
	Min     float64 `json:"min"`
	Jitter  float64 `json:"jitter"`
	LossLo  float64 `json:"lossLo"`
	LossHi  float64 `json:"lossHi"`
}

type Series struct {
	TargetID int64    `json:"targetId"`
	Label    string   `json:"label"`
	Kind     string   `json:"kind"`
	Buckets  []Bucket `json:"buckets"`
}

type Gap struct {
	From  int64  `json:"from"` // unix seconds
	To    int64  `json:"to"`
	Cause string `json:"cause"`
}

type Event struct {
	TS     int64  `json:"ts"`
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

// PickGrain chooses the stored tier to read for a given time range.
//
// The browser is never handed raw samples: a month at one-minute resolution is
// 43,000 points per target, both slow to transfer and finer than any screen
// can show. Reading a coarser tier is what the tiered rollup exists for.
//
// Selection is by range, deliberately *not* by ideal bucket width. Coarser
// tiers are built only from closed buckets, so the hourly tier lags reality by
// up to an hour and the daily tier by up to a day. Choosing a tier because its
// bucket width matched the pixel budget would leave the most recent - and most
// interesting - part of a short chart mysteriously blank. The minute tier is
// therefore used for anything up to a week, and the output buckets are merged
// down to the point budget in Go afterwards.
//
// Row counts stay modest: a week of minutes is ~10k rows per target, a
// two-year range at hourly grain ~17k.
func PickGrain(rangeSec int64, points int) int {
	const week = 7 * 24 * 3600
	const twoYears = 2 * 365 * 24 * 3600

	switch {
	case rangeSec <= week:
		return Grain1m
	case rangeSec <= twoYears:
		return Grain1h
	default:
		return Grain1d
	}
}

// QuerySeries returns per-target buckets for a time range.
//
// Rows at the chosen grain are merged into output buckets by adding their
// histograms, which is exact - not an approximation of the finer data.
func (d *DB) QuerySeries(ctx context.Context, from, to time.Time, points int, load int) ([]Series, error) {
	fromS, toS := from.Unix(), to.Unix()
	if toS <= fromS {
		return nil, nil
	}
	grain := PickGrain(toS-fromS, points)

	width := int64(grain)
	if w := (toS - fromS) / int64(points); w > width {
		// Round up to a whole number of stored buckets so no row straddles
		// two output buckets.
		width = ((w + int64(grain) - 1) / int64(grain)) * int64(grain)
	}

	rows, err := d.r.QueryContext(ctx, `
		SELECT a.target_id, t.label, t.kind, a.bucket_s,
		       a.sent, a.lost, a.send_err, a.min_us,
		       a.ipdv_sum_us, a.ipdv_n, a.hist
		FROM icmp_agg a JOIN target t ON t.id = a.target_id
		WHERE a.grain = ? AND a.bucket_s >= ? AND a.bucket_s < ? AND a.load = ?
		ORDER BY a.target_id, a.bucket_s`, grain, fromS, toS, load)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type acc struct {
		sent, lost, sendErr int64
		min                 sql.NullInt64
		ipdvSum, ipdvN      int64
		hist                metrics.Hist
	}
	type seriesAcc struct {
		s       Series
		buckets map[int64]*acc
		order   []int64
	}

	byTarget := map[int64]*seriesAcc{}
	var targetOrder []int64

	for rows.Next() {
		var targetID, bucketS, sent, lost, sendErr, ipdvSum, ipdvN int64
		var label, kind string
		var min sql.NullInt64
		var blob []byte
		if err := rows.Scan(&targetID, &label, &kind, &bucketS,
			&sent, &lost, &sendErr, &min, &ipdvSum, &ipdvN, &blob); err != nil {
			return nil, err
		}

		sa := byTarget[targetID]
		if sa == nil {
			sa = &seriesAcc{
				s:       Series{TargetID: targetID, Label: label, Kind: kind},
				buckets: map[int64]*acc{},
			}
			byTarget[targetID] = sa
			targetOrder = append(targetOrder, targetID)
		}

		slot := bucketS / width * width
		a := sa.buckets[slot]
		if a == nil {
			a = &acc{}
			sa.buckets[slot] = a
			sa.order = append(sa.order, slot)
		}
		a.sent += sent
		a.lost += lost
		a.sendErr += sendErr
		a.ipdvSum += ipdvSum
		a.ipdvN += ipdvN
		if min.Valid && (!a.min.Valid || min.Int64 < a.min.Int64) {
			a.min = min
		}
		if h, err := metrics.Decode(blob); err == nil {
			a.hist.Merge(h)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]Series, 0, len(targetOrder))
	for _, tid := range targetOrder {
		sa := byTarget[tid]
		for _, slot := range sa.order {
			a := sa.buckets[slot]
			b := Bucket{
				TS:      slot,
				Sent:    a.sent,
				Lost:    a.lost,
				SendErr: a.sendErr,
				P50:     a.hist.Quantile(0.50),
				P95:     a.hist.Quantile(0.95),
				P99:     a.hist.Quantile(0.99),
			}
			if a.min.Valid {
				b.Min = float64(a.min.Int64)
			}
			if a.ipdvN > 0 {
				b.Jitter = float64(a.ipdvSum) / float64(a.ipdvN)
			}
			b.LossLo, b.LossHi = metrics.WilsonInterval(
				uint64(a.lost), uint64(a.sent), 1.96)
			sa.s.Buckets = append(sa.s.Buckets, b)
		}
		out = append(out, sa.s)
	}
	return out, nil
}

// QueryGaps returns the periods with no valid data in a range.
func (d *DB) QueryGaps(ctx context.Context, from, to time.Time) ([]Gap, error) {
	rows, err := d.r.QueryContext(ctx, `
		SELECT from_ms/1000, to_ms/1000, cause FROM gap
		WHERE to_ms >= ? AND from_ms <= ? ORDER BY from_ms`,
		from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Gap
	for rows.Next() {
		var g Gap
		if err := rows.Scan(&g.From, &g.To, &g.Cause); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (d *DB) QueryEvents(ctx context.Context, from, to time.Time, limit int) ([]Event, error) {
	rows, err := d.r.QueryContext(ctx, `
		SELECT ts_us/1000000, kind, detail FROM event
		WHERE ts_us >= ? AND ts_us <= ? ORDER BY ts_us DESC LIMIT ?`,
		from.UnixMicro(), to.UnixMicro(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.TS, &e.Kind, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CurrentNetwork returns the most recently seen network.
func (d *DB) CurrentNetwork(ctx context.Context) (label, fingerprint, kind string, err error) {
	err = d.r.QueryRowContext(ctx,
		`SELECT label, fingerprint, kind FROM network ORDER BY last_seen_ms DESC LIMIT 1`).
		Scan(&label, &fingerprint, &kind)
	if err == sql.ErrNoRows {
		return "", "", "", nil
	}
	return
}
