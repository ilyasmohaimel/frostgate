package probe

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/ilyasmohaimel/frostgate/internal/config"
	"github.com/ilyasmohaimel/frostgate/internal/health"
	"github.com/ilyasmohaimel/frostgate/internal/metrics"
	"github.com/ilyasmohaimel/frostgate/internal/ranking"
	"github.com/ilyasmohaimel/frostgate/internal/singbox"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
	"sync"
)

type Status = health.Status

const (
	StatusUp       = health.StatusUp
	StatusDegraded = health.StatusDegraded
	StatusDown     = health.StatusDown
)

type Result struct {
	Endpoint config.Endpoint  `json:"endpoint"`
	Metrics  metrics.Metrics  `json:"metrics"`
	Samples  []metrics.Sample `json:"samples,omitempty"`
	Status   Status           `json:"status"`
	Category string           `json:"category"`
	Reasons  []health.Reason  `json:"reasons,omitempty"`
	Err      error            `json:"-"`
	ErrMsg   string           `json:"error,omitempty"`
}

func (r Result) OK() bool { return r.Status != StatusDown }

var httpClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func Run(ctx context.Context, cfg *config.Config) []Result {
	results := make([]Result, 0, len(cfg.Endpoints))
	var mu sync.Mutex
	sem := semaphore.NewWeighted(32)

	g, ctx := errgroup.WithContext(ctx)
	for i := range cfg.Endpoints {
		if err := sem.Acquire(ctx, 1); err != nil {
			break
		}
		i := i
		g.Go(func() error {
			defer sem.Release(1)
			// Only started endpoints are recorded; endpoints that never
			// began (e.g. context cancelled before scheduling) are not
			// returned as phantom zero-value results.
			r := ProbeEndpoint(ctx, cfg, cfg.Endpoints[i])
			mu.Lock()
			results = append(results, r)
			mu.Unlock()
			return nil
		})
	}
	_ = g.Wait()
	// Defensive: never rank or return phantom entries with no name.
	kept := results[:0]
	for _, r := range results {
		if r.Endpoint.Name != "" {
			kept = append(kept, r)
		}
	}
	results = kept
	Rank(results)
	return results
}

func ProbeEndpoint(ctx context.Context, cfg *config.Config, e config.Endpoint) Result {
	timeout := cfg.TimeoutFor(e)
	samples := cfg.SamplesFor(e)
	interval := cfg.SampleIntervalFor(e)

	var s []metrics.Sample
	collecting := true
	for i := 0; i < samples && collecting; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				// Cancellation while waiting between samples: stop
				// immediately. Do NOT append a synthetic failed attempt and
				// do NOT launch another probe.
				collecting = false
				continue
			case <-time.After(interval):
			}
		}
		if ctx.Err() != nil {
			collecting = false
			continue
		}
		s = append(s, probeOnce(ctx, cfg, e, timeout))
		if ctx.Err() != nil {
			collecting = false
			continue
		}
	}

	m := metrics.Compute(s)

	th := health.Thresholds{
		HealthyLatency: cfg.HealthyLatencyFor(e),
		MaxJitter:      cfg.MaxJitterFor(e),
		MaxLossPct:     cfg.MaxLossFor(e),
		// Any partial loss is classified by the configured max_loss. A
		// complete outage remains DOWN above in health.ClassifyWithReasons.
		DegradedLossPct: 100,
	}
	status, reasons := health.ClassifyWithReasons(m, th)

	// find representative error: last failed sample's error
	var repErr error
	var category string
	var errMsg string
	for i := len(s) - 1; i >= 0; i-- {
		if !s[i].Success && s[i].Err != nil {
			repErr = s[i].Err
			category = s[i].ErrCategory
			errMsg = s[i].Err.Error()
			break
		}
	}
	if repErr == nil && status == StatusDown && m.Succeeded == 0 && len(s) > 0 {
		for _, sam := range s {
			if sam.Err != nil {
				repErr = sam.Err
				category = sam.ErrCategory
				errMsg = sam.Err.Error()
				break
			}
		}
	}

	return Result{
		Endpoint: e,
		Metrics:  m,
		Samples:  s,
		Status:   status,
		Category: category,
		Reasons:  reasons,
		Err:      repErr,
		ErrMsg:   errMsg,
	}
}

// probeOnce runs a single sample
func probeOnce(ctx context.Context, cfg *config.Config, e config.Endpoint, timeout time.Duration) metrics.Sample {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	var err error
	var latency time.Duration
	switch e.Type {
	case config.ProbeTCP:
		err = probeTCP(ctx, e)
	case config.ProbeTLS:
		err = probeTLS(ctx, e)
	case config.ProbeHTTP:
		err = probeHTTP(ctx, e, e.ExpectedStatus)
	case config.ProbeSingBox:
		latency, err = probeSingBox(ctx, cfg, e, timeout)
	default:
		err = fmt.Errorf("unsupported probe type %q", e.Type)
	}
	if latency == 0 {
		latency = time.Since(start)
	}
	if err != nil {
		return metrics.Sample{Success: false, Latency: latency, Err: err, ErrCategory: categorizeError(err)}
	}
	return metrics.Sample{Success: true, Latency: latency, ErrCategory: ""}
}

// legacy Probe for backwards compat and simple tests
func Probe(ctx context.Context, e config.Endpoint, timeout time.Duration) Result {
	cfg := &config.Config{
		Defaults: config.Defaults{
			Timeout:        config.Duration{Duration: timeout},
			Samples:        1,
			SampleInterval: config.Duration{Duration: 0},
			HealthyLatency: config.Duration{Duration: 800 * time.Millisecond},
			MaxJitter:      config.Duration{Duration: 80 * time.Millisecond},
		},
	}
	cfg.Defaults.Samples = 1
	// Ensure endpoint timeout not interfering
	e.Timeout = config.Duration{}
	e.Samples = 0
	return ProbeEndpoint(ctx, cfg, e)
}

func probeTCP(ctx context.Context, e config.Endpoint) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", e.Addr())
	if err != nil {
		return err
	}
	return conn.Close()
}

func probeTLS(ctx context.Context, e config.Endpoint) error {
	d := &tls.Dialer{
		Config: &tls.Config{
			ServerName: e.ServerName(),
			MinVersion: tls.VersionTLS12,
		},
	}
	conn, err := d.DialContext(ctx, "tcp", e.Addr())
	if err != nil {
		return err
	}
	return conn.Close()
}

func probeHTTP(ctx context.Context, e config.Endpoint, expectedStatus int) error {
	urlStr := e.EffectiveURL()
	if urlStr == "" {
		return fmt.Errorf("http probe missing url or host/port")
	}
	// Ensure scheme
	if !strings.HasPrefix(urlStr, "http://") && !strings.HasPrefix(urlStr, "https://") {
		urlStr = "https://" + urlStr
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if expectedStatus != 0 {
		if resp.StatusCode != expectedStatus {
			return fmt.Errorf("expected status %d, got %d", expectedStatus, resp.StatusCode)
		}
		return nil
	}
	// Generic policy: 200-399 is success, 400+ is failure.
	if resp.StatusCode >= 400 || resp.StatusCode < 200 {
		return fmt.Errorf("http status %d", resp.StatusCode)
	}
	return nil
}

func probeSingBox(ctx context.Context, cfg *config.Config, e config.Endpoint, timeout time.Duration) (time.Duration, error) {
	client := singbox.New(cfg.SingBox.Controller, cfg.SingBox.Secret)
	d, err := client.Delay(ctx, e.Outbound, cfg.SingBox.TestURL, timeout)
	if err != nil {
		return 0, err
	}
	if d == 0 {
		return 0, fmt.Errorf("proxy-test: zero delay")
	}
	// The controller reports the end-to-end route delay. The local Clash API
	// request duration only measures the controller round trip, not the proxy.
	return d, nil
}

// Classify is deprecated; use health.Classify. Kept for tests.
func Classify(err error, latency, timeout time.Duration) Status {
	if err != nil {
		return StatusDown
	}
	if latency > (timeout*6)/10 {
		return StatusDegraded
	}
	return StatusUp
}

// Rank orders results using the shared ranking policy (single source of
// truth alongside recommend/compare/benchmark). Direct is included in the
// ordering as a diagnostic baseline; per-route "best proxy" separation is
// handled at the reporting/recommendation layer.
func Rank(results []Result) {
	if len(results) == 0 {
		return
	}
	cands := make([]ranking.Candidate, len(results))
	for i, r := range results {
		cands[i] = ranking.Candidate{
			Name:        r.Endpoint.Name,
			IsProxy:     r.Endpoint.Type == config.ProbeSingBox,
			Status:      string(r.Status),
			LossPct:     r.Metrics.LossPct,
			Median:      r.Metrics.Median,
			Jitter:      r.Metrics.Jitter,
			JitterKnown: r.Metrics.JitterKnown,
			Priority:    r.Endpoint.Priority,
		}
	}
	sorted := ranking.Sort(cands)
	byName := make(map[string]Result, len(results))
	for _, r := range results {
		byName[r.Endpoint.Name] = r
	}
	ordered := make([]Result, len(results))
	for i, c := range sorted {
		ordered[i] = byName[c.Name]
	}
	copy(results, ordered)
}

func Best(results []Result) (Result, bool) {
	for _, r := range results {
		if r.OK() {
			return r, true
		}
	}
	return Result{}, false
}

// HasProxy reports whether any proxy (singbox) route exists in results.
func HasProxy(results []Result) bool {
	for _, r := range results {
		if r.Endpoint.Type == config.ProbeSingBox {
			return true
		}
	}
	return false
}

// IsDirectBaseline reports the deliberately small, backwards-compatible
// baseline identity used by setup-generated configs. Arbitrary health probes
// are not silently repurposed as a user's internet baseline.
func IsDirectBaseline(e config.Endpoint) bool {
	if e.Type != config.ProbeHTTP {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(e.Name)) {
	case "direct", "direct-baseline", "baseline":
		return true
	default:
		return false
	}
}

// DirectBaseline returns an explicitly identified non-proxy diagnostic
// baseline. It never falls back to arbitrary HTTP/TCP/TLS endpoints.
func DirectBaseline(results []Result) (Result, bool) {
	for _, r := range results {
		if IsDirectBaseline(r.Endpoint) {
			return r, true
		}
	}
	return Result{}, false
}

// BestProxy returns the highest-ranked usable proxy route using the shared
// ranking policy. ok is false when no proxy route exists or none is usable.
func BestProxy(results []Result) (Result, bool) {
	var proxies []Result
	for _, r := range results {
		if r.Endpoint.Type == config.ProbeSingBox {
			proxies = append(proxies, r)
		}
	}
	if len(proxies) == 0 {
		return Result{}, false
	}
	Rank(proxies)
	best := proxies[0]
	if !best.OK() {
		return Result{}, false
	}
	return best, true
}
