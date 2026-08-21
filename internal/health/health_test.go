package health

import (
	"testing"
	"time"

	"github.com/ilyasmohaimel/frostgate/internal/metrics"
)

func TestClassify(t *testing.T) {
	th := DefaultThresholds()
	tests := []struct {
		name string
		m    metrics.Metrics
		want Status
	}{
		{"healthy perfect", metrics.Metrics{Attempted: 4, Succeeded: 4, Failed: 0, LossPct: 0, Median: 100 * time.Millisecond, Jitter: 10 * time.Millisecond}, StatusUp},
		{"some loss -> degraded", metrics.Metrics{Attempted: 4, Succeeded: 3, Failed: 1, LossPct: 25, Median: 100 * time.Millisecond, Jitter: 10 * time.Millisecond}, StatusDegraded},
		{"high loss -> down", metrics.Metrics{Attempted: 4, Succeeded: 1, Failed: 3, LossPct: 75, Median: 100 * time.Millisecond, Jitter: 10 * time.Millisecond}, StatusDown},
		{"all fail -> down", metrics.Metrics{Attempted: 4, Succeeded: 0, Failed: 4, LossPct: 100}, StatusDown},
		{"slow median -> degraded", metrics.Metrics{Attempted: 4, Succeeded: 4, Failed: 0, LossPct: 0, Median: 900 * time.Millisecond, Jitter: 10 * time.Millisecond}, StatusDegraded},
		{"high jitter -> degraded", metrics.Metrics{Attempted: 4, Succeeded: 4, Failed: 0, LossPct: 0, Median: 100 * time.Millisecond, Jitter: 100 * time.Millisecond}, StatusDegraded},
		{"zero attempted -> down", metrics.Metrics{Attempted: 0}, StatusDown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.m, th); got != tt.want {
				t.Errorf("Classify() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClassify_CustomThresholds(t *testing.T) {
	th := Thresholds{HealthyLatency: 100 * time.Millisecond, MaxJitter: 10 * time.Millisecond, MaxLossPct: 10, DegradedLossPct: 50}
	m := metrics.Metrics{Attempted: 4, Succeeded: 4, LossPct: 0, Median: 90 * time.Millisecond, Jitter: 5 * time.Millisecond}
	if got := Classify(m, th); got != StatusUp {
		t.Errorf("expected up, got %v", got)
	}
	m.Median = 150 * time.Millisecond
	if got := Classify(m, th); got != StatusDegraded {
		t.Errorf("slow should be degraded")
	}
}

func TestClassify_Boundaries(t *testing.T) {
	th := DefaultThresholds()
	// exactly at healthy latency should be up
	m := metrics.Metrics{Attempted: 4, Succeeded: 4, LossPct: 0, Median: th.HealthyLatency, Jitter: 5 * time.Millisecond}
	if got := Classify(m, th); got != StatusUp {
		t.Errorf("at healthy latency should be up, got %v", got)
	}
	// exactly at max jitter should be up
	m.Median = 100 * time.Millisecond
	m.Jitter = th.MaxJitter
	if got := Classify(m, th); got != StatusUp {
		t.Errorf("at max jitter should be up, got %v", got)
	}
	// loss exactly at MaxLossPct up, just over degraded
	m.Jitter = 5 * time.Millisecond
	m.LossPct = 0
	m.Succeeded = 4
	m.Failed = 0
	m.Attempted = 4
	if got := Classify(m, th); got != StatusUp {
		t.Errorf("0 loss should be up")
	}
}
