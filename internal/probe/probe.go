// Package probe runs concurrent reachability/latency checks against endpoints.
package probe

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"sort"
	"time"

	"github.com/ilyasmohaimel/frostgate/internal/config"
	"golang.org/x/sync/errgroup"
)

// Status is the health classification of a probed endpoint.
type Status string

const (
	StatusUp       Status = "up"
	StatusDegraded Status = "degraded"
	StatusDown     Status = "down"
)

// Result is the outcome of probing one endpoint.
type Result struct {
	Endpoint config.Endpoint `json:"endpoint"`
	Latency  time.Duration   `json:"latency_ns"`
	Status   Status          `json:"status"`
	Err      error           `json:"-"`
	ErrMsg   string          `json:"error,omitempty"`
}

// OK reports whether the route is usable (up or degraded, not down).
func (r Result) OK() bool { return r.Status != StatusDown }

// httpClient is shared across probes; per-probe deadlines come from the
// request context, so we deliberately leave Client.Timeout unset.
var httpClient = &http.Client{
	// Don't follow redirects — we're measuring the endpoint, not where it sends us.
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// Run probes every endpoint concurrently and returns the results, ranked.
func Run(ctx context.Context, cfg *config.Config) []Result {
	results := make([]Result, len(cfg.Endpoints))
	timeout := cfg.Defaults.Timeout.Duration

	// errgroup manages the goroutines and lets us Wait() for all of them.
	// We return nil from each goroutine on purpose: a single down endpoint
	// must NOT cancel the context and abort the others.
	g, ctx := errgroup.WithContext(ctx)
	for i, ep := range cfg.Endpoints {
		g.Go(func() error {
			results[i] = Probe(ctx, ep, timeout)
			return nil
		})
	}
	_ = g.Wait()

	Rank(results)
	return results
}

// Probe runs the type-appropriate check for one endpoint and times it.
func Probe(ctx context.Context, e config.Endpoint, timeout time.Duration) Result {
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
	default:
		err = fmt.Errorf("unsupported probe type %q", e.Type)
	}
	latency := time.Since(start)

	res := Result{
		Endpoint: e,
		Latency:  latency,
		Status:   Classify(err, latency, timeout),
		Err:      err,
	}
	if err != nil {
		res.ErrMsg = err.Error()
	}
	return res
}

// probeTCP measures the time to open a raw TCP connection.
func probeTCP(ctx context.Context, e config.Endpoint) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", e.Addr())
	if err != nil {
		return err
	}
	return conn.Close()
}

// probeTLS measures TCP connect plus a full TLS handshake.
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

// probeHTTP measures time to first response from an HTTPS GET.
func probeHTTP(ctx context.Context, e config.Endpoint) error {
	path := e.Path
	if path == "" {
		path = "/"
	}
	url := fmt.Sprintf("https://%s%s", e.Addr(), path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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

// Classify turns a probe outcome into a Status.
//
// MAKE-IT-YOURS #2 (the health policy): this is where you decide what "down"
// vs "degraded" vs "up" actually means. The default below is deliberately
// simple — a working starting point you should tune to your reality:
//   - Any error  -> down.
//   - Succeeded but slow (>60% of the timeout budget) -> degraded.
//   - Otherwise  -> up.
// Things to consider making it yours:
//   - Should a TLS cert error be "down" or its own "insecure" status?
//   - Is a fixed 60% threshold right, or should "slow" be an absolute ms value
//     you set per-endpoint?
//   - In China a route at 400ms might still be your best option — does
//     "degraded" mean "avoid" or just "warn"?
func Classify(err error, latency, timeout time.Duration) Status {
	if err != nil {
		return StatusDown
	}
	if latency > (timeout*6)/10 {
		return StatusDegraded
	}
	return StatusUp
}

// statusRank orders statuses best-to-worst for sorting.
func statusRank(s Status) int {
	switch s {
	case StatusUp:
		return 0
	case StatusDegraded:
		return 1
	default:
		return 2
	}
}

// Rank sorts results in place from best route to worst.
//
// MAKE-IT-YOURS #3 (the ranking): this decides which route frostgate calls
// "best right now". The default sorts by status, then by raw latency. But the
// interesting product decisions are yours:
//   - Should a fast Hysteria 2 backup outrank a slightly faster Reality primary,
//     or do you prefer your primary unless it's clearly worse? (protocol weight)
//   - Should you rank on jitter/stability once you sample multiple times, not a
//     single-shot latency? (needs the v1 multi-sample probe)
//   - Tie-breaking: alphabetical, config order, or region?
// Edit less() to encode your preference.
func Rank(results []Result) {
	sort.SliceStable(results, func(i, j int) bool {
		return less(results[i], results[j])
	})
}

// less reports whether result a should rank above result b.
func less(a, b Result) bool {
	if statusRank(a.Status) != statusRank(b.Status) {
		return statusRank(a.Status) < statusRank(b.Status)
	}
	return a.Latency < b.Latency
}

// Best returns the top-ranked usable route, or false if every route is down.
// Call Rank first (Run already does).
func Best(results []Result) (Result, bool) {
	for _, r := range results {
		if r.OK() {
			return r, true
		}
	}
	return Result{}, false
}
