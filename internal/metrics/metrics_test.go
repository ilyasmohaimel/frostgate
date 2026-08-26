package metrics

import (
	"testing"
	"time"
)

func TestCompute_AllSuccess(t *testing.T) {
	samples := []Sample{
		{Success: true, Latency: 100 * time.Millisecond},
		{Success: true, Latency: 112 * time.Millisecond},
		{Success: true, Latency: 105 * time.Millisecond},
		{Success: true, Latency: 125 * time.Millisecond},
	}
	m := Compute(samples)
	if m.Attempted != 4 || m.Succeeded != 4 || m.Failed != 0 {
		t.Fatalf("attempted=%d succeeded=%d failed=%d", m.Attempted, m.Succeeded, m.Failed)
	}
	if m.LossPct != 0 {
		t.Errorf("loss=%f want 0", m.LossPct)
	}
	if m.Min != 100*time.Millisecond {
		t.Errorf("min=%v want 100ms", m.Min)
	}
	if m.Max != 125*time.Millisecond {
		t.Errorf("max=%v want 125ms", m.Max)
	}
	if m.Median != 108*time.Millisecond && m.Median != 108*time.Millisecond+500*time.Microsecond {
		// (105+112)/2 = 108.5ms
		t.Errorf("median=%v want ~108ms", m.Median)
	}
	// jitter: mean abs diff consecutive in original order: 12,7,20 => avg 13ms
	if m.Jitter != 13*time.Millisecond {
		t.Errorf("jitter=%v want 13ms", m.Jitter)
	}
	if !m.JitterKnown {
		t.Error("multiple successful samples must provide jitter evidence")
	}
}

func TestCompute_AllFail(t *testing.T) {
	samples := []Sample{
		{Success: false}, {Success: false},
	}
	m := Compute(samples)
	if m.Succeeded != 0 || m.Failed != 2 {
		t.Fatalf("got %+v", m)
	}
	if m.LossPct != 100 {
		t.Errorf("loss=%f want 100", m.LossPct)
	}
	if m.Min != 0 || m.Median != 0 || m.Jitter != 0 {
		t.Errorf("zero success should have zero latencies: %+v", m)
	}
}

func TestCompute_Partial(t *testing.T) {
	samples := []Sample{
		{Success: true, Latency: 50 * time.Millisecond},
		{Success: false},
		{Success: true, Latency: 150 * time.Millisecond},
		{Success: false},
	}
	m := Compute(samples)
	if m.Succeeded != 2 || m.Failed != 2 {
		t.Fatalf("got %+v", m)
	}
	if m.LossPct != 50 {
		t.Errorf("loss=%v want 50", m.LossPct)
	}
	if m.Min != 50*time.Millisecond || m.Max != 150*time.Millisecond {
		t.Errorf("min/max wrong %+v", m)
	}
	if m.Median != 100*time.Millisecond {
		t.Errorf("median=%v want 100ms", m.Median)
	}
	// jitter: only two successes diff 100ms => jitter 100ms
	if m.Jitter != 100*time.Millisecond {
		t.Errorf("jitter=%v want 100ms", m.Jitter)
	}
}

func TestMedian(t *testing.T) {
	tests := []struct {
		input []time.Duration
		want  time.Duration
	}{
		{[]time.Duration{10 * time.Millisecond}, 10 * time.Millisecond},
		{[]time.Duration{10 * time.Millisecond, 20 * time.Millisecond}, 15 * time.Millisecond},
		{[]time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond}, 20 * time.Millisecond},
		{[]time.Duration{30 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond}, 25 * time.Millisecond},
	}
	for _, tt := range tests {
		// Compute sorts internally, but median expects sorted
		// we test median via Compute with sorted input
		// directly test median function by sorting
		s := make([]time.Duration, len(tt.input))
		copy(s, tt.input)
		// sort for median expectation if using Compute path
		// Instead call Compute and check median
		samples := make([]Sample, len(s))
		for i, d := range s {
			samples[i] = Sample{Success: true, Latency: d}
		}
		m := Compute(samples)
		if m.Median != tt.want {
			t.Errorf("input %v median=%v want %v", tt.input, m.Median, tt.want)
		}
	}
}

func TestJitter_SingleSuccess(t *testing.T) {
	samples := []Sample{{Success: true, Latency: 100 * time.Millisecond}}
	m := Compute(samples)
	if m.Jitter != 0 {
		t.Errorf("single sample jitter should be 0, got %v", m.Jitter)
	}
	if m.JitterKnown {
		t.Error("one successful sample must have unknown jitter")
	}
}

func TestJitter_ZeroSuccess(t *testing.T) {
	samples := []Sample{{Success: false}, {Success: false}}
	m := Compute(samples)
	if m.Jitter != 0 {
		t.Errorf("zero success jitter should be 0, got %v", m.Jitter)
	}
	if m.JitterKnown {
		t.Error("zero successful samples must have unknown jitter")
	}
}

func TestJitterMeasuredZeroIsKnown(t *testing.T) {
	m := Compute([]Sample{
		{Success: true, Latency: 100 * time.Millisecond},
		{Success: true, Latency: 100 * time.Millisecond},
	})
	if !m.JitterKnown || m.Jitter != 0 {
		t.Fatalf("equal successful samples must prove 0ms jitter: %+v", m)
	}
}

func TestComputeMeanAndJitterKeepIntegerRemainders(t *testing.T) {
	m := Compute([]Sample{
		{Success: true, Latency: 1 * time.Nanosecond},
		{Success: true, Latency: 2 * time.Nanosecond},
		{Success: true, Latency: 3 * time.Nanosecond},
	})
	if m.Mean != 2*time.Nanosecond {
		t.Fatalf("mean=%s, want 2ns", m.Mean)
	}
	if !m.JitterKnown || m.Jitter != 1*time.Nanosecond {
		t.Fatalf("jitter=%s known=%t, want 1ns known", m.Jitter, m.JitterKnown)
	}
}
