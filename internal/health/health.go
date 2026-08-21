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

func DefaultThresholds() Thresholds {
	return Thresholds{
		HealthyLatency:  800 * time.Millisecond,
		MaxJitter:       80 * time.Millisecond,
		MaxLossPct:      0,
		DegradedLossPct: 50,
	}
}

func Classify(m metrics.Metrics, th Thresholds) Status {
	if m.Attempted == 0 {
		return StatusDown
	}
	if m.Succeeded == 0 {
		return StatusDown
	}
	loss := m.LossPct

	if loss > th.DegradedLossPct {
		return StatusDown
	}
	if loss > th.MaxLossPct {
		return StatusDegraded
	}
	if m.Median > th.HealthyLatency {
		return StatusDegraded
	}
	if m.Jitter > th.MaxJitter {
		return StatusDegraded
	}
	return StatusUp
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
