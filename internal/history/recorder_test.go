package history

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ilyasmohaimel/frostgate/internal/config"
	"github.com/ilyasmohaimel/frostgate/internal/probe"
)

func withTempDataHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	return dir
}

func TestRecorderDisabledNeverCreatesFiles(t *testing.T) {
	dataHome := withTempDataHome(t)
	rec := NewRecorder(false, 90*24*time.Hour, 5*time.Minute)
	if rec.Enabled() {
		t.Fatal("should be disabled")
	}
	written, err := rec.RecordThrottled([]Record{{Route: "x", Status: "up"}})
	if err != nil || written {
		t.Fatalf("disabled recorder must no-op: written=%v err=%v", written, err)
	}
	if err := rec.Record([]Record{{Route: "y", Status: "up"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dataHome, "frostgate", "history.db")); !os.IsNotExist(err) {
		t.Fatal("history db must not exist when disabled")
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRecorderEnabledWritesAndPrunes(t *testing.T) {
	withTempDataHome(t)
	retention := 24 * time.Hour
	rec := NewRecorder(true, retention, time.Hour)
	old := Record{Timestamp: time.Now().Add(-72 * time.Hour), Route: "old", Status: "up"}
	fresh := Record{Route: "new", Status: "up"}
	if err := rec.Record([]Record{old, fresh}); err != nil {
		t.Fatal(err)
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	all, err := db.Query(0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range all {
		if r.Route == "old" {
			t.Fatal("old record should have been pruned on first write")
		}
	}
	foundNew := false
	for _, r := range all {
		if r.Route == "new" {
			foundNew = true
		}
	}
	if !foundNew {
		t.Fatal("fresh record missing")
	}
}

func TestRecorderThrottlingRespectsWatchInterval(t *testing.T) {
	withTempDataHome(t)
	rec := NewRecorder(true, 90*24*time.Hour, 30*time.Minute)
	if _, err := rec.RecordThrottled([]Record{{Route: "a", Status: "up"}}); err != nil {
		t.Fatal(err)
	}
	written, err := rec.RecordThrottled([]Record{{Route: "b", Status: "up"}})
	if err != nil {
		t.Fatal(err)
	}
	if written {
		t.Fatal("second write inside watch interval must be throttled")
	}
	// Unthrottled writes always land.
	if err := rec.Record([]Record{{Route: "c", Status: "up"}}); err != nil {
		t.Fatal(err)
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	recs, _ := db.Query(0, "", "")
	routes := map[string]bool{}
	for _, r := range recs {
		routes[r.Route] = true
	}
	if !routes["a"] || routes["b"] || !routes["c"] {
		t.Fatalf("want a+c without b, got %+v", recs)
	}
}

func TestOpenReadonlyNoCreate(t *testing.T) {
	dataHome := withTempDataHome(t)
	if _, err := OpenReadonly(); err != ErrNoHistory {
		t.Fatalf("want ErrNoHistory got %v", err)
	}
	if _, err := os.Stat(dataHome); err != nil {
		t.Fatal("readonly open must not create data home")
	}
}

func TestFromProbeResults(t *testing.T) {
	cfg := &config.Config{Defaults: config.Defaults{Samples: 2}}
	ep := config.Endpoint{Name: "r1", Type: config.ProbeHTTP, URL: "http://127.0.0.1:1/x"}
	res := probe.ProbeEndpoint(context.Background(), cfg, ep)
	recs := FromProbeResults([]probe.Result{res}, func(string) float64 { return 12.5 }, "home")
	if len(recs) != 1 {
		t.Fatalf("want 1 record got %d", len(recs))
	}
	if recs[0].Route != "r1" || recs[0].Network != "home" || recs[0].DownloadMbps != 12.5 {
		t.Fatalf("bad record %+v", recs[0])
	}
	if recs[0].Type != string(config.ProbeHTTP) {
		t.Fatalf("type %q", recs[0].Type)
	}
}
