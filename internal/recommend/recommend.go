package recommend

import (
	"fmt"
	"time"

	"github.com/ilyasmohaimel/frostgate/internal/history"
	"github.com/ilyasmohaimel/frostgate/internal/ranking"
)

type Input struct {
	Name         string        `json:"name"`
	Status       string        `json:"status"` // up, degraded, down
	Loss         float64       `json:"loss_pct"`
	Median       time.Duration `json:"median_ns"`
	Jitter       time.Duration `json:"jitter_ns"`
	Download     float64       `json:"download_mbps,omitempty"`
	Priority     int           `json:"priority"`
	Availability float64       `json:"availability_pct,omitempty"` // from history 0-100
	HistoryCount int           `json:"history_count,omitempty"`    // number of historical samples for THIS route
}

type Result struct {
	BestOverall      Input    `json:"best_overall"`
	Reasons          []string `json:"reasons"`
	Preferred        *Input   `json:"preferred,omitempty"`
	Fastest          Input    `json:"fastest"`
	MostStable       Input    `json:"most_stable"`
	BestFallback     *Input   `json:"best_fallback,omitempty"`
	Avoid            []Input  `json:"avoid"`
	Confidence       string   `json:"confidence"`
	ConfidenceDetail string   `json:"confidence_detail"`
}

func Recommend(inputs []Input, historyCount int) Result {
	if len(inputs) == 0 {
		return Result{}
	}
	// Filter healthy
	var healthy []Input
	for _, in := range inputs {
		if in.Status != "down" {
			healthy = append(healthy, in)
		}
	}
	// A down route is never a recommendation, even when every route is down.
	if len(healthy) == 0 {
		return Result{
			Avoid:            append([]Input(nil), inputs...),
			Confidence:       "low",
			ConfidenceDetail: "No usable route measurements",
		}
	}

	// Order healthy candidates using the shared ranking policy (single
	// source of truth for ordering across probe/benchmark/recommend/compare).
	cands := make([]ranking.Candidate, len(healthy))
	for i, in := range healthy {
		cands[i] = ranking.Candidate{
			Name:            in.Name,
			IsProxy:         true,
			Status:          in.Status,
			LossPct:         in.Loss,
			Median:          in.Median,
			Jitter:          in.Jitter,
			DownloadMbps:    in.Download,
			Priority:        in.Priority,
			AvailabilityPct: in.Availability,
			HistoryCount:    in.HistoryCount,
		}
	}
	sorted := ranking.Sort(cands)
	scored := make([]Input, len(sorted))
	for i, c := range sorted {
		scored[i] = Input{
			Name:         c.Name,
			Status:       c.Status,
			Loss:         c.LossPct,
			Median:       c.Median,
			Jitter:       c.Jitter,
			Download:     c.DownloadMbps,
			Priority:     c.Priority,
			Availability: c.AvailabilityPct,
			HistoryCount: c.HistoryCount,
		}
	}

	best := scored[0]
	reasons := []string{}
	if best.Loss == 0 {
		reasons = append(reasons, "0% loss")
	} else {
		reasons = append(reasons, fmt.Sprintf("%.0f%% loss", best.Loss))
	}
	reasons = append(reasons, fmt.Sprintf("%dms median", best.Median.Milliseconds()))
	if best.Download > 0 {
		reasons = append(reasons, fmt.Sprintf("%.0f Mbps", best.Download))
	}
	if best.Jitter > 0 {
		// Jitter is meaningful only when every usable candidate has enough
		// sample evidence to provide a non-zero aggregate. Otherwise a zero is
		// ambiguous (often one successful sample), so avoid a superlative.
		lowest := true
		for _, in := range healthy {
			if in.Name == best.Name {
				continue
			}
			if in.Jitter <= 0 || in.Jitter < best.Jitter {
				lowest = false
				break
			}
		}
		if lowest {
			reasons = append(reasons, "lowest jitter")
		} else {
			reasons = append(reasons, fmt.Sprintf("%dms jitter", best.Jitter.Milliseconds()))
		}
	}
	if best.Availability > 90 {
		reasons = append(reasons, fmt.Sprintf("%.1f%% availability over history", best.Availability))
	}

	// Preferred: lowest priority value (most preferred) among healthy
	var preferred *Input
	minPrio := 1 << 30
	for _, in := range healthy {
		if in.Priority < minPrio {
			minPrio = in.Priority
			tmp := in
			preferred = &tmp
		}
	}
	if preferred != nil && preferred.Name == best.Name {
		preferred = nil // best already is preferred
	}

	// Fastest: lowest median
	fastest := scored[0]
	for _, in := range healthy {
		if in.Median < fastest.Median {
			fastest = in
		}
	}
	// Most stable: lowest jitter + low loss
	mostStable := scored[0]
	for _, in := range healthy {
		if in.Jitter < mostStable.Jitter && in.Loss <= mostStable.Loss {
			mostStable = in
		}
	}
	// Best fallback: second best
	var fallback *Input
	if len(scored) > 1 {
		tmp := scored[1]
		fallback = &tmp
	}
	// Avoid: any down or >30% loss
	var avoid []Input
	for _, in := range inputs {
		if in.Status == "down" || in.Loss > 30 {
			avoid = append(avoid, in)
		}
	}

	// Confidence derives from the recommended route's OWN history evidence,
	// not from the global history row count.
	confCount := best.HistoryCount
	if confCount == 0 {
		confCount = historyCount
	}
	return Result{
		BestOverall:      best,
		Reasons:          reasons,
		Preferred:        preferred,
		Fastest:          fastest,
		MostStable:       mostStable,
		BestFallback:     fallback,
		Avoid:            avoid,
		Confidence:       confidence(confCount),
		ConfidenceDetail: fmt.Sprintf("Based on %d measurements for %s", confCount, best.Name),
	}
}

func confidence(count int) string {
	switch {
	case count >= 50:
		return "high"
	case count >= 10:
		return "medium"
	default:
		return "low"
	}
}

// Helper to convert history.Summary to Recommend Input
func FromSummary(s history.Summary, status string, download float64, priority int) Input {
	return Input{
		Name:         s.Route,
		Status:       status,
		Loss:         100 - s.Availability, // approximate
		Median:       s.Median,
		Jitter:       s.Jitter,
		Download:     download,
		Availability: s.Availability,
		Priority:     priority,
	}
}
