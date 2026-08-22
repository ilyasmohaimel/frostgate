package ranking

import (
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
	DownloadMbps    float64
	Priority        int
	HistoryCount    int
	AvailabilityPct float64
}

func rankScore(c Candidate) int {
	// Lower is better.
	if c.Status == "down" {
		return 1000000 + int(c.LossPct)
	}
	score := 0
	if c.Status == "degraded" {
		score += 400000
	}
	// Loss bucket: severe loss
	if c.LossPct > 30 {
		score += 100000
	} else if c.LossPct > 5 {
		score += 50000 + int(c.LossPct*1000)
	} else {
		score += int(c.LossPct * 100)
	}
	// History availability if sufficient samples (>10)
	if c.HistoryCount > 10 {
		// Low availability is bad
		if c.AvailabilityPct < 80 {
			score += 40000
		} else if c.AvailabilityPct < 95 {
			score += 10000
		}
	}
	// Download: major difference
	// We invert: higher download is better, so lower score for higher download
	// Use buckets
	if c.DownloadMbps > 0 {
		// Normalize: 100 Mbps is good
		score -= int(c.DownloadMbps * 10)
	}
	// Median latency: major
	score += int(c.Median.Milliseconds()) * 2
	// Jitter
	score += int(c.Jitter.Milliseconds())
	return score
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
	adjustedScore := func(c Candidate) int {
		// Priority is deliberately bounded to a small tie-break adjustment.
		// It can settle noisy/close results but cannot override a meaningful
		// health, reliability, latency, or throughput gap.
		adjustment := priorityRank[c.Priority] * 100
		if adjustment > 200 {
			adjustment = 200
		}
		return rankScore(c) + adjustment
	}
	sort.SliceStable(out, func(i, j int) bool {
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
