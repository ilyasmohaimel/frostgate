package health

import (
	"time"

	"github.com/ilyasmohaimel/frostgate/internal/metrics"
)

type Status string

const (
	StatusUp       Status = "up"
	StatusDegraded Status = "degraded"
	StatusDown     Status = "down"
)

type Thresholds struct {
	HealthyLatency  time.Duration
	MaxJitter       time.Duration
	MaxLossPct      float64
	DegradedLossPct float64
}

// Reason is a classification cause suitable for user-facing reporting. It is
// derived with the status so tables cannot invent a conflicting explanation.
type Reason string

const (
	ReasonUnavailable Reason = "unavailable"
	ReasonLoss        Reason = "loss"
	ReasonLatency     Reason = "slow"
	ReasonJitter      Reason = "jitter"
)

func DefaultThresholds() Thresholds {
	return Thresholds{
		HealthyLatency: 800 * time.Millisecond,
		MaxJitter:      80 * time.Millisecond,
		MaxLossPct:     0,
		// Partial loss is governed by the caller's MaxLossPct. A route is
		// DOWN for loss only when it has no successful samples.
		DegradedLossPct: 100,
	}
}

func Classify(m metrics.Metrics, th Thresholds) Status {
	status, _ := ClassifyWithReasons(m, th)
	return status
}

// ClassifyWithReasons applies the health policy and returns every observed
// classification cause. Unknown jitter is intentionally not treated as 0 ms.
func ClassifyWithReasons(m metrics.Metrics, th Thresholds) (Status, []Reason) {
	if m.Attempted == 0 {
		return StatusDown, []Reason{ReasonUnavailable}
	}
	if m.Succeeded == 0 {
		return StatusDown, []Reason{ReasonUnavailable}
	}
	loss := m.LossPct

	if loss > th.DegradedLossPct {
		return StatusDown, []Reason{ReasonLoss}
	}
	var reasons []Reason
	if loss > th.MaxLossPct {
		reasons = append(reasons, ReasonLoss)
	}
	if m.Median > th.HealthyLatency {
		reasons = append(reasons, ReasonLatency)
	}
	if m.JitterKnown && m.Jitter > th.MaxJitter {
		reasons = append(reasons, ReasonJitter)
	}
	if len(reasons) > 0 {
		return StatusDegraded, reasons
	}
	return StatusUp, nil
}

func Rank(s Status) int {
	switch s {
	case StatusUp:
		return 0
	case StatusDegraded:
		return 1
	default:
		return 2
	}
}
