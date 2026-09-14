package metrics

import (
	"encoding/binary"
	"errors"
	"math"
)

// Percentiles do not compose: an hourly p99 cannot be derived from sixty
// per-minute p99 values. Storing a histogram per bucket instead of scalar
// percentiles is what lets a rollup answer any percentile at any zoom level
// from data that has already been downsampled, so raw samples need not be kept
// forever.
//
// Buckets are log-spaced so that resolution is proportional to magnitude:
// sub-millisecond detail where a LAN lives, coarse buckets out in timeout
// territory. Edges are fixed for the life of the database — changing them
// would make historical rows unmergeable with new ones.
const (
	NumBuckets = 64

	firstEdgeUS = 250.0 // 250µs: below any real network RTT
	growth      = 1.15
)

var edges = func() [NumBuckets]float64 {
	var e [NumBuckets]float64
	v := firstEdgeUS
	for i := range e {
		e[i] = v
		v *= growth
	}
	return e
}()

// MaxEdgeUS is the upper bound of the last non-overflow bucket.
var MaxEdgeUS = edges[NumBuckets-1]

// Hist counts observations in fixed log-spaced buckets, plus an overflow
// bucket for anything beyond the last edge.
//
// Merging is element-wise addition, which is the entire point: a 1h row is the
// sum of sixty 1m rows, and a 1d row the sum of twenty-four 1h rows.
type Hist struct {
	Counts   [NumBuckets]uint32
	Overflow uint32
}

// bucketFor returns the index whose edge first meets or exceeds v, or -1 for
// overflow.
func bucketFor(us float64) int {
	if us <= edges[0] {
		return 0
	}
	if us > edges[NumBuckets-1] {
		return -1
	}
	// i = ceil(log(us/first) / log(growth)), clamped.
	i := int(math.Ceil(math.Log(us/firstEdgeUS) / math.Log(growth)))
	if i < 0 {
		i = 0
	}
	if i >= NumBuckets {
		return -1
	}
	// Guard against floating-point drift at the boundaries.
	for i > 0 && edges[i-1] >= us {
		i--
	}
	for i < NumBuckets && edges[i] < us {
		i++
	}
	if i >= NumBuckets {
		return -1
	}
	return i
}

func (h *Hist) Observe(us int64) {
	if us < 0 {
		return
	}
	if i := bucketFor(float64(us)); i < 0 {
		h.Overflow++
	} else {
		h.Counts[i]++
	}
}

func (h *Hist) Merge(o *Hist) {
	for i := range h.Counts {
		h.Counts[i] += o.Counts[i]
	}
	h.Overflow += o.Overflow
}

func (h *Hist) Total() uint64 {
	var n uint64
	for _, c := range h.Counts {
		n += uint64(c)
	}
	return n + uint64(h.Overflow)
}

// Quantile returns the upper edge of the bucket containing the q-th quantile.
//
// The result is an upper bound on the true value, never an interpolation:
// claiming more precision than log-spaced buckets carry would be dishonest.
// Returns 0 when empty, and MaxEdgeUS when the quantile falls in overflow.
func (h *Hist) Quantile(q float64) float64 {
	total := h.Total()
	if total == 0 {
		return 0
	}
	if q <= 0 {
		q = 0
	}
	if q > 1 {
		q = 1
	}
	target := uint64(math.Ceil(q * float64(total)))
	if target == 0 {
		target = 1
	}
	var cum uint64
	for i, c := range h.Counts {
		cum += uint64(c)
		if cum >= target {
			return edges[i]
		}
	}
	return MaxEdgeUS
}

// Encode serialises to a fixed-width little-endian blob for storage.
func (h *Hist) Encode() []byte {
	b := make([]byte, 4*(NumBuckets+1))
	for i, c := range h.Counts {
		binary.LittleEndian.PutUint32(b[4*i:], c)
	}
	binary.LittleEndian.PutUint32(b[4*NumBuckets:], h.Overflow)
	return b
}

var ErrBadHist = errors.New("metrics: malformed histogram blob")

func Decode(b []byte) (*Hist, error) {
	if len(b) != 4*(NumBuckets+1) {
		return nil, ErrBadHist
	}
	var h Hist
	for i := range h.Counts {
		h.Counts[i] = binary.LittleEndian.Uint32(b[4*i:])
	}
	h.Overflow = binary.LittleEndian.Uint32(b[4*NumBuckets:])
	return &h, nil
}
