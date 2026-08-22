package probe

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ilyasmohaimel/frostgate/internal/config"
)

// countedServer returns an httptest server that counts every request it
// serves, so tests can prove exactly how many samples executed.
func countedServer(t *testing.T, status int) (*httptest.Server, *int64) {
	t.Helper()
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestExpectedStatusExplicitMatch(t *testing.T) {
	srv, _ := countedServer(t, http.StatusNoContent)
	cfg := baseCfg(2, 10*time.Millisecond)
	ep := config.Endpoint{Name: "gen204", Type: config.ProbeHTTP, URL: srv.URL, ExpectedStatus: 204}
	res := ProbeEndpoint(context.Background(), cfg, ep)
	if res.Status != StatusUp {
		t.Fatalf("explicit 204 vs 204 must be up: %+v", res)
	}
}

func TestExpectedStatusExplicitMismatch(t *testing.T) {
	srv, _ := countedServer(t, http.StatusOK)
	cfg := baseCfg(1, 0)
	ep := config.Endpoint{Name: "strict", Type: config.ProbeHTTP, URL: srv.URL, ExpectedStatus: 204}
	res := ProbeEndpoint(context.Background(), cfg, ep)
	if res.Status != StatusDown {
		t.Fatalf("200 must fail when expected_status is 204: %+v", res)
	}
}

func TestGenericHTTPRange(t *testing.T) {
	for _, code := range []int{200, 301, 399} {
		srv, _ := countedServer(t, code)
		cfg := baseCfg(1, 0)
		ep := config.Endpoint{Name: "generic", Type: config.ProbeHTTP, URL: srv.URL}
		res := ProbeEndpoint(context.Background(), cfg, ep)
		if res.Status == StatusDown {
			t.Fatalf("%d should pass generic policy", code)
		}
	}
	srv404, _ := countedServer(t, 404)
	cfg := baseCfg(1, 0)
	res := ProbeEndpoint(context.Background(), cfg, config.Endpoint{Name: "missing", Type: config.ProbeHTTP, URL: srv404.URL})
	if res.Status != StatusDown {
		t.Fatal("404 must fail generic policy")
	}
}

// TestCancellationDuringIntervalStopsCleanly proves the v0.4.1 phantom-sample
// fix with real execution counters: sample 1 executes, the context is
// canceled during the inter-sample wait, sample 2 never executes and no
// synthetic failed sample is appended.
func TestCancellationDuringIntervalStopsCleanly(t *testing.T) {
	srv, hits := countedServer(t, http.StatusNoContent)
	cfg := baseCfg(5, 250*time.Millisecond) // 5 samples, long gaps
	ep := config.Endpoint{Name: "cxl", Type: config.ProbeHTTP, URL: srv.URL}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(60 * time.Millisecond) // after sample 1, during interval wait
		cancel()
	}()
	res := ProbeEndpoint(ctx, cfg, ep)
	if got := atomic.LoadInt64(hits); got != 1 {
		t.Fatalf("exactly one sample must execute, got %d", got)
	}
	if res.Metrics.Attempted != 1 {
		t.Fatalf("attempted must be 1 (no phantom failed sample), got %d", res.Metrics.Attempted)
	}
	if res.Metrics.Failed != 0 {
		t.Fatalf("cancel must not fabricate a failed sample, got %d failed", res.Metrics.Failed)
	}
	if len(res.Samples) != 1 || !res.Samples[0].Success {
		t.Fatalf("only the successful sample must be kept: %+v", res.Samples)
	}
	if res.Endpoint.Name == "" {
		t.Fatal("result must carry endpoint name")
	}
}

// TestRunCancellationNoPhantomResults checks Run() under mid-flight
// cancellation: only started endpoints are returned, no empty names.
func TestRunCancellationNoPhantomResults(t *testing.T) {
	var started int64
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&started, 1)
		<-r.Context().Done()
	}))
	defer slow.Close()
	endpoints := make([]config.Endpoint, 64)
	for i := range endpoints {
		endpoints[i] = config.Endpoint{Name: fmt.Sprintf("slow-%02d", i), Type: config.ProbeHTTP, URL: slow.URL}
	}
	cfg := &config.Config{
		Defaults: config.Defaults{
			Samples:        1,
			SampleInterval: config.Duration{},
			Timeout:        config.Duration{Duration: 5 * time.Second},
			HealthyLatency: config.Duration{Duration: time.Second},
			MaxJitter:      config.Duration{Duration: 100 * time.Millisecond},
		},
		Endpoints: endpoints,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	results := Run(ctx, cfg)
	if len(results) == 0 {
		t.Fatal("started endpoints must still be returned")
	}
	if len(results) > 32 || len(results) >= len(cfg.Endpoints) {
		t.Fatalf("cancellation must prevent unscheduled endpoints from appearing: got %d of %d", len(results), len(cfg.Endpoints))
	}
	names := map[string]bool{}
	for _, r := range results {
		if r.Endpoint.Name == "" {
			t.Fatal("no result may have an empty name")
		}
		if names[r.Endpoint.Name] {
			t.Fatalf("duplicate result for %q", r.Endpoint.Name)
		}
		names[r.Endpoint.Name] = true
	}
	if got := int(atomic.LoadInt64(&started)); got != len(results) {
		t.Fatalf("result count %d must equal actually-started request count %d", len(results), got)
	}
}

// TestProbeLegacySingleSample guards the compatibility wrapper used by tests.
func TestProbeLegacySingleSample(t *testing.T) {
	srv, hits := countedServer(t, 204)
	res := Probe(context.Background(), config.Endpoint{Name: "legacy", Type: config.ProbeHTTP, URL: srv.URL}, time.Second)
	if got := atomic.LoadInt64(hits); got != 1 {
		t.Fatalf("want exactly 1 request, got %d", got)
	}
	if res.Status != StatusUp {
		t.Fatalf("status %+v", res)
	}
}

func baseCfg(samples int, interval time.Duration) *config.Config {
	return &config.Config{
		Defaults: config.Defaults{
			Samples:        samples,
			SampleInterval: config.Duration{Duration: interval},
			Timeout:        config.Duration{Duration: time.Second},
			HealthyLatency: config.Duration{Duration: time.Second},
			MaxJitter:      config.Duration{Duration: 100 * time.Millisecond},
		},
	}
}
