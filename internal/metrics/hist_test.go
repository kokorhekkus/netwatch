package metrics

import (
	"math"
	"testing"
)

func TestQuantileBracketsTrueValue(t *testing.T) {
	var h Hist
	// 1..1000 ms, one sample per millisecond.
	for ms := 1; ms <= 1000; ms++ {
		h.Observe(int64(ms) * 1000)
	}
	if got := h.Total(); got != 1000 {
		t.Fatalf("Total() = %d, want 1000", got)
	}

	for _, tc := range []struct{ q, want float64 }{
		{0.50, 500_000},
		{0.90, 900_000},
		{0.99, 990_000},
	} {
		got := h.Quantile(tc.q)
		// The bucket upper edge must be >= the true value, and within one
		// bucket width (15%) above it.
		if got < tc.want {
			t.Errorf("Quantile(%v) = %.0f, below true value %.0f", tc.q, got, tc.want)
		}
		if got > tc.want*growth {
			t.Errorf("Quantile(%v) = %.0f, more than one bucket above %.0f", tc.q, got, tc.want)
		}
	}
}

// The property the whole retention design rests on: a coarse-grain row built by
// merging fine-grain rows must equal one built from the raw samples directly.
func TestMergeEqualsDirectObservation(t *testing.T) {
	var direct Hist
	var merged Hist

	for minute := 0; minute < 60; minute++ {
		var perMinute Hist
		for i := 0; i < 100; i++ {
			us := int64(1000 + minute*137 + i*11)
			perMinute.Observe(us)
			direct.Observe(us)
		}
		// Round-trip through storage on the way, since that is what the
		// rollup job actually does.
		blob := perMinute.Encode()
		decoded, err := Decode(blob)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		merged.Merge(decoded)
	}

	if direct.Total() != merged.Total() {
		t.Fatalf("Total: direct=%d merged=%d", direct.Total(), merged.Total())
	}
	if direct.Counts != merged.Counts || direct.Overflow != merged.Overflow {
		t.Fatal("merged histogram differs from directly observed histogram")
	}
	for _, q := range []float64{0.5, 0.9, 0.99, 1.0} {
		if direct.Quantile(q) != merged.Quantile(q) {
			t.Errorf("Quantile(%v): direct=%.0f merged=%.0f", q,
				direct.Quantile(q), merged.Quantile(q))
		}
	}
}

func TestOverflowAndEmpty(t *testing.T) {
	var h Hist
	if got := h.Quantile(0.5); got != 0 {
		t.Errorf("empty Quantile = %v, want 0", got)
	}

	h.Observe(int64(MaxEdgeUS) * 10) // far beyond the last edge
	if h.Overflow != 1 {
		t.Errorf("Overflow = %d, want 1", h.Overflow)
	}
	if got := h.Quantile(0.5); got != MaxEdgeUS {
		t.Errorf("Quantile with only overflow = %v, want %v", got, MaxEdgeUS)
	}
}

func TestEncodeRoundTrip(t *testing.T) {
	var h Hist
	for i := 0; i < 500; i++ {
		h.Observe(int64(i * 997))
	}
	got, err := Decode(h.Encode())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Counts != h.Counts || got.Overflow != h.Overflow {
		t.Error("round trip changed the histogram")
	}
	if _, err := Decode([]byte{1, 2, 3}); err == nil {
		t.Error("Decode accepted a short blob")
	}
}

func TestBucketMonotonic(t *testing.T) {
	prev := -1
	for us := int64(100); us < int64(MaxEdgeUS); us = us * 103 / 100 {
		i := bucketFor(float64(us))
		if i < prev {
			t.Fatalf("bucketFor(%d) = %d went backwards from %d", us, i, prev)
		}
		if i >= 0 && float64(us) > edges[i] {
			t.Fatalf("bucketFor(%d) = %d but edge is %.0f", us, i, edges[i])
		}
		prev = i
	}
}

func TestQuantileAgainstNaiveSort(t *testing.T) {
	var h Hist
	var raw []float64
	// Deterministic pseudo-random spread across three orders of magnitude.
	x := uint64(12345)
	for i := 0; i < 5000; i++ {
		x = x*6364136223846793005 + 1442695040888963407
		us := int64(300 + (x>>33)%400_000)
		h.Observe(us)
		raw = append(raw, float64(us))
	}
	sortFloats(raw)

	for _, q := range []float64{0.5, 0.75, 0.9, 0.99} {
		idx := int(math.Ceil(q*float64(len(raw)))) - 1
		true_ := raw[idx]
		got := h.Quantile(q)
		if got < true_ {
			t.Errorf("Quantile(%v)=%.0f below true %.0f", q, got, true_)
		}
		if got > true_*growth*1.01 {
			t.Errorf("Quantile(%v)=%.0f too far above true %.0f", q, got, true_)
		}
	}
}

func sortFloats(a []float64) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}
