package probe

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ilyasmohaimel/frostgate/internal/config"
	"github.com/ilyasmohaimel/frostgate/internal/health"
	"github.com/ilyasmohaimel/frostgate/internal/metrics"
	"github.com/ilyasmohaimel/frostgate/internal/singbox"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
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
	results := make([]Result, len(cfg.Endpoints))
	sem := semaphore.NewWeighted(32)

	g, ctx := errgroup.WithContext(ctx)
	for i, ep := range cfg.Endpoints {
		if err := sem.Acquire(ctx, 1); err != nil {
			break
		}
		i, ep := i, ep
		g.Go(func() error {
			defer sem.Release(1)
			results[i] = ProbeEndpoint(ctx, cfg, ep)
			return nil
		})
	}
	_ = g.Wait()
	Rank(results)
	return results
}

func ProbeEndpoint(ctx context.Context, cfg *config.Config, e config.Endpoint) Result {
	timeout := cfg.TimeoutFor(e)
	samples := cfg.SamplesFor(e)
	interval := cfg.SampleIntervalFor(e)

	var s []metrics.Sample
	for i := 0; i < samples; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				s = append(s, metrics.Sample{Success: false, Err: ctx.Err(), ErrCategory: categorizeError(ctx.Err())})
				break
			case <-time.After(interval):
			}
		}
		sample := probeOnce(ctx, cfg, e, timeout)
		s = append(s, sample)
		if ctx.Err() != nil {
			break
		}
	}

	m := metrics.Compute(s)

	th := health.Thresholds{
		HealthyLatency:  cfg.HealthyLatencyFor(e),
		MaxJitter:       cfg.MaxJitterFor(e),
		MaxLossPct:      cfg.MaxLossFor(e),
		DegradedLossPct: 50,
	}
	status := health.Classify(m, th)

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
	switch e.Type {
	case config.ProbeTCP:
		err = probeTCP(ctx, e)
	case config.ProbeTLS:
		err = probeTLS(ctx, e)
	case config.ProbeHTTP:
		err = probeHTTP(ctx, e)
	case config.ProbeSingBox:
		err = probeSingBox(ctx, cfg, e, timeout)
	default:
		err = fmt.Errorf("unsupported probe type %q", e.Type)
	}
	lat := time.Since(start)
	if err != nil {
		return metrics.Sample{Success: false, Latency: lat, Err: err, ErrCategory: categorizeError(err)}
	}
	return metrics.Sample{Success: true, Latency: lat, ErrCategory: ""}
}

// legacy Probe for backwards compat and simple tests
func Probe(ctx context.Context, e config.Endpoint, timeout time.Duration) Result {
	cfg := &config.Config{
		Defaults: config.Defaults{
			Timeout:        config.Duration{Duration: timeout},
			Samples:        1,
			SampleInterval: config.Duration{Duration: 0},
			HealthyLatency: config.Duration{Duration: 800 * 1e9},
			MaxJitter:      config.Duration{Duration: 80 * 1e9},
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

func probeHTTP(ctx context.Context, e config.Endpoint) error {
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
	if resp.StatusCode >= 500 {
		return fmt.Errorf("server error: %s", resp.Status)
	}
	return nil
}

func probeSingBox(ctx context.Context, cfg *config.Config, e config.Endpoint, timeout time.Duration) error {
	client := singbox.New(cfg.SingBox.Controller, cfg.SingBox.Secret)
	// Use a per-sample timeout context already applied; but singbox client Delay uses its own timeout param
	// So we need to call Delay with timeout slightly less than context deadline to avoid double timeout?
	// Use timeout for singbox delay param
	d, err := client.Delay(ctx, e.Outbound, cfg.SingBox.TestURL, timeout)
	if err != nil {
		return err
	}
	if d == 0 {
		return fmt.Errorf("proxy-test: zero delay")
	}
	// success; we already measured latency via probeOnce start timer, but we could also use d
	_ = d
	return nil
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

func statusRank(s Status) int { return health.Rank(s) }

func Rank(results []Result) {
	sort.SliceStable(results, func(i, j int) bool {
		return less(results[i], results[j])
	})
}

func less(a, b Result) bool {
	ra := statusRank(a.Status)
	rb := statusRank(b.Status)
	if ra != rb {
		return ra < rb
	}
	// reliability: lower loss wins
	if a.Metrics.LossPct != b.Metrics.LossPct {
		return a.Metrics.LossPct < b.Metrics.LossPct
	}
	// priority: lower value wins (documented)
	if a.Endpoint.Priority != b.Endpoint.Priority {
		return a.Endpoint.Priority < b.Endpoint.Priority
	}
	// median latency
	if a.Metrics.Median != b.Metrics.Median {
		// if one has 0 median due to all failed, treat as large
		if a.Metrics.Median == 0 && b.Metrics.Median != 0 {
			return false
		}
		if b.Metrics.Median == 0 && a.Metrics.Median != 0 {
			return true
		}
		return a.Metrics.Median < b.Metrics.Median
	}
	if a.Metrics.Jitter != b.Metrics.Jitter {
		return a.Metrics.Jitter < b.Metrics.Jitter
	}
	return a.Endpoint.Name < b.Endpoint.Name
}

func Best(results []Result) (Result, bool) {
	for _, r := range results {
		if r.OK() {
			return r, true
		}
	}
	return Result{}, false
}
