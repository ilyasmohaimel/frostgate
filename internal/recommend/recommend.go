package recommend

import (
	"fmt"
	"sort"
	"time"

	"github.com/ilyasmohaimel/frostgate/internal/history"
)

type Input struct {
	Name         string
	Status       string // up, degraded, down
	Loss         float64
	Median       time.Duration
	Jitter       time.Duration
	Download     float64
	Priority     int
	Availability float64 // from history 0-100
}

type Result struct {
	BestOverall      Input
	Reasons          []string
	Preferred        *Input
	Fastest          Input
	MostStable       Input
	BestFallback     *Input
	Avoid            []Input
	Confidence       string
	ConfidenceDetail string
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
	// If no healthy, best overall is least bad down?
	if len(healthy) == 0 {
		// pick least loss down
		sort.Slice(inputs, func(i, j int) bool {
			if inputs[i].Loss != inputs[j].Loss {
				return inputs[i].Loss < inputs[j].Loss
			}
			return inputs[i].Median < inputs[j].Median
		})
		return Result{
			BestOverall: inputs[0],
			Reasons:     []string{"no healthy route, least loss"},
			Confidence:  confidence(historyCount),
		}
	}

	// Score healthy: lower loss is most important, then download, then median, then jitter, then priority as tie-break
	// Use explainable rules, not just numeric score
	scored := make([]Input, len(healthy))
	copy(scored, healthy)
	sort.Slice(scored, func(i, j int) bool {
		// 1. loss bucket: 0% vs >0% vs >5% etc
		// For simplicity, lower loss wins
		if scored[i].Loss != scored[j].Loss {
			// If loss difference >5%, lower wins regardless of speed
			if (scored[i].Loss == 0) != (scored[j].Loss == 0) {
				return scored[i].Loss < scored[j].Loss
			}
			// Both have some loss or both zero, compare loss
			if scored[i].Loss < scored[j].Loss-5 {
				return true
			}
			if scored[j].Loss < scored[i].Loss-5 {
				return false
			}
		}
		// 2. download: if difference >20 Mbps, higher wins
		if scored[i].Download > 0 && scored[j].Download > 0 {
			if scored[i].Download > scored[j].Download+20 {
				return true
			}
			if scored[j].Download > scored[i].Download+20 {
				return false
			}
		} else if scored[i].Download > 0 && scored[j].Download == 0 {
			return true
		} else if scored[j].Download > 0 && scored[i].Download == 0 {
			return false
		}
		// 3. median: if difference >30ms, lower wins
		if scored[i].Median < scored[j].Median-30*time.Millisecond {
			return true
		}
		if scored[j].Median < scored[i].Median-30*time.Millisecond {
			return false
		}
		// 4. jitter: lower wins if median close
		if scored[i].Jitter != scored[j].Jitter {
			return scored[i].Jitter < scored[j].Jitter
		}
		// 5. priority as tie-break: lower priority value wins if close
		return scored[i].Priority < scored[j].Priority
	})

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
	if best.Jitter < 20*time.Millisecond {
		reasons = append(reasons, "lowest jitter")
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

	return Result{
		BestOverall:      best,
		Reasons:          reasons,
		Preferred:        preferred,
		Fastest:          fastest,
		MostStable:       mostStable,
		BestFallback:     fallback,
		Avoid:            avoid,
		Confidence:       confidence(historyCount),
		ConfidenceDetail: fmt.Sprintf("Based on %d measurements", historyCount),
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
