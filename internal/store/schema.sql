-- netwatch schema.
--
-- Retention is tiered rather than uniform: raw samples for forensics on a
-- recent incident, minute rollups for a season, and hour/day rollups kept
-- indefinitely. The rollups store a mergeable histogram rather than scalar
-- percentiles, because percentiles do not compose - an hourly p99 cannot be
-- rebuilt from sixty per-minute p99 values, but it can from sixty histograms.

CREATE TABLE IF NOT EXISTS network (
  id            INTEGER PRIMARY KEY,
  fingerprint   TEXT    NOT NULL UNIQUE,
  iface         TEXT    NOT NULL,
  kind          TEXT    NOT NULL,
  gw_ip         TEXT    NOT NULL,
  gw_mac        TEXT    NOT NULL,
  cidr          TEXT    NOT NULL,
  dhcp_domain   TEXT    NOT NULL,
  label         TEXT    NOT NULL,
  first_seen_ms INTEGER NOT NULL,
  last_seen_ms  INTEGER NOT NULL,
  -- Heavy probes are default-deny. Detecting a phone tether by heuristic is
  -- guesswork; requiring the network to be named trusted is not.
  allow_heavy   INTEGER NOT NULL DEFAULT 0,
  -- The ISP hop discovered for this network, pinned once.
  --
  -- Traceroute picks a different address run to run on a path with ECMP, so
  -- rediscovering on every start would create a new target row each time and
  -- shatter the history into dozens of short, separately-labelled series.
  isp_hop       TEXT NOT NULL DEFAULT ''
) STRICT;

-- A contiguous interval over which sampling was actually running and valid.
-- Anything outside an epoch is absence of evidence, not evidence of loss.
CREATE TABLE IF NOT EXISTS epoch (
  id         INTEGER PRIMARY KEY,
  network_id INTEGER NOT NULL REFERENCES network(id),
  started_ms INTEGER NOT NULL,
  ended_ms   INTEGER,
  end_reason TEXT,
  build      TEXT NOT NULL
) STRICT;
CREATE INDEX IF NOT EXISTS epoch_started ON epoch(started_ms);

-- Explicit "there is no data here, and this is why". The dashboard shades
-- these; it must never draw a line across one.
CREATE TABLE IF NOT EXISTS gap (
  id      INTEGER PRIMARY KEY,
  from_ms INTEGER NOT NULL,
  to_ms   INTEGER NOT NULL,
  cause   TEXT    NOT NULL
) STRICT;
CREATE INDEX IF NOT EXISTS gap_from ON gap(from_ms);

CREATE TABLE IF NOT EXISTS target (
  id     INTEGER PRIMARY KEY,
  kind   TEXT    NOT NULL,   -- gateway | isp_hop | anycast | control
  family INTEGER NOT NULL,   -- 4 | 6
  addr   TEXT    NOT NULL,
  label  TEXT    NOT NULL,
  UNIQUE(kind, family, addr)
) STRICT;

-- Raw ICMP samples. WITHOUT ROWID with this primary key clusters rows by
-- (target, time), which is the only way anything ever reads them.
CREATE TABLE IF NOT EXISTS icmp_raw (
  target_id INTEGER NOT NULL,
  ts_us     INTEGER NOT NULL,
  epoch_id  INTEGER NOT NULL,
  rtt_us    INTEGER,          -- NULL unless a reply arrived
  outcome   INTEGER NOT NULL,
  load      INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (target_id, ts_us)
) WITHOUT ROWID, STRICT;

CREATE TABLE IF NOT EXISTS tcp_raw (
  target_id  INTEGER NOT NULL,
  ts_us      INTEGER NOT NULL,
  epoch_id   INTEGER NOT NULL,
  connect_us INTEGER,
  outcome    INTEGER NOT NULL,
  load       INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (target_id, ts_us)
) WITHOUT ROWID, STRICT;

-- Rollups. `load` is part of the key so that loaded and unloaded samples are
-- never mixed into one bucket: the difference between them is the bufferbloat
-- measurement, which is destroyed by averaging them together.
CREATE TABLE IF NOT EXISTS icmp_agg (
  grain      INTEGER NOT NULL,   -- bucket width in seconds: 60, 3600, 86400
  target_id  INTEGER NOT NULL,
  bucket_s   INTEGER NOT NULL,
  load       INTEGER NOT NULL,
  sent       INTEGER NOT NULL,
  recv       INTEGER NOT NULL,
  late       INTEGER NOT NULL,
  lost       INTEGER NOT NULL,
  send_err   INTEGER NOT NULL,   -- excluded from the loss denominator
  min_us     INTEGER,
  max_us     INTEGER,
  sum_us     INTEGER NOT NULL,
  sumsq_us   INTEGER NOT NULL,
  ipdv_sum_us INTEGER NOT NULL,
  ipdv_n      INTEGER NOT NULL,
  ipdv_max_us INTEGER NOT NULL,
  hist       BLOB    NOT NULL,
  PRIMARY KEY (grain, target_id, bucket_s, load)
) WITHOUT ROWID, STRICT;

CREATE TABLE IF NOT EXISTS dns_raw (
  resolver   TEXT    NOT NULL,
  ts_us      INTEGER NOT NULL,
  epoch_id   INTEGER NOT NULL,
  -- Cold queries use a random label so the resolver cannot answer from cache;
  -- warm queries repeat a popular name. Timing them together would average a
  -- cache hit with a full recursion and describe neither.
  cold       INTEGER NOT NULL,
  latency_us INTEGER,
  rcode      INTEGER,
  answers    INTEGER,
  answer_hash TEXT NOT NULL DEFAULT '',
  outcome    INTEGER NOT NULL,
  PRIMARY KEY (resolver, cold, ts_us)
) WITHOUT ROWID, STRICT;

CREATE TABLE IF NOT EXISTS http_raw (
  url        TEXT    NOT NULL,
  ts_us      INTEGER NOT NULL,
  epoch_id   INTEGER NOT NULL,
  dns_us     INTEGER,
  connect_us INTEGER,
  tls_us     INTEGER,
  ttfb_us    INTEGER,
  total_us   INTEGER,
  status     INTEGER,
  server_ip  TEXT NOT NULL DEFAULT '',
  outcome    INTEGER NOT NULL,
  PRIMARY KEY (url, ts_us)
) WITHOUT ROWID, STRICT;

-- Radio telemetry, sampled alongside latency so the two can be compared.
-- A jitter spike that coincides with an RSSI drop is a Wi-Fi problem; one that
-- does not is somebody else's.
CREATE TABLE IF NOT EXISTS wifi_raw (
  ts_us     INTEGER PRIMARY KEY,
  epoch_id  INTEGER NOT NULL,
  rssi      INTEGER NOT NULL,
  noise     INTEGER NOT NULL,
  tx_rate   REAL    NOT NULL,
  channel   INTEGER NOT NULL,
  width_mhz INTEGER NOT NULL,
  band      TEXT    NOT NULL,
  phy_mode  TEXT    NOT NULL
) WITHOUT ROWID, STRICT;

CREATE TABLE IF NOT EXISTS throughput (
  id                INTEGER PRIMARY KEY,
  ts_us             INTEGER NOT NULL,
  epoch_id          INTEGER NOT NULL,
  engine            TEXT    NOT NULL,
  dl_mbps           REAL,
  ul_mbps           REAL,
  dl_rpm            INTEGER,
  ul_rpm            INTEGER,
  base_rtt_us       INTEGER,
  loaded_rtt_p50_us INTEGER,
  loaded_rtt_p95_us INTEGER,
  bytes_down        INTEGER,
  bytes_up          INTEGER,
  aborted           INTEGER NOT NULL DEFAULT 0,
  raw_json          TEXT
) STRICT;
CREATE INDEX IF NOT EXISTS throughput_ts ON throughput(ts_us);

CREATE TABLE IF NOT EXISTS event (
  id     INTEGER PRIMARY KEY,
  ts_us  INTEGER NOT NULL,
  kind   TEXT    NOT NULL,
  detail TEXT    NOT NULL DEFAULT ''
) STRICT;
CREATE INDEX IF NOT EXISTS event_ts ON event(ts_us);

CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
) STRICT;
