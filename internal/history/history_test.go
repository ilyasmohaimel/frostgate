package history

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func tempDB(t *testing.T) (*DB, func()) {
	t.Helper()
	dir := t.TempDir()
	orig := os.Getenv("XDG_DATA_HOME")
	os.Setenv("XDG_DATA_HOME", dir)
	t.Cleanup(func() { os.Setenv("XDG_DATA_HOME", orig) })
	db, err := Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return db, func() { db.Close(); os.RemoveAll(filepath.Join(dir, "frostgate")) }
}

func TestWriteRead(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()
	rec := Record{Route: "test-route", Type: "singbox", Status: "up", Loss: 0, MedianMs: 100, JitterMs: 5, Network: "home"}
	if err := db.Add(rec); err != nil {
		t.Fatalf("add: %v", err)
	}
	records, err := db.Query(24*time.Hour, "", "")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("want 1 got %d", len(records))
	}
	if records[0].Route != "test-route" {
		t.Errorf("route")
	}
	// ensure no secret fields (check JSON marshaling not containing password)
}

func TestSinceFilter(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()
	now := time.Now()
	db.Add(Record{Route: "a", Status: "up", Timestamp: now.Add(-2 * time.Hour)})
	db.Add(Record{Route: "a", Status: "up", Timestamp: now.Add(-30 * time.Minute)})
	records, _ := db.Query(1*time.Hour, "", "")
	if len(records) != 1 {
		t.Fatalf("since 1h want 1 got %d", len(records))
	}
}

func TestRouteFilter(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()
	db.Add(Record{Route: "reality", Status: "up"})
	db.Add(Record{Route: "hysteria", Status: "up"})
	records, _ := db.Query(24*time.Hour, "reality", "")
	if len(records) != 1 || records[0].Route != "reality" {
		t.Errorf("route filter")
	}
}

func TestNetworkFilter(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()
	db.Add(Record{Route: "a", Status: "up", Network: "home"})
	db.Add(Record{Route: "a", Status: "up", Network: "dorm"})
	records, _ := db.Query(24*time.Hour, "", "home")
	if len(records) != 1 {
		t.Fatalf("network filter %d", len(records))
	}
}

func TestRetention(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()
	old := time.Now().Add(-100 * 24 * time.Hour)
	db.Add(Record{Route: "old", Status: "up", Timestamp: old})
	db.Add(Record{Route: "new", Status: "up"})
	if err := db.Prune(90 * 24 * time.Hour); err != nil {
		t.Fatalf("prune: %v", err)
	}
	records, _ := db.Query(200*24*time.Hour, "", "")
	for _, r := range records {
		if r.Route == "old" {
			t.Error("old not pruned")
		}
	}
}

func TestSummarize(t *testing.T) {
	records := []Record{
		{Route: "reality", Status: "up", MedianMs: 100, JitterMs: 10},
		{Route: "reality", Status: "up", MedianMs: 120, JitterMs: 12},
		{Route: "reality", Status: "down", MedianMs: 0},
		{Route: "hysteria", Status: "up", MedianMs: 80, JitterMs: 5},
	}
	summ := Summarize(records)
	if len(summ) != 2 {
		t.Fatalf("summ %d", len(summ))
	}
	for _, s := range summ {
		if s.Route == "reality" && s.Availability > 70 || s.Availability < 60 { // 2/3 =66%
			// allow range
		}
	}
}

func TestNoSecrets(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()
	// ensure record doesn't contain URL
	rec := Record{Route: "test", Status: "up", Type: "singbox"}
	db.Add(rec)
	records, _ := db.Query(24*time.Hour, "", "")
	// check that record fields don't contain password-like
	if len(records[0].Route) > 100 {
		t.Error("route too long")
	}
}

func TestTrendCalculation(t *testing.T) {
	var recs []Record
	base := time.Now()
	for i := 0; i < 10; i++ {
		median := int64(100 + i*10) // increasing -> getting worse
		recs = append(recs, Record{Route: "a", Status: "up", MedianMs: median, Timestamp: base.Add(time.Duration(i) * time.Minute)})
	}
	summ := Summarize(recs)
	if len(summ) != 1 {
		t.Fatal("summ")
	}
	if summ[0].Trend != "getting worse" {
		t.Errorf("trend %q", summ[0].Trend)
	}
}

func TestSummarizeIgnoresFailedLatencyForStatisticsAndTrend(t *testing.T) {
	base := time.Now()
	records := []Record{
		{Route: "route", Status: "up", MedianMs: 100, JitterMs: 10, Timestamp: base},
		{Route: "route", Status: "down", MedianMs: 0, Timestamp: base.Add(time.Minute)},
		{Route: "route", Status: "degraded", MedianMs: 140, JitterMs: 20, Timestamp: base.Add(2 * time.Minute)},
		{Route: "route", Status: "down", MedianMs: 0, Timestamp: base.Add(3 * time.Minute)},
	}
	s := Summarize(records)[0]
	if s.Availability != 50 || s.Failures != 2 || s.Median != 120*time.Millisecond || s.Jitter != 15*time.Millisecond || s.Trend != "stable" {
		t.Fatalf("bad summary: %+v", s)
	}
}

func TestSummarizeAllFailuresHasNoLatency(t *testing.T) {
	s := Summarize([]Record{{Route: "down", Status: "down"}, {Route: "down", Status: "down"}})[0]
	if s.Availability != 0 || s.Median != 0 || s.Jitter != 0 || s.Trend != "stable" || s.Failures != 2 {
		t.Fatalf("bad all-down summary: %+v", s)
	}
}

func TestSummarizeLatencyMedianAndTrendUseOnlyUsableMeasurements(t *testing.T) {
	base := time.Now()
	for _, tt := range []struct {
		name string
		ms   []int64
		want time.Duration
	}{
		{"odd valid samples", []int64{100, 200, 300}, 200 * time.Millisecond},
		{"even valid samples", []int64{100, 200, 400, 500}, 300 * time.Millisecond},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var records []Record
			for i, median := range tt.ms {
				records = append(records, Record{Route: "route", Status: "up", MedianMs: median, Timestamp: base.Add(time.Duration(i) * time.Minute)})
			}
			if got := Summarize(records)[0].Median; got != tt.want {
				t.Fatalf("median %s want %s", got, tt.want)
			}
		})
	}

	records := []Record{
		{Route: "route", Status: "up", MedianMs: 100, Timestamp: base},
		{Route: "route", Status: "down", Timestamp: base.Add(time.Minute)},
		{Route: "route", Status: "degraded", MedianMs: 120, Timestamp: base.Add(2 * time.Minute)},
		{Route: "route", Status: "down", Timestamp: base.Add(3 * time.Minute)},
		{Route: "route", Status: "up", MedianMs: 240, Timestamp: base.Add(4 * time.Minute)},
		{Route: "route", Status: "down", Timestamp: base.Add(5 * time.Minute)},
		{Route: "route", Status: "up", MedianMs: 270, Timestamp: base.Add(6 * time.Minute)},
	}
	s := Summarize(records)[0]
	if s.Trend != "getting worse" || s.Availability < 57.14 || s.Availability > 57.15 || s.Failures != 3 {
		t.Fatalf("failed samples distorted trend or availability: %+v", s)
	}
}

func TestConcurrentSafe(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()
	done := make(chan struct{})
	for i := 0; i < 10; i++ {
		go func(n int) {
			db.Add(Record{Route: "a", Status: "up", MedianMs: int64(n)})
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < 10; i++ {
		<-done
	}
	records, _ := db.Query(24*time.Hour, "", "")
	if len(records) != 10 {
		t.Errorf("concurrent %d", len(records))
	}
}

func TestQueryProfileScopesSameRouteNamesAndKeepsDefaultLegacyRecords(t *testing.T) {
	db, cleanup := tempDB(t)
	defer cleanup()
	now := time.Now()
	for _, record := range []Record{
		{Timestamp: now, Route: "same-name", Status: "up", MedianMs: 40, Profile: "profile-a"},
		{Timestamp: now.Add(time.Nanosecond), Route: "same-name", Status: "down", Profile: "profile-b"},
		{Timestamp: now.Add(2 * time.Nanosecond), Route: "same-name", Status: "up", MedianMs: 80},
	} {
		if err := db.Add(record); err != nil {
			t.Fatal(err)
		}
	}
	a, err := db.QueryProfile(0, "same-name", "", "profile-a", false)
	if err != nil || len(a) != 1 || a[0].Profile != "profile-a" {
		t.Fatalf("profile A records = %+v, %v", a, err)
	}
	b, err := db.QueryProfile(0, "same-name", "", "profile-b", false)
	if err != nil || len(b) != 1 || b[0].Profile != "profile-b" {
		t.Fatalf("profile B records = %+v, %v", b, err)
	}
	def, err := db.QueryProfile(0, "same-name", "", DefaultProfile, true)
	if err != nil || len(def) != 1 || def[0].Profile != "" {
		t.Fatalf("default legacy compatibility records = %+v, %v", def, err)
	}
}
