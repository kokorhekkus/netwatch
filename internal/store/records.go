package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/kokorhekkus/netwatch/internal/netid"
)

func nowMillis() int64 { return time.Now().UnixMilli() }
func nowMicros() int64 { return time.Now().UnixMicro() }

// UpsertNetwork records the network and returns its id, preserving the
// allow_heavy flag a user has already set for it.
func (d *DB) UpsertNetwork(ctx context.Context, n netid.Network) (int64, error) {
	fp := n.Fingerprint()
	now := nowMillis()

	var id int64
	err := d.w.QueryRowContext(ctx,
		`SELECT id FROM network WHERE fingerprint = ?`, fp).Scan(&id)
	switch {
	case err == sql.ErrNoRows:
		res, err := d.w.ExecContext(ctx, `INSERT INTO network
			(fingerprint, iface, kind, gw_ip, gw_mac, cidr, dhcp_domain, label,
			 first_seen_ms, last_seen_ms, allow_heavy)
			VALUES (?,?,?,?,?,?,?,?,?,?,0)`,
			fp, n.Iface, string(n.Kind), n.GatewayIP, n.GatewayMAC, n.CIDR,
			n.DHCPDomain, n.Label(), now, now)
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	case err != nil:
		return 0, err
	}

	_, err = d.w.ExecContext(ctx,
		`UPDATE network SET last_seen_ms = ?, iface = ? WHERE id = ?`, now, n.Iface, id)
	return id, err
}

// ISPHop returns the hop pinned for this network, if one has been discovered.
func (d *DB) ISPHop(ctx context.Context, networkID int64) (string, error) {
	var hop string
	err := d.r.QueryRowContext(ctx,
		`SELECT isp_hop FROM network WHERE id = ?`, networkID).Scan(&hop)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return hop, err
}

func (d *DB) SetISPHop(ctx context.Context, networkID int64, hop string) error {
	_, err := d.w.ExecContext(ctx,
		`UPDATE network SET isp_hop = ? WHERE id = ?`, hop, networkID)
	return err
}

func (d *DB) AllowHeavy(ctx context.Context, networkID int64) (bool, error) {
	var v int
	err := d.r.QueryRowContext(ctx,
		`SELECT allow_heavy FROM network WHERE id = ?`, networkID).Scan(&v)
	return v == 1, err
}

func (d *DB) SetAllowHeavy(ctx context.Context, networkID int64, allow bool) error {
	v := 0
	if allow {
		v = 1
	}
	_, err := d.w.ExecContext(ctx,
		`UPDATE network SET allow_heavy = ? WHERE id = ?`, v, networkID)
	return err
}

func (d *DB) OpenEpoch(ctx context.Context, networkID int64, build string) (int64, error) {
	res, err := d.w.ExecContext(ctx,
		`INSERT INTO epoch (network_id, started_ms, build) VALUES (?,?,?)`,
		networkID, nowMillis(), build)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (d *DB) CloseEpoch(ctx context.Context, epochID int64, reason string) error {
	_, err := d.w.ExecContext(ctx,
		`UPDATE epoch SET ended_ms = ?, end_reason = ? WHERE id = ? AND ended_ms IS NULL`,
		nowMillis(), reason, epochID)
	return err
}

// CloseOrphanEpochs marks epochs left open by a crash or power loss.
//
// Without this, a killed daemon leaves an epoch that appears to run to the
// present, and the dashboard would claim to have been measuring during hours
// when nothing was running.
func (d *DB) CloseOrphanEpochs(ctx context.Context) (int64, error) {
	res, err := d.w.ExecContext(ctx, `
		UPDATE epoch SET ended_ms = (
			SELECT COALESCE(MAX(ts_us)/1000, started_ms) FROM icmp_raw
			WHERE icmp_raw.epoch_id = epoch.id
		), end_reason = 'crash'
		WHERE ended_ms IS NULL`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (d *DB) RecordGap(ctx context.Context, fromMS, toMS int64, cause string) error {
	_, err := d.w.ExecContext(ctx,
		`INSERT INTO gap (from_ms, to_ms, cause) VALUES (?,?,?)`, fromMS, toMS, cause)
	return err
}

func (d *DB) RecordEvent(ctx context.Context, kind, detail string) error {
	_, err := d.w.ExecContext(ctx,
		`INSERT INTO event (ts_us, kind, detail) VALUES (?,?,?)`, nowMicros(), kind, detail)
	return err
}

// InsertICMPRaw writes a single sample immediately.
//
// The daemon uses the batching Writer instead; this is for one-shot paths and
// for tests that need a known dataset on disk.
func (d *DB) InsertICMPRaw(ctx context.Context, s Sample) error {
	_, err := d.w.ExecContext(ctx, `INSERT OR REPLACE INTO icmp_raw
		(target_id, ts_us, epoch_id, rtt_us, outcome, load) VALUES (?,?,?,?,?,?)`,
		s.TargetID, s.TSMicro, s.EpochID, s.ValueUS, int(s.Outcome), s.Load)
	return err
}

func (d *DB) UpsertTarget(ctx context.Context, kind string, family int, addr, label string) (int64, error) {
	var id int64
	err := d.w.QueryRowContext(ctx,
		`SELECT id FROM target WHERE kind = ? AND family = ? AND addr = ?`,
		kind, family, addr).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, err
	}
	res, err := d.w.ExecContext(ctx,
		`INSERT INTO target (kind, family, addr, label) VALUES (?,?,?,?)`,
		kind, family, addr, label)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}
