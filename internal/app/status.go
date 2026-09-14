package app

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kokorhekkus/netwatch/internal/metrics"
	"github.com/kokorhekkus/netwatch/internal/store"
)

type TargetStatus struct {
	Label   string
	Kind    string
	Sent    int64
	Lost    int64
	SendErr int64
	P50     float64
	P95     float64
	P99     float64
	JitterM float64
	LossLo  float64
	LossHi  float64

	// Observations is how many round-trip times back the percentiles. It
	// should equal Sent-Lost; a mismatch means histogram rows were dropped
	// somewhere between storage and here.
	Observations uint64
}

// Status summarises the last window from the rollups.
func Status(ctx context.Context, db *store.DB, window time.Duration) ([]TargetStatus, error) {
	since := time.Now().Add(-window).Unix()

	// Deliberately not aggregated in SQL. Histograms cannot be summed by
	// SQLite, so the rows are merged in Go; grouping here would collapse two
	// buckets that happen to hold identical histograms into one and silently
	// drop the second one's samples.
	rows, err := db.Reader().QueryContext(ctx, `
		SELECT t.label, t.kind,
		       a.sent, a.lost, a.send_err,
		       a.ipdv_sum_us, a.ipdv_n,
		       a.hist
		FROM icmp_agg a JOIN target t ON t.id = a.target_id
		WHERE a.grain = ? AND a.bucket_s >= ? AND a.load = 0
		ORDER BY t.id, a.bucket_s`, store.Grain1m, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Merge per-target across buckets. Histograms add element-wise, which is
	// what makes an arbitrary window answerable from pre-aggregated rows.
	type agg struct {
		st   TargetStatus
		hist metrics.Hist
		isum int64
		in   int64
	}
	byLabel := map[string]*agg{}
	var order []string

	for rows.Next() {
		var label, kind string
		var sent, lost, sendErr, isum, in sql.NullInt64
		var blob []byte
		if err := rows.Scan(&label, &kind, &sent, &lost, &sendErr, &isum, &in, &blob); err != nil {
			return nil, err
		}
		a := byLabel[label]
		if a == nil {
			a = &agg{st: TargetStatus{Label: label, Kind: kind}}
			byLabel[label] = a
			order = append(order, label)
		}
		a.st.Sent += sent.Int64
		a.st.Lost += lost.Int64
		a.st.SendErr += sendErr.Int64
		a.isum += isum.Int64
		a.in += in.Int64
		if h, err := metrics.Decode(blob); err == nil {
			a.hist.Merge(h)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]TargetStatus, 0, len(order))
	for _, label := range order {
		a := byLabel[label]
		a.st.P50 = a.hist.Quantile(0.50)
		a.st.P95 = a.hist.Quantile(0.95)
		a.st.P99 = a.hist.Quantile(0.99)
		a.st.Observations = a.hist.Total()
		if a.in > 0 {
			a.st.JitterM = float64(a.isum) / float64(a.in)
		}
		a.st.LossLo, a.st.LossHi = metrics.WilsonInterval(
			uint64(a.st.Lost), uint64(a.st.Sent), 1.96)
		out = append(out, a.st)
	}
	return out, nil
}

func WriteStatus(w io.Writer, sts []TargetStatus, window time.Duration) {
	if len(sts) == 0 {
		fmt.Fprintf(w, "No data yet for the last %s.\n", window)
		fmt.Fprintln(w, "The daemon aggregates once a minute, so give it a couple of minutes.")
		return
	}

	fmt.Fprintf(w, "Last %s\n\n", window)
	fmt.Fprintf(w, "%-16s %7s %9s %9s %9s %9s  %s\n",
		"TARGET", "SENT", "p50", "p95", "p99", "JITTER", "LOSS")
	fmt.Fprintln(w, strings.Repeat("-", 78))

	for _, s := range sts {
		loss := "-"
		if s.Sent > 0 {
			// Report the interval, not the point estimate: at these sample
			// counts a bare percentage is mostly binomial noise.
			loss = fmt.Sprintf("%.1f-%.1f%%", s.LossLo*100, s.LossHi*100)
		}
		fmt.Fprintf(w, "%-16s %7d %9s %9s %9s %9s  %s\n",
			trunc(s.Label, 16), s.Sent,
			ms(s.P50), ms(s.P95), ms(s.P99), ms(s.JitterM), loss)
	}

	var errs int64
	for _, s := range sts {
		errs += s.SendErr
	}
	if errs > 0 {
		fmt.Fprintf(w, "\n%d local send failures (interface down) — excluded from loss.\n", errs)
	}
}

func ms(us float64) string {
	if us <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.1fms", us/1000)
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
