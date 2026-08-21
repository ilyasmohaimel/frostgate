package metrics

import (
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
}

func Compute(samples []Sample) Metrics {
	m := Metrics{Attempted: len(samples)}
	if len(samples) == 0 {
		m.LossPct = 100
		return m
	}
	var lats []time.Duration
	var sum time.Duration
	for _, s := range samples {
		if s.Success {
			m.Succeeded++
			lats = append(lats, s.Latency)
			sum += s.Latency
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
	m.Mean = time.Duration(int64(sum) / int64(len(lats)))
	m.Median = median(lats)

	// Jitter: mean absolute difference between consecutive successful samples in original order
	m.Jitter = jitter(samples)

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
	return (a + b) / 2
}

func jitter(samples []Sample) time.Duration {
	var prev *time.Duration
	var diffs []time.Duration
	var sum time.Duration
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
			sum += d
		}
		v := s.Latency
		prev = &v
	}
	if len(diffs) == 0 {
		return 0
	}
	return time.Duration(int64(sum) / int64(len(diffs)))
}
