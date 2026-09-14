package probe

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// UserAgent identifies our requests.
//
// speed.cloudflare.com returns 403 to unrecognised clients - Python's default
// agent is rejected outright - so this is load-bearing, not cosmetic.
const UserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 netwatch/0.1"

// NetworkQualityTimeout bounds a run. Measured duration is ~45s; this leaves
// room for a slow link without letting a stuck run hold the heavy lease.
const NetworkQualityTimeout = 3 * time.Minute

// MeasuredCostBytes is what one `networkQuality -c` run actually consumed when
// measured with a netstat delta: 246 MB down and 69 MB up.
//
// This is why the probe is scheduled twice a day rather than hourly, and why
// it is gated behind an explicit per-network opt-in.
const MeasuredCostBytes = 315 << 20

// CapacityResult is one throughput measurement.
type CapacityResult struct {
	Engine       string
	DownMbps     float64
	UpMbps       float64
	RPM          int
	DownRPM      int
	UpRPM        int
	Endpoint     string
	BaseRTT      time.Duration
	LoadedRTTP50 time.Duration
	LoadedRTTP95 time.Duration
	BytesDown    int64
	BytesUp      int64
	Duration     time.Duration
	Aborted      bool
	RawJSON      string
}

// networkQualityJSON is the subset of Apple's output we keep.
//
// On macOS 26 the single `responsiveness` field is the one actually emitted;
// the per-direction variants documented elsewhere are absent, so reading only
// those silently yields zero.
type networkQualityJSON struct {
	BaseRTT          float64   `json:"base_rtt"`
	DLThroughput     float64   `json:"dl_throughput"`
	ULThroughput     float64   `json:"ul_throughput"`
	Responsiveness   float64   `json:"responsiveness"`
	DLResponsiveness float64   `json:"dl_responsiveness"`
	ULResponsiveness float64   `json:"ul_responsiveness"`
	DLBytes          int64     `json:"dl_bytes_transferred"`
	ULBytes          int64     `json:"ul_bytes_transferred"`
	TestEndpoint     string    `json:"test_endpoint"`
	LUDForeign       []float64 `json:"lud_foreign_h2_req_resp"`
	LUDSelf          []float64 `json:"lud_self_h2_req_resp"`
}

// RunNetworkQuality shells out to Apple's built-in responsiveness tester.
//
// Writing an equivalent from scratch means handling slow-start exclusion,
// steady-state windowing, multi-flow aggregation and PoP pinning - a week of
// subtle work for a number taken twice a day. More importantly, this tool
// already reports *latency under load*, which is the single most informative
// number about a home connection and the one a plain speed test never shows.
func RunNetworkQuality(ctx context.Context, iface string) (CapacityResult, error) {
	res := CapacityResult{Engine: "networkQuality"}

	before, _ := interfaceBytes(iface)
	start := time.Now()

	ctx, cancel := context.WithTimeout(ctx, NetworkQualityTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "/usr/bin/networkQuality", "-c").Output()
	res.Duration = time.Since(start)

	after, _ := interfaceBytes(iface)
	if before.rx > 0 && after.rx >= before.rx {
		res.BytesDown = after.rx - before.rx
		res.BytesUp = after.tx - before.tx
	}

	if err != nil {
		res.Aborted = true
		return res, fmt.Errorf("networkQuality: %w", err)
	}

	var parsed networkQualityJSON
	if err := json.Unmarshal(out, &parsed); err != nil {
		res.Aborted = true
		return res, fmt.Errorf("parse networkQuality output: %w", err)
	}
	res.RawJSON = string(out)

	// Apple reports throughput in bits per second.
	res.DownMbps = parsed.DLThroughput / 1e6
	res.UpMbps = parsed.ULThroughput / 1e6
	res.BaseRTT = time.Duration(parsed.BaseRTT * float64(time.Millisecond))
	res.Endpoint = parsed.TestEndpoint

	// RPM (round-trips per minute) is Apple's headline responsiveness score:
	// higher is better, and below ~300 is poor. Prefer the combined field,
	// which is the one macOS 26 actually emits.
	res.RPM = int(parsed.Responsiveness)
	res.DownRPM = int(parsed.DLResponsiveness)
	res.UpRPM = int(parsed.ULResponsiveness)
	if res.RPM == 0 && res.DownRPM > 0 {
		res.RPM = res.DownRPM
	}

	// Loaded latency is the payload. The foreign-host series is the one that
	// reflects what a video call would experience while the link is busy.
	loaded := parsed.LUDForeign
	if len(loaded) == 0 {
		loaded = parsed.LUDSelf
	}
	if len(loaded) > 0 {
		res.LoadedRTTP50 = percentileMS(loaded, 0.50)
		res.LoadedRTTP95 = percentileMS(loaded, 0.95)
	}

	if res.BytesDown == 0 {
		// Fall back to the tool's own accounting if the interface counters
		// were unreadable, so the budget is never silently charged zero.
		res.BytesDown = parsed.DLBytes
		res.BytesUp = parsed.ULBytes
	}
	return res, nil
}

// Bufferbloat is how much latency the link adds when saturated.
//
// A connection with excellent headline speed and 30x latency inflation under
// load feels broken during a call; one with modest speed and none feels fine.
func (r CapacityResult) Bufferbloat() time.Duration {
	if r.LoadedRTTP95 <= 0 || r.BaseRTT <= 0 {
		return 0
	}
	return r.LoadedRTTP95 - r.BaseRTT
}

func percentileMS(vals []float64, q float64) time.Duration {
	if len(vals) == 0 {
		return 0
	}
	s := append([]float64(nil), vals...)
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	i := int(q * float64(len(s)-1))
	return time.Duration(s[i] * float64(time.Millisecond))
}

type ifBytes struct{ rx, tx int64 }

// interfaceBytes reads cumulative byte counters so a probe's real cost can be
// charged against the budget, rather than trusting it to self-report.
func interfaceBytes(iface string) (ifBytes, error) {
	out, err := exec.Command("/usr/sbin/netstat", "-ibn").Output()
	if err != nil {
		return ifBytes{}, err
	}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		// Only the <Link#n> row carries the interface totals; the per-address
		// rows repeat them and would double-count.
		if len(f) < 11 || f[0] != iface || !strings.HasPrefix(f[2], "<Link") {
			continue
		}
		rx, err1 := strconv.ParseInt(f[6], 10, 64)
		tx, err2 := strconv.ParseInt(f[9], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		return ifBytes{rx: rx, tx: tx}, nil
	}
	return ifBytes{}, fmt.Errorf("interface %s not found in netstat output", iface)
}
