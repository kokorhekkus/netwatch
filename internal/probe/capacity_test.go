package probe

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// parseCapacity mirrors what RunNetworkQuality does with the command's output,
// so the mapping can be tested without spending 315MB of traffic.
func parseCapacity(t *testing.T, raw []byte) CapacityResult {
	t.Helper()
	var parsed networkQualityJSON
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	var res CapacityResult
	res.DownMbps = parsed.DLThroughput / 1e6
	res.UpMbps = parsed.ULThroughput / 1e6
	res.BaseRTT = time.Duration(parsed.BaseRTT * float64(time.Millisecond))
	res.Endpoint = parsed.TestEndpoint
	res.RPM = int(parsed.Responsiveness)
	if res.RPM == 0 && int(parsed.DLResponsiveness) > 0 {
		res.RPM = int(parsed.DLResponsiveness)
	}
	loaded := parsed.LUDForeign
	if len(loaded) == 0 {
		loaded = parsed.LUDSelf
	}
	if len(loaded) > 0 {
		res.LoadedRTTP50 = percentileMS(loaded, 0.50)
		res.LoadedRTTP95 = percentileMS(loaded, 0.95)
	}
	return res
}

// The fixture is real output captured from this machine.
//
// The regression it guards: macOS 26 emits a single `responsiveness` field,
// not the per-direction `dl_responsiveness`/`ul_responsiveness` documented
// elsewhere. Reading only the latter yielded a silent zero, which looks like
// a perfectly plausible "no data" rather than a parsing mistake.
func TestParseNetworkQualityOutput(t *testing.T) {
	raw, err := os.ReadFile("testdata/networkquality.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	res := parseCapacity(t, raw)

	if got := res.DownMbps; got < 70 || got > 73 {
		t.Errorf("DownMbps = %.1f, want ~71.5", got)
	}
	if got := res.UpMbps; got < 8 || got > 9 {
		t.Errorf("UpMbps = %.1f, want ~8.2", got)
	}
	if res.RPM == 0 {
		t.Error("RPM is zero — the responsiveness field was not read")
	}
	if res.RPM < 35 || res.RPM > 45 {
		t.Errorf("RPM = %d, want ~39", res.RPM)
	}
	if res.BaseRTT < 15*time.Millisecond || res.BaseRTT > 25*time.Millisecond {
		t.Errorf("BaseRTT = %v, want ~20ms", res.BaseRTT)
	}
	if res.Endpoint == "" {
		t.Error("test endpoint not captured")
	}
}

func TestBufferbloatDerivation(t *testing.T) {
	raw, err := os.ReadFile("testdata/networkquality.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	res := parseCapacity(t, raw)

	if res.LoadedRTTP95 <= res.BaseRTT {
		t.Fatalf("loaded p95 %v not above idle %v", res.LoadedRTTP95, res.BaseRTT)
	}
	bloat := res.Bufferbloat()
	if bloat != res.LoadedRTTP95-res.BaseRTT {
		t.Errorf("Bufferbloat() = %v, want %v", bloat, res.LoadedRTTP95-res.BaseRTT)
	}

	// Missing inputs must yield zero rather than a nonsensical negative.
	if (CapacityResult{BaseRTT: 20 * time.Millisecond}).Bufferbloat() != 0 {
		t.Error("Bufferbloat with no loaded measurement should be zero")
	}
	if (CapacityResult{LoadedRTTP95: time.Second}).Bufferbloat() != 0 {
		t.Error("Bufferbloat with no baseline should be zero")
	}
}

func TestPercentileMS(t *testing.T) {
	vals := make([]float64, 100)
	for i := range vals {
		vals[i] = float64(i + 1) // 1..100 ms
	}
	for _, tc := range []struct {
		q    float64
		want time.Duration
	}{
		{0.50, 50 * time.Millisecond},
		{0.95, 95 * time.Millisecond},
	} {
		got := percentileMS(vals, tc.q)
		// Index arithmetic can be off by one either way; anything further is
		// a real error.
		diff := got - tc.want
		if diff < -2*time.Millisecond || diff > 2*time.Millisecond {
			t.Errorf("percentileMS(%v) = %v, want ~%v", tc.q, got, tc.want)
		}
	}
	if percentileMS(nil, 0.5) != 0 {
		t.Error("percentileMS of nothing should be zero")
	}
}

func TestBudgetStopsOverspend(t *testing.T) {
	b := NewBudget(1000)
	if !b.Allows(600) {
		t.Fatal("first charge should be allowed")
	}
	b.Charge(600)
	if !b.Allows(400) {
		t.Error("a charge that exactly fits should be allowed")
	}
	if b.Allows(401) {
		t.Error("budget allowed an overspend")
	}
	b.Charge(400)
	if b.Allows(1) {
		t.Error("budget allowed spending past the limit")
	}
}

func TestBudgetRestore(t *testing.T) {
	b := NewBudget(1000)
	month, _, _ := b.State()

	b.Restore(month, 900)
	if b.Allows(200) {
		t.Error("restored usage was ignored — the budget would reset on every restart")
	}

	// A stale month must not carry over into the new one.
	b2 := NewBudget(1000)
	b2.Restore("1999-01", 999)
	if !b2.Allows(900) {
		t.Error("usage from a previous month was applied to this one")
	}
}
