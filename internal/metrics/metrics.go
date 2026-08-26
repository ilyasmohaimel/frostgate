package metrics

import (
	"math/bits"
	"sort"
	"time"
)

type Sample struct {
	Success     bool
	Latency     time.Duration
	Err         error
	ErrCategory string
}

type Metrics struct {
	Attempted int
	Succeeded int
	Failed    int
	LossPct   float64
	Min       time.Duration
	Median    time.Duration
	Max       time.Duration
	Mean      time.Duration
	Jitter    time.Duration
	// JitterKnown distinguishes a measured 0 ms jitter from a route with
	// fewer than two successful samples, for which jitter cannot be computed.
	JitterKnown bool
}

func Compute(samples []Sample) Metrics {
	m := Metrics{Attempted: len(samples)}
	if len(samples) == 0 {
		m.LossPct = 100
		return m
	}
	var lats []time.Duration
	for _, s := range samples {
		if s.Success {
			m.Succeeded++
			lats = append(lats, s.Latency)
		} else {
			m.Failed++
		}
	}
	m.LossPct = float64(m.Failed) / float64(m.Attempted) * 100

	if len(lats) == 0 {
		return m
	}
	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	m.Min = lats[0]
	m.Max = lats[len(lats)-1]
	m.Median = median(lats)
	m.Mean = meanDurations(lats)

	// Jitter is the mean absolute difference between successful samples in
	// their original order. A single successful sample has no delta, so it is
	// unknown rather than a perfect 0 ms measurement.
	m.Jitter, m.JitterKnown = jitter(samples)

	return m
}

func median(sorted []time.Duration) time.Duration {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	a := sorted[n/2-1]
	b := sorted[n/2]
	return a + (b-a)/2
}

func jitter(samples []Sample) (time.Duration, bool) {
	var prev *time.Duration
	var diffs []time.Duration
	for _, s := range samples {
		if !s.Success {
			continue
		}
		if prev != nil {
			d := s.Latency - *prev
			if d < 0 {
				d = -d
			}
			diffs = append(diffs, d)
		}
		v := s.Latency
		prev = &v
	}
	if len(diffs) == 0 {
		return 0, false
	}
	return meanDurations(diffs), true
}

// meanDurations accumulates nanoseconds as an unsigned 128-bit total. It
// keeps sub-nanosecond quotient remainders until the final division, unlike
// incremental duration averaging which loses precision on each sample.
func meanDurations(values []time.Duration) time.Duration {
	if len(values) == 0 {
		return 0
	}
	var hi, lo uint64
	for _, value := range values {
		nextLo, carryLo := bits.Add64(lo, uint64(value), 0)
		lo = nextLo
		hi, _ = bits.Add64(hi, 0, carryLo)
	}
	quotient, _ := bits.Div64(hi, lo, uint64(len(values)))
	return time.Duration(quotient)
}
