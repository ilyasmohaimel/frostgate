package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ilyasmohaimel/frostgate/internal/config"
	"github.com/ilyasmohaimel/frostgate/internal/history"
	"github.com/ilyasmohaimel/frostgate/internal/network"
)

func TestApplyTimeoutOverrideOverridesEndpointTimeout(t *testing.T) {
	cfg := &config.Config{
		Defaults:  config.Defaults{Timeout: config.Duration{Duration: 5 * time.Second}},
		Endpoints: []config.Endpoint{{Timeout: config.Duration{Duration: 30 * time.Second}}},
	}
	applyTimeoutOverride(cfg, 2*time.Second)
	if got := cfg.TimeoutFor(cfg.Endpoints[0]); got != 2*time.Second {
		t.Fatalf("global timeout = %s, want 2s", got)
	}
}

func TestBenchmarkMigrationSkipsDirectOnlyProfiles(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(filepath.Join(dir, "sing-box.json"), []byte("legacy sidecar"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Endpoints: []config.Endpoint{{Type: config.ProbeHTTP}}}
	migrated, err := migrateLegacyProfileForBenchmarkIfNeeded(cfg, configPath, filepath.Join(dir, "managed"))
	if err != nil || migrated {
		t.Fatalf("direct-only migration = %v, %v", migrated, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "managed")); !os.IsNotExist(err) {
		t.Fatalf("direct-only migration touched managed state: %v", err)
	}
}

func TestNetworkCheckSkipsMalformedManagedStateForDirectOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sing-box.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Endpoints: []config.Endpoint{{Type: config.ProbeHTTP}}}
	managed, err := loadManagedConfigForNetworkCheck(cfg, dir)
	if err != nil || managed != nil {
		t.Fatalf("direct-only managed load = %#v, %v", managed, err)
	}
}

func TestNetworkCheckRequiresValidManagedStateForProxy(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sing-box.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Endpoints: []config.Endpoint{{Type: config.ProbeSingBox}}}
	if _, err := loadManagedConfigForNetworkCheck(cfg, dir); err == nil {
		t.Fatal("malformed proxy managed state was accepted")
	}
}

func TestNetworkCheckExitCodeReportsFailures(t *testing.T) {
	tests := []struct {
		name string
		doc  networkCheckDoc
		want int
	}{
		{name: "pass and unavailable", doc: networkCheckDoc{Checks: []network.Check{{Status: "PASS"}, {Status: "UNAVAILABLE"}}}, want: 0},
		{name: "check failure", doc: networkCheckDoc{Checks: []network.Check{{Status: "FAIL"}}}, want: 1},
		{name: "matrix direct failure", doc: networkCheckDoc{Matrix: []network.MatrixRow{{Direct: "FAIL"}}}, want: 1},
		{name: "matrix route failure", doc: networkCheckDoc{Matrix: []network.MatrixRow{{Direct: "PASS", Routes: []network.MatrixCell{{Status: "FAIL"}}}}}, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := networkCheckExitCode(tt.doc); got != tt.want {
				t.Fatalf("exit code = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCancelledWatchProbeIsNotRecordedAsOutage(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-started:
		default:
			close(started)
		}
		<-r.Context().Done()
	}))
	defer srv.Close()
	cfg := &config.Config{
		Defaults: config.Defaults{
			Timeout:        config.Duration{Duration: 5 * time.Second},
			Samples:        1,
			HealthyLatency: config.Duration{Duration: time.Second},
			MaxJitter:      config.Duration{Duration: time.Second},
		},
		History:   config.HistoryConfig{Enabled: boolPtr(true)},
		Endpoints: []config.Endpoint{{Name: "cancelled", Type: config.ProbeHTTP, URL: srv.URL}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	if code := watchJSON(ctx, cfg, history.DefaultProfile, time.Second, false); code != 0 {
		t.Fatalf("cancelled watch code = %d", code)
	}
	if _, err := history.OpenReadonly(); !errors.Is(err, history.ErrNoHistory) {
		t.Fatalf("cancelled watch created outage history: %v", err)
	}
}

func boolPtr(value bool) *bool { return &value }
