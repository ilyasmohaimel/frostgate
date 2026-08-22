package benchmark

import (
	"context"
	"sync"
	"time"
)

// SelectorController is the minimal clash-api surface needed to attribute
// throughput measurements to a specific route.
type SelectorController interface {
	GetSelectedOutbound(ctx context.Context, selector string) (string, error)
	SelectOutbound(ctx context.Context, selector, target string) error
}

// MeasureSOCKSMbps adapts MeasureViaSOCKS to the single-return measure
// signature used by BenchRoutes.
func MeasureSOCKSMbps(ctx context.Context, socksAddr, downloadURL string, bytes int64) (float64, error) {
	mbps, _, err := MeasureViaSOCKS(ctx, socksAddr, downloadURL, bytes)
	return mbps, err
}

// RouteThroughput is the per-route outcome of an attributed measurement.
type RouteThroughput struct {
	Route        string  `json:"route"`
	DownloadMbps float64 `json:"download_mbps"`
	NAReason     string  `json:"na_reason,omitempty"`
}

// BenchRoutes measures throughput for each route by switching the selector to
// that route and measuring via the local SOCKS listener. The original
// selection is captured up front and always restored when the function
// returns — on success, on measurement errors, and on context cancellation.
//
// measure is injected so tests can observe which route was actually active.
func BenchRoutes(
	ctx context.Context,
	ctrl SelectorController,
	selector string,
	socksAddr string,
	downloadURL string,
	bytesPerRoute int64,
	routes []string,
	measure func(ctx context.Context, socksAddr, url string, bytes int64) (float64, error),
) []RouteThroughput {
	out := make([]RouteThroughput, 0, len(routes))
	if len(routes) == 0 {
		return out
	}
	if selector == "" {
		selector = "frostgate-selector"
	}
	originalSelector := ""
	selectorExists := false
	if now, err := ctrl.GetSelectedOutbound(ctx, selector); err == nil && now != "" {
		originalSelector = now
		selectorExists = true
	}
	var restoreOnce sync.Once
	restore := func() {
		restoreOnce.Do(func() {
			if !selectorExists || originalSelector == "" {
				return
			}
			rctx, rcancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer rcancel()
			_ = ctrl.SelectOutbound(rctx, selector, originalSelector)
		})
	}
	// Restore on early return, error paths, and context cancellation.
	defer restore()
	go func() {
		<-ctx.Done()
		restore()
	}()
	for _, route := range routes {
		res := RouteThroughput{Route: route}
		if err := ctrl.SelectOutbound(ctx, selector, route); err != nil {
			res.NAReason = "selector switch failed"
			out = append(out, res)
			continue
		}
		// Give the core a beat to settle connections through the new outbound.
		time.Sleep(100 * time.Millisecond)
		mctx, mcancel := context.WithTimeout(ctx, 30*time.Second)
		mbps, err := measure(mctx, socksAddr, downloadURL, bytesPerRoute)
		mcancel()
		if err != nil {
			res.NAReason = "throughput measurement failed"
			if ctx.Err() != nil {
				out = append(out, res)
				return out
			}
		} else {
			res.DownloadMbps = mbps
		}
		out = append(out, res)
	}
	return out
}
