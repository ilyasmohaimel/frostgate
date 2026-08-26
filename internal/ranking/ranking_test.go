package ranking

import (
	"math"
	"reflect"
	"sort"
	"testing"
	"time"
)

func TestMajorPerformanceDifference(t *testing.T) {
	cands := []Candidate{
		{Name: "Reality", IsProxy: true, Status: "up", LossPct: 0, Median: 280 * time.Millisecond, DownloadMbps: 80, Priority: 10},
		{Name: "Hysteria2", IsProxy: true, Status: "up", LossPct: 0, Median: 170 * time.Millisecond, DownloadMbps: 125, Priority: 20},
	}
	got := Sort(cands)
	if got[0].Name != "Hysteria2" {
		t.Fatalf("expected Hysteria2 first, got %s", got[0].Name)
	}
}

func TestClosePerformanceAllowsPriority(t *testing.T) {
	cands := []Candidate{
		{Name: "Reality", IsProxy: true, Status: "up", LossPct: 0, Median: 180 * time.Millisecond, DownloadMbps: 100, Priority: 10},
		{Name: "Hysteria2", IsProxy: true, Status: "up", LossPct: 0, Median: 170 * time.Millisecond, DownloadMbps: 103, Priority: 20},
	}
	got := Sort(cands)
	if got[0].Name != "Reality" {
		t.Fatalf("expected Reality (priority) first, got %s", got[0].Name)
	}
}

func TestReliabilityLossWins(t *testing.T) {
	cands := []Candidate{
		{Name: "Lossy", IsProxy: true, Status: "up", LossPct: 20, Median: 120 * time.Millisecond, Priority: 10},
		{Name: "Clean", IsProxy: true, Status: "up", LossPct: 0, Median: 160 * time.Millisecond, Priority: 20},
	}
	got := Sort(cands)
	if got[0].Name != "Clean" {
		t.Fatalf("expected Clean (0%% loss) first, got %s", got[0].Name)
	}
}

func TestDownNeverWins(t *testing.T) {
	cands := []Candidate{
		{Name: "Down", IsProxy: true, Status: "down", LossPct: 100, Median: 0, Priority: 10},
		{Name: "Up", IsProxy: true, Status: "up", LossPct: 0, Median: 200 * time.Millisecond, Priority: 20},
	}
	if b := Best(cands); b.Name != "Up" {
		t.Fatalf("down recommended: %s", b.Name)
	}
}

func TestAllDownHasNoWinner(t *testing.T) {
	if best := Best([]Candidate{{Name: "Down", IsProxy: true, Status: "down", LossPct: 100}}); best != nil {
		t.Fatalf("all-down set must have no winner: %+v", best)
	}
}

func TestHistoryInstabilityAffectsRanking(t *testing.T) {
	good := Candidate{Name: "Good", IsProxy: true, Status: "up", LossPct: 0, Median: 200 * time.Millisecond, Priority: 10, HistoryCount: 50, AvailabilityPct: 99}
	shaky := Candidate{Name: "Shaky", IsProxy: true, Status: "up", LossPct: 0, Median: 190 * time.Millisecond, Priority: 20, HistoryCount: 50, AvailabilityPct: 70}
	got := Sort([]Candidate{good, shaky})
	if got[0].Name != "Good" {
		t.Fatalf("history instability should drop Shaky, got %s", got[0].Name)
	}
}

func TestPermutationStability(t *testing.T) {
	base := []Candidate{
		{Name: "A", IsProxy: true, Status: "up", LossPct: 0, Median: 180 * time.Millisecond, DownloadMbps: 100, Priority: 10, HistoryCount: 50, AvailabilityPct: 95},
		{Name: "B", IsProxy: true, Status: "up", LossPct: 0, Median: 170 * time.Millisecond, DownloadMbps: 103, Priority: 20, HistoryCount: 50, AvailabilityPct: 95},
		{Name: "C", IsProxy: true, Status: "degraded", LossPct: 8, Median: 160 * time.Millisecond, Priority: 15, HistoryCount: 5, AvailabilityPct: 90},
		{Name: "D", IsProxy: true, Status: "down", LossPct: 100, Median: 0, Priority: 5},
	}
	// Compute reference ordering from sorted base.
	ref := Sort(append([]Candidate{}, base...))
	winner := ref[0].Name

	perms := permutations(base)
	for _, p := range perms {
		got := Sort(append([]Candidate{}, p...))
		if got[0].Name != winner {
			t.Fatalf("permutation winner mismatch: want %s got %s (perm %v)", winner, got[0].Name, names(p))
		}
		if !reflect.DeepEqual(names(got), names(ref)) {
			t.Fatalf("permutation ordering mismatch:\nref %v\ngot %v", names(ref), names(got))
		}
	}
}

func TestOrderingIsTransitive(t *testing.T) {
	cands := []Candidate{
		{Name: "A", IsProxy: true, Status: "up", Median: 170 * time.Millisecond, DownloadMbps: 103, Priority: 20},
		{Name: "B", IsProxy: true, Status: "up", Median: 180 * time.Millisecond, DownloadMbps: 100, Priority: 10},
		{Name: "C", IsProxy: true, Status: "up", Median: 205 * time.Millisecond, DownloadMbps: 95, Priority: 30},
		{Name: "D", IsProxy: true, Status: "degraded", LossPct: 8, Median: 120 * time.Millisecond, Priority: 5},
	}
	ordered := Sort(cands)
	for i := 0; i < len(ordered); i++ {
		for j := i + 1; j < len(ordered); j++ {
			pair := Sort([]Candidate{ordered[i], ordered[j]})
			if pair[0].Name != ordered[i].Name {
				t.Fatalf("global order contradicts pair order: %s before %s globally, pair=%v", ordered[i].Name, ordered[j].Name, names(pair))
			}
		}
	}
}

func TestHealthStatusIsStrictRankingTier(t *testing.T) {
	candidates := []Candidate{
		{Name: "up-extreme", IsProxy: true, Status: "up", Median: time.Duration(1<<63 - 1), DownloadMbps: 0},
		{Name: "degraded-infinite-download", IsProxy: true, Status: "degraded", DownloadMbps: math.Inf(1)},
		{Name: "down-fast", IsProxy: true, Status: "down", Median: 0},
	}
	got := Sort(candidates)
	if names(got)[0] != "up-extreme" || names(got)[1] != "degraded-infinite-download" || names(got)[2] != "down-fast" {
		t.Fatalf("status tiers crossed: %v", names(got))
	}
}

func names(c []Candidate) []string {
	out := make([]string, len(c))
	for i, x := range c {
		out[i] = x.Name
	}
	return out
}

func permutations(in []Candidate) [][]Candidate {
	if len(in) <= 1 {
		return [][]Candidate{append([]Candidate{}, in...)}
	}
	var out [][]Candidate
	for i, x := range in {
		rest := append([]Candidate{}, in[:i]...)
		rest = append(rest, in[i+1:]...)
		for _, p := range permutations(rest) {
			out = append(out, append([]Candidate{x}, p...))
		}
	}
	sort.Slice(out, func(i, j int) bool { return names(out[i])[0] < names(out[j])[0] })
	return out
}
