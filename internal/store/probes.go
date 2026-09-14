package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/kokorhekkus/netwatch/internal/metrics"
)

type DNSSample struct {
	Resolver   string
	EpochID    int64
	Cold       bool
	LatencyUS  sql.NullInt64
	Rcode      sql.NullInt64
	Answers    sql.NullInt64
	AnswerHash string
	Outcome    metrics.Outcome
}

func (d *DB) InsertDNS(ctx context.Context, s DNSSample) error {
	cold := 0
	if s.Cold {
		cold = 1
	}
	_, err := d.w.ExecContext(ctx, `INSERT OR REPLACE INTO dns_raw
		(resolver, ts_us, epoch_id, cold, latency_us, rcode, answers, answer_hash, outcome)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		s.Resolver, time.Now().UnixMicro(), s.EpochID, cold,
		s.LatencyUS, s.Rcode, s.Answers, s.AnswerHash, int(s.Outcome))
	return err
}

type HTTPSample struct {
	URL       string
	EpochID   int64
	DNSUS     sql.NullInt64
	ConnectUS sql.NullInt64
	TLSUS     sql.NullInt64
	TTFBUS    sql.NullInt64
	TotalUS   sql.NullInt64
	Status    sql.NullInt64
	ServerIP  string
	Outcome   metrics.Outcome
}

func (d *DB) InsertHTTP(ctx context.Context, s HTTPSample) error {
	_, err := d.w.ExecContext(ctx, `INSERT OR REPLACE INTO http_raw
		(url, ts_us, epoch_id, dns_us, connect_us, tls_us, ttfb_us, total_us,
		 status, server_ip, outcome)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		s.URL, time.Now().UnixMicro(), s.EpochID,
		s.DNSUS, s.ConnectUS, s.TLSUS, s.TTFBUS, s.TotalUS,
		s.Status, s.ServerIP, int(s.Outcome))
	return err
}

type ThroughputSample struct {
	EpochID     int64
	Engine      string
	DownMbps    float64
	UpMbps      float64
	DownRPM     int
	UpRPM       int
	BaseRTTUS   int64
	LoadedP50US int64
	LoadedP95US int64
	BytesDown   int64
	BytesUp     int64
	Aborted     bool
	RawJSON     string
}

func (d *DB) InsertThroughput(ctx context.Context, s ThroughputSample) error {
	aborted := 0
	if s.Aborted {
		aborted = 1
	}
	_, err := d.w.ExecContext(ctx, `INSERT INTO throughput
		(ts_us, epoch_id, engine, dl_mbps, ul_mbps, dl_rpm, ul_rpm,
		 base_rtt_us, loaded_rtt_p50_us, loaded_rtt_p95_us,
		 bytes_down, bytes_up, aborted, raw_json)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		time.Now().UnixMicro(), s.EpochID, s.Engine, s.DownMbps, s.UpMbps,
		s.DownRPM, s.UpRPM, s.BaseRTTUS, s.LoadedP50US, s.LoadedP95US,
		s.BytesDown, s.BytesUp, aborted, s.RawJSON)
	return err
}

// ThroughputRow is one capacity measurement, for the dashboard.
type ThroughputRow struct {
	TS          int64   `json:"ts"`
	Engine      string  `json:"engine"`
	DownMbps    float64 `json:"downMbps"`
	UpMbps      float64 `json:"upMbps"`
	DownRPM     int     `json:"downRpm"`
	BaseRTTUS   int64   `json:"baseRttUs"`
	LoadedP95US int64   `json:"loadedP95Us"`
	BytesDown   int64   `json:"bytesDown"`
	BytesUp     int64   `json:"bytesUp"`
}

func (d *DB) QueryThroughput(ctx context.Context, limit int) ([]ThroughputRow, error) {
	query := `
		SELECT ts_us/1000000, engine, dl_mbps, ul_mbps, dl_rpm,
		       base_rtt_us, loaded_rtt_p95_us, bytes_down, bytes_up
		FROM throughput WHERE aborted = 0 ORDER BY ts_us DESC`
	args := []any{}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := d.r.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ThroughputRow
	for rows.Next() {
		var r ThroughputRow
		var dl, ul sql.NullFloat64
		var rpm, base, p95, bd, bu sql.NullInt64
		if err := rows.Scan(&r.TS, &r.Engine, &dl, &ul, &rpm, &base, &p95, &bd, &bu); err != nil {
			return nil, err
		}
		r.DownMbps, r.UpMbps = dl.Float64, ul.Float64
		r.DownRPM = int(rpm.Int64)
		r.BaseRTTUS, r.LoadedP95US = base.Int64, p95.Int64
		r.BytesDown, r.BytesUp = bd.Int64, bu.Int64
		out = append(out, r)
	}
	return out, rows.Err()
}

// MonthlyHeavyBytes totals what capacity tests have spent this month, so the
// budget survives a restart rather than resetting to zero each time.
func (d *DB) MonthlyHeavyBytes(ctx context.Context, month string) (int64, error) {
	var n sql.NullInt64
	err := d.r.QueryRowContext(ctx, `
		SELECT SUM(COALESCE(bytes_down,0) + COALESCE(bytes_up,0)) FROM throughput
		WHERE strftime('%Y-%m', ts_us/1000000, 'unixepoch') = ?`, month).Scan(&n)
	if err != nil && err != sql.ErrNoRows {
		return 0, err
	}
	return n.Int64, nil
}

// LastThroughputAt reports when a capacity test last completed.
func (d *DB) LastThroughputAt(ctx context.Context) (time.Time, error) {
	var ts sql.NullInt64
	err := d.r.QueryRowContext(ctx,
		`SELECT MAX(ts_us) FROM throughput WHERE aborted = 0`).Scan(&ts)
	if err != nil && err != sql.ErrNoRows {
		return time.Time{}, err
	}
	if !ts.Valid {
		return time.Time{}, nil
	}
	return time.UnixMicro(ts.Int64), nil
}

// DNSSummary aggregates recent DNS timings per resolver.
type DNSSummary struct {
	Resolver string  `json:"resolver"`
	Cold     bool    `json:"cold"`
	N        int64   `json:"n"`
	MeanUS   float64 `json:"meanUs"`
	Failures int64   `json:"failures"`
}

func (d *DB) QueryDNSSummary(ctx context.Context, since time.Time) ([]DNSSummary, error) {
	rows, err := d.r.QueryContext(ctx, `
		SELECT resolver, cold, COUNT(*), AVG(latency_us),
		       SUM(CASE WHEN outcome NOT IN (0,1) THEN 1 ELSE 0 END)
		FROM dns_raw WHERE ts_us >= ?
		GROUP BY resolver, cold ORDER BY resolver, cold`, since.UnixMicro())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DNSSummary
	for rows.Next() {
		var s DNSSummary
		var cold int
		var avg sql.NullFloat64
		if err := rows.Scan(&s.Resolver, &cold, &s.N, &avg, &s.Failures); err != nil {
			return nil, err
		}
		s.Cold = cold == 1
		s.MeanUS = avg.Float64
		out = append(out, s)
	}
	return out, rows.Err()
}

// HTTPSummary averages the request phases over a window.
type HTTPSummary struct {
	URL       string  `json:"url"`
	N         int64   `json:"n"`
	DNSUS     float64 `json:"dnsUs"`
	ConnectUS float64 `json:"connectUs"`
	TLSUS     float64 `json:"tlsUs"`
	TTFBUS    float64 `json:"ttfbUs"`
	Failures  int64   `json:"failures"`
}

func (d *DB) QueryHTTPSummary(ctx context.Context, since time.Time) ([]HTTPSummary, error) {
	rows, err := d.r.QueryContext(ctx, `
		SELECT url, COUNT(*), AVG(dns_us), AVG(connect_us), AVG(tls_us), AVG(ttfb_us),
		       SUM(CASE WHEN outcome NOT IN (0,1) THEN 1 ELSE 0 END)
		FROM http_raw WHERE ts_us >= ? GROUP BY url ORDER BY url`, since.UnixMicro())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []HTTPSummary
	for rows.Next() {
		var s HTTPSummary
		var dns, conn, tlsv, ttfb sql.NullFloat64
		if err := rows.Scan(&s.URL, &s.N, &dns, &conn, &tlsv, &ttfb, &s.Failures); err != nil {
			return nil, err
		}
		s.DNSUS, s.ConnectUS, s.TLSUS, s.TTFBUS =
			dns.Float64, conn.Float64, tlsv.Float64, ttfb.Float64
		out = append(out, s)
	}
	return out, rows.Err()
}

type WiFiSample struct {
	EpochID  int64
	RSSI     int
	Noise    int
	TxRate   float64
	Channel  int
	WidthMHz int
	Band     string
	PHYMode  string
}

func (d *DB) InsertWiFi(ctx context.Context, s WiFiSample) error {
	_, err := d.w.ExecContext(ctx, `INSERT OR REPLACE INTO wifi_raw
		(ts_us, epoch_id, rssi, noise, tx_rate, channel, width_mhz, band, phy_mode)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		time.Now().UnixMicro(), s.EpochID, s.RSSI, s.Noise, s.TxRate,
		s.Channel, s.WidthMHz, s.Band, s.PHYMode)
	return err
}

// WiFiPoint is one radio reading for the dashboard.
type WiFiPoint struct {
	TS     int64   `json:"ts"`
	RSSI   int     `json:"rssi"`
	Noise  int     `json:"noise"`
	SNR    int     `json:"snr"`
	TxRate float64 `json:"txRate"`
}

// QueryWiFi returns radio readings over a window, thinned to about `points`.
//
// Thinning keeps the shape (the dips are what matter) without shipping one
// row per 30 seconds across a month.
func (d *DB) QueryWiFi(ctx context.Context, from, to time.Time, points int) ([]WiFiPoint, error) {
	if points <= 0 {
		points = 500
	}
	span := to.Unix() - from.Unix()
	if span <= 0 {
		return nil, nil
	}
	bucket := span / int64(points)
	if bucket < 1 {
		bucket = 1
	}

	rows, err := d.r.QueryContext(ctx, `
		SELECT (ts_us/1000000/?)*? AS slot,
		       AVG(rssi), AVG(noise), AVG(tx_rate)
		FROM wifi_raw
		WHERE ts_us >= ? AND ts_us < ?
		GROUP BY slot ORDER BY slot`,
		bucket, bucket, from.UnixMicro(), to.UnixMicro())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []WiFiPoint
	for rows.Next() {
		var p WiFiPoint
		var rssi, noise, tx float64
		if err := rows.Scan(&p.TS, &rssi, &noise, &tx); err != nil {
			return nil, err
		}
		p.RSSI, p.Noise, p.TxRate = int(rssi), int(noise), tx
		p.SNR = p.RSSI - p.Noise
		out = append(out, p)
	}
	return out, rows.Err()
}

// LatestWiFi returns the most recent radio reading.
func (d *DB) LatestWiFi(ctx context.Context) (WiFiPoint, bool, error) {
	var p WiFiPoint
	err := d.r.QueryRowContext(ctx, `
		SELECT ts_us/1000000, rssi, noise, tx_rate
		FROM wifi_raw ORDER BY ts_us DESC LIMIT 1`).
		Scan(&p.TS, &p.RSSI, &p.Noise, &p.TxRate)
	if err == sql.ErrNoRows {
		return p, false, nil
	}
	if err != nil {
		return p, false, err
	}
	p.SNR = p.RSSI - p.Noise
	return p, true, nil
}

// PruneProbes drops DNS, HTTP and radio rows past the raw retention window.
func (d *DB) PruneProbes(ctx context.Context, now time.Time) error {
	cutoff := now.Add(-RetainRaw).UnixMicro()
	for _, table := range []string{"dns_raw", "http_raw", "wifi_raw"} {
		if _, err := d.w.ExecContext(ctx,
			`DELETE FROM `+table+` WHERE ts_us < ?`, cutoff); err != nil {
			return err
		}
	}
	return nil
}
