package recommend

import (
	"testing"
	"time"
)

func TestUnstableFasterLoses(t *testing.T) {
	inputs := []Input{
		{Name: "A", Status: "up", Loss: 20, Median: 120 * time.Millisecond, Jitter: 10 * time.Millisecond, Priority: 10},
		{Name: "B", Status: "up", Loss: 0, Median: 160 * time.Millisecond, Jitter: 10 * time.Millisecond, Priority: 10},
	}
	res := Recommend(inputs, 10)
	if res.BestOverall.Name != "B" {
		t.Fatalf("best %q want B", res.BestOverall.Name)
	}
}

func TestHugeDifferenceBeatsPriority(t *testing.T) {
	inputs := []Input{
		{Name: "Reality", Status: "up", Loss: 0, Median: 280 * time.Millisecond, Download: 80, Priority: 10},
		{Name: "Hysteria", Status: "up", Loss: 0, Median: 170 * time.Millisecond, Download: 125, Priority: 20},
	}
	res := Recommend(inputs, 20)
	if res.BestOverall.Name != "Hysteria" {
		t.Fatalf("best %q", res.BestOverall.Name)
	}
}

func TestCloseAllowsPriority(t *testing.T) {
	inputs := []Input{
		{Name: "Reality", Status: "up", Loss: 0, Median: 180 * time.Millisecond, Download: 100, Priority: 10},
		{Name: "Hysteria", Status: "up", Loss: 0, Median: 170 * time.Millisecond, Download: 103, Priority: 20},
	}
	res := Recommend(inputs, 5)
	if res.BestOverall.Name != "Reality" {
		t.Fatalf("close should allow priority Reality but got %q", res.BestOverall.Name)
	}
}

func TestDownNeverRecommended(t *testing.T) {
	inputs := []Input{
		{Name: "Down", Status: "down", Loss: 100, Median: 0, Priority: 10},
		{Name: "Up", Status: "up", Loss: 0, Median: 200 * time.Millisecond, Priority: 20},
	}
	res := Recommend(inputs, 5)
	if res.BestOverall.Name != "Up" {
		t.Fatalf("down wins")
	}
}

func TestAllDownProducesNoRecommendation(t *testing.T) {
	res := Recommend([]Input{
		{Name: "A", Status: "down", Loss: 100, Priority: 10},
		{Name: "B", Status: "down", Loss: 100, Priority: 20},
	}, 20)
	if res.BestOverall.Name != "" || len(res.Avoid) != 2 {
		t.Fatalf("all-down routes must not be recommended: %+v", res)
	}
}

func TestDegradedOnlyIfNoHealthy(t *testing.T) {
	inputs := []Input{
		{Name: "Degraded", Status: "degraded", Loss: 5, Median: 200 * time.Millisecond, Priority: 10},
		{Name: "Down", Status: "down", Loss: 100, Median: 0, Priority: 20},
	}
	res := Recommend(inputs, 5)
	if res.BestOverall.Name != "Degraded" {
		t.Fatalf("degraded should win")
	}
}

func TestHistoryConfidence(t *testing.T) {
	low := Recommend([]Input{{Name: "A", Status: "up", Loss: 0, Median: 100 * time.Millisecond}}, 2)
	if low.Confidence != "low" {
		t.Errorf("low")
	}
	med := Recommend([]Input{{Name: "A", Status: "up", Loss: 0, Median: 100 * time.Millisecond}}, 15)
	if med.Confidence != "medium" {
		t.Errorf("medium")
	}
	high := Recommend([]Input{{Name: "A", Status: "up", Loss: 0, Median: 100 * time.Millisecond}}, 60)
	if high.Confidence != "high" {
		t.Errorf("high")
	}
}

func TestPerRouteConfidence(t *testing.T) {
	// Global history is large but the WINNER has almost no evidence of its own:
	// confidence must come from the winner's own count, not global rows.
	inputs := []Input{
		{Name: "fresh", Status: "up", Loss: 0, Median: 100 * time.Millisecond, Priority: 10, HistoryCount: 3},
		{Name: "old", Status: "up", Loss: 0, Median: 500 * time.Millisecond, Priority: 20, HistoryCount: 90},
	}
	res := Recommend(inputs, 93)
	if res.BestOverall.Name != "fresh" {
		t.Fatalf("best %q want fresh", res.BestOverall.Name)
	}
	if res.Confidence != "low" {
		t.Errorf("confidence %q want low (winner has 3 samples)", res.Confidence)
	}
	if res.ConfidenceDetail != "Based on 3 measurements for fresh" {
		t.Errorf("detail %q", res.ConfidenceDetail)
	}
}

func TestAvoidList(t *testing.T) {
	inputs := []Input{
		{Name: "Good", Status: "up", Loss: 0, Median: 100 * time.Millisecond},
		{Name: "Bad", Status: "down", Loss: 100, Median: 0},
		{Name: "Lossy", Status: "up", Loss: 40, Median: 100 * time.Millisecond},
	}
	res := Recommend(inputs, 5)
	if len(res.Avoid) != 2 {
		t.Fatalf("avoid %d", len(res.Avoid))
	}
}
