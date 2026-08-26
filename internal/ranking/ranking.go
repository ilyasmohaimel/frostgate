package ranking

import (
	"math"
	"sort"
	"time"
)

type Candidate struct {
	Name            string
	IsProxy         bool
	Status          string // up, degraded, down
	LossPct         float64
	Median          time.Duration
	Jitter          time.Duration
	JitterKnown     bool
	DownloadMbps    float64
	Priority        int
	HistoryCount    int
	AvailabilityPct float64
}

func rankScore(c Candidate) int64 {
	// Lower is better.
	if c.Status == "down" {
		return saturatingAdd(1000000, scoreFloat(c.LossPct))
	}
	var score int64
	if c.Status == "degraded" {
		score = saturatingAdd(score, 400000)
	}
	// Loss bucket: severe loss
	if c.LossPct > 30 {
		score = saturatingAdd(score, 100000)
	} else if c.LossPct > 5 {
		score = saturatingAdd(score, saturatingAdd(50000, scoreFloat(c.LossPct*1000)))
	} else {
		score = saturatingAdd(score, scoreFloat(c.LossPct*100))
	}
	// History availability if sufficient samples (>10)
	if c.HistoryCount > 10 {
		// Low availability is bad
		if c.AvailabilityPct < 80 {
			score = saturatingAdd(score, 40000)
		} else if c.AvailabilityPct < 95 {
			score = saturatingAdd(score, 10000)
		}
	}
	// Download: major difference
	// We invert: higher download is better, so lower score for higher download
	// Use buckets
	if c.DownloadMbps > 0 {
		// Normalize: 100 Mbps is good
		score = saturatingAdd(score, -scoreFloat(c.DownloadMbps*10))
	}
	// Median latency: major
	score = saturatingAdd(score, saturatingAdd(c.Median.Milliseconds(), c.Median.Milliseconds()))
	// Unknown jitter is neither rewarded as perfect nor penalized. Known
	// jitter only settles otherwise close route measurements.
	if c.JitterKnown {
		score = saturatingAdd(score, c.Jitter.Milliseconds())
	}
	return score
}

const (
	maxScore int64 = 1<<63 - 1
	minScore int64 = -1 << 63
)

func saturatingAdd(left, right int64) int64 {
	if right > 0 && left > maxScore-right {
		return maxScore
	}
	if right < 0 && left < minScore-right {
		return minScore
	}
	return left + right
}

func scoreFloat(value float64) int64 {
	if math.IsNaN(value) {
		return 0
	}
	if value >= float64(maxScore) || math.IsInf(value, 1) {
		return maxScore
	}
	if value <= float64(minScore) || math.IsInf(value, -1) {
		return minScore
	}
	return int64(value)
}

func statusTier(status string) int {
	switch status {
	case "up":
		return 0
	case "degraded":
		return 1
	case "down":
		return 2
	default:
		return 3
	}
}

func Sort(cands []Candidate) []Candidate {
	out := make([]Candidate, len(cands))
	copy(out, cands)
	priorities := make([]int, 0, len(out))
	seenPriority := map[int]bool{}
	for _, c := range out {
		if !seenPriority[c.Priority] {
			seenPriority[c.Priority] = true
			priorities = append(priorities, c.Priority)
		}
	}
	sort.Ints(priorities)
	priorityRank := make(map[int]int, len(priorities))
	for i, p := range priorities {
		priorityRank[p] = i
	}
	adjustedScore := func(c Candidate) int64 {
		// Priority is deliberately bounded to a small tie-break adjustment.
		// It can settle noisy/close results but cannot override a meaningful
		// health, reliability, latency, or throughput gap.
		// The adjustment is capped before multiplication, so an adversarially
		// large candidate set cannot overflow an int64 while computing a tie
		// breaker.
		rank := priorityRank[c.Priority]
		adjustment := int64(200)
		if rank < 2 {
			adjustment = int64(rank) * 100
		}
		return saturatingAdd(rankScore(c), adjustment)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ti, tj := statusTier(out[i].Status), statusTier(out[j].Status)
		if ti != tj {
			// Health is a strict, non-negotiable ordering tier. No finite
			// performance/history score can cross it.
			return ti < tj
		}
		si := adjustedScore(out[i])
		sj := adjustedScore(out[j])
		if si == sj {
			rawI := rankScore(out[i])
			rawJ := rankScore(out[j])
			if rawI != rawJ {
				return rawI < rawJ
			}
			return out[i].Name < out[j].Name
		}
		return si < sj
	})
	return out
}

func Best(cands []Candidate) *Candidate {
	if len(cands) == 0 {
		return nil
	}
	sorted := Sort(cands)
	// Exclude direct from best proxy: filter IsProxy
	var proxies []Candidate
	for _, c := range sorted {
		if c.IsProxy {
			proxies = append(proxies, c)
		}
	}
	if len(proxies) > 0 {
		for _, p := range proxies {
			if p.Status != "down" {
				return &p
			}
		}
		return nil
	}
	for _, c := range sorted {
		if c.Status != "down" {
			return &c
		}
	}
	return nil
}
