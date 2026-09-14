# netwatch

Continuous home internet quality monitor for macOS. Samples latency, jitter and
loss against several targets, stores everything in SQLite, and keeps a
multi-year history in about fifteen megabytes a year.

It exists because the usual tools are one-shot and anecdotal: you notice a call
stuttering, run a speed test, it looks fine, and you learn nothing. Home
connections rarely fail by being slow. They fail by being intermittent.

## Status

Phases 1 to 3. ICMP, TCP, DNS and HTTP probing; capacity and bufferbloat
measurement; network fingerprinting; sleep detection; tiered rollups; a local
dashboard; and a launchd agent. Per-hop path analysis and evidence export are
not built.

## Usage

```
netwatch install                # install and start the background agent
netwatch run                    # or run in the foreground
netwatch status -window 6h      # summarise recent quality
netwatch probe -n 20            # one-shot, comparable with ping(8)
netwatch trust                  # allow capacity tests on this network
netwatch speedtest              # run one now (~315 MB)
netwatch uninstall              # stop the agent (keeps the database)
```

## Capacity tests are opt-in and metered

A capacity test moves about **315 MB** and takes 45 seconds, measured with a
`netstat` counter delta rather than taken on trust. Three gates apply, and a
manual run skips only the scheduling ones:

- The network must be explicitly marked with `netwatch trust`. Detecting a
  phone tether by heuristic is guesswork, and being wrong spends somebody's
  cellular allowance, so unknown networks are refused.
- A monthly byte budget (25 GB) that survives restarts, rebuilt from what is
  already recorded rather than reset to zero on launch.
- At most one run per 12 hours, and only while the link has been idle for a
  minute — a test run against a busy link measures the contention, not the
  link.

Latency probes keep running throughout and their samples are tagged as loaded,
which is what produces the bufferbloat measurement.

The dashboard is at <http://127.0.0.1:7717>. It binds loopback explicitly:
a wildcard bind would count as an incoming connection to the Application
Firewall and prompt on every rebuild of an ad-hoc-signed binary.

## Dashboard

Every latency figure is a histogram quantile, drawn on a log scale — a router
at 4 ms and a stalled anycast probe at 900 ms share one axis, and on a linear
scale the router is a flat line pinned to zero. Loss is drawn as a confidence
interval rather than a point estimate. Gaps are shaded, never interpolated
across: a line joined over an overnight suspend would claim measurements that
were never taken.

All assets are embedded in the binary. Nothing is fetched from a CDN, because
the dashboard has to work when the internet is down — which is exactly when
somebody opens it.

## What it measures, and why that way

**Percentiles, never averages.** A mean hides exactly the tail events that make
a connection feel bad. During development, twenty pings to 1.1.1.1 averaged
20.6 ms purely because one packet took 72 ms; the median was 16.9 ms. The
average described nothing that happened.

**Loss separated from local failure.** A timeout, an unreachable destination and
a failed `sendto` are three different facts. Only the first two are the
network's doing. Counting local send errors as loss would turn every overnight
suspend into eight hours of "100% packet loss", so they are excluded from the
loss denominator entirely.

**Poisson sampling, not a fixed tick.** A regular 3-second tick aliases with
regular network behaviour — Wi-Fi off-channel scans, DFS checks, a neighbour's
cron job — so it can systematically miss a recurring problem or systematically
land on it, with no way to tell which from the data. Inter-arrivals are
exponentially distributed per RFC 2330. Targets are also phase-offset from each
other, so probes never burst together and measure our own serialisation delay.

**Radio state sampled alongside latency.** On Wi-Fi, a weak signal and a
congested ISP look identical from latency alone — both are jitter and loss. The
dashboard plots signal-to-noise margin under the latency chart so the two can
be compared directly: a jitter spike that coincides with an SNR dip is the
radio, one that does not is somebody else's. SNR matters more than raw RSSI,
because −60 dBm in a quiet band is a good link while −60 dBm against a −65 dBm
noise floor is not.

**Bufferbloat as a first-class metric.** Headline speed and packet loss can
both look perfect on a connection that feels broken. What usually explains it
is latency under load: oversized buffers in the router or modem let a bulk
transfer build a deep queue that every other packet then waits behind. The
dashboard draws idle and loaded latency as bars on one scale, because the
comparison is the point.

**DNS measured cold and warm, per resolver.** A cold lookup uses a random label
so it cannot be answered from cache and must reach an authoritative server; a
warm one repeats a popular name. Averaging the two describes neither. Running
the same pair against the router and two public resolvers is what separates
"my router's DNS is slow" from "my line is slow" — a distinction with a
five-minute fix.

**A TCP control probe.** Routers deprioritise and rate-limit ICMP, so the first
objection to any ICMP graph — including from an ISP's support desk — is that
ICMP is not representative. A TCP handshake to `1.1.1.1:443` is forwarded in the
data plane like real traffic. When the two series agree, the ICMP data is
corroborated; when they diverge, that is itself the finding.

**Loss shown as a confidence interval.** At twenty samples a minute, a raw loss
percentage is a 0/5/10% sawtooth that looks like a failing link but is only
binomial noise. `status` reports a Wilson interval instead.

## Platform findings

These were established experimentally on macOS 26 and are the reason several
parts of the code look the way they do.

**Unprivileged ICMP works.** `SOCK_DGRAM`/`IPPROTO_ICMP` needs no root and no
setuid binary.

**ICMP replies are delivered promiscuously.** macOS hands *every* ICMP reply to
*every* open datagram ICMP socket, not just the one that sent the request. Two
sockets each sent one echo and both received both replies; a socket that sent
nothing still received another socket's TimeExceeded. Filtering on our own echo
ID and a per-process payload magic is therefore required for correctness — a
`ping` running in a terminal would otherwise be counted as our own samples — and
an unmatched reply is normal rather than an error.

**The echo ID is preserved**, unlike Linux, which rewrites it with the socket's
implicit port.

**`x/net/icmp` strips the IPv4 header** that Darwin prepends on datagram ICMP
reads. `ParseMessage` can be called on the buffer directly. A hand-rolled
`unix.Socket` would need to skip `IHL*4` bytes first.

**Reply TTL is unavailable.** `SetControlMessage(FlagTTL)` fails with
`setsockopt: invalid argument`, and with the IP header stripped there is no
other route to it. Anycast PoP changes are better detected with a
`CH TXT id.server` query, which returns a specific server name (`lhr21`) rather
than a coarse region.

**SSID is not usable for network identity.** macOS gates it behind Location
Services, and `networksetup -getairportnetwork` reports "not associated" on
macOS 26 even when associated. The gateway MAC needs no permission and is a
better key anyway: it distinguishes two APs sharing an SSID, survives renames,
and works on Ethernet.

**Wi-Fi RSSI needs CoreWLAN; every shell route is a dead end.**
`system_profiler SPAirPortDataType` costs 4.2 seconds and triggers a scan for
nearby networks — which can itself cause the latency spike being measured.
`ipconfig getsummary` omits RSSI, `wdutil` needs sudo, and the `airport` binary
was removed in macOS 14.4. A small cgo shim over CoreWLAN reads the same values
in **4 ms**, a thousandfold cheaper.

**CoreWLAN does not gate RSSI behind Location Services.** Only `ssid` and
`bssid` are gated; RSSI, noise, transmit rate, channel, width, band and PHY
mode all read without a permission prompt. Since network identity comes from
the gateway MAC, netwatch never asks for the gated fields — a background agent
requesting location access would be a poor trade for data it does not need.

**Two clocks detect sleep with no cgo.** `CLOCK_REALTIME` advances across
suspend and `CLOCK_UPTIME_RAW` does not, so comparing them identifies a suspend
and, as a bonus, an NTP step.

**Traceroute picks a different hop each run.** BT's path has equal-cost paths,
so rediscovering the ISP hop on every start created a new target row each time
and shattered the history into short, separately-labelled series. The hop is
discovered once and pinned per network.

**macOS 26 emits `responsiveness`, not `dl_responsiveness`.** The per-direction
responsiveness fields documented elsewhere are absent from `networkQuality -c`
output, so reading only those yields a silent zero that looks exactly like a
missing measurement. A fixture captured from a real run guards this.

**speed.cloudflare.com filters by user agent.** It returned 403 to Python's
default agent and 200 to a browser-like one, so any client of it must set a
`User-Agent` and treat 403/429 as probe-unavailable rather than as zero
throughput.

**launchd needs `ProcessType: Standard`.** `Background` opts into aggressive
CPU *and timer* throttling, which is exactly wrong for a process whose job is
to sample on a schedule. Note also that a LaunchAgent runs only in an Aqua
session, so there is genuinely no data while logged out — hence the gap
shading.

## Storage

Percentiles do not compose: an hourly p99 cannot be rebuilt from sixty
per-minute p99 values. Rollups therefore store a fixed-edge log-spaced histogram
per bucket, which merges by element-wise addition. Any percentile can be
computed at any zoom from data that has already been downsampled.

| Tier | Grain | Retention | Footprint |
|---|---|---|---|
| Raw samples | per-ping | 14 days | ~200 MB rolling |
| Rollup | 1 min | 180 days | ~1 GB rolling |
| Rollup | 1 h, 1 d | forever | ~12–15 MB/year |

Samples are keyed by network fingerprint, so home Wi-Fi is never silently
averaged with a phone tether. They are also keyed by load state, so latency
measured while a throughput test saturates the link is kept separate — that
difference is the bufferbloat measurement, and averaging it away would destroy
the most useful number the tool produces.

## Development

```
go test ./...          # includes tests that touch the real network and radio
go test ./... -short   # offline-safe subset
go build ./cmd/netwatch
```

Radio telemetry is the only part that needs cgo. It is isolated behind a build
tag, so `CGO_ENABLED=0 go build` still works and simply reports no Wi-Fi
sample — which is also what an Ethernet connection does.
