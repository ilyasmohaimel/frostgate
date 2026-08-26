package benchmark

import (
	"context"
	"fmt"
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
// returns, including after measurement errors and context cancellation.
//
// measure is injected so tests can observe which route was actually active.
// Deprecated callers that need restoration failures should use
// BenchRoutesChecked.
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
	out, _ := BenchRoutesChecked(ctx, ctrl, selector, socksAddr, downloadURL, bytesPerRoute, routes, measure)
	return out
}

// BenchRoutesChecked is the fail-closed attributed benchmark API. It captures
// the active selector before the first switch and returns a restoration error
// to the caller instead of silently leaving the selector changed.
func BenchRoutesChecked(
	ctx context.Context,
	ctrl SelectorController,
	selector string,
	socksAddr string,
	downloadURL string,
	bytesPerRoute int64,
	routes []string,
	measure func(ctx context.Context, socksAddr, url string, bytes int64) (float64, error),
) ([]RouteThroughput, error) {
	out := make([]RouteThroughput, 0, len(routes))
	if len(routes) == 0 {
		return out, nil
	}
	if selector == "" {
		selector = "frostgate-selector"
	}
	originalSelector, err := ctrl.GetSelectedOutbound(ctx, selector)
	if err != nil || originalSelector == "" {
		for _, route := range routes {
			out = append(out, RouteThroughput{Route: route, NAReason: "selector state unavailable"})
		}
		if err == nil {
			err = fmt.Errorf("selector %q has no active outbound", selector)
		}
		return out, fmt.Errorf("capture selector state: %w", err)
	}
	var restoreErr error
	var lifecycleErr error
	restore := func() {
		rctx, rcancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer rcancel()
		restoreErr = ctrl.SelectOutbound(rctx, selector, originalSelector)
	}
routeLoop:
	for _, route := range routes {
		if ctx.Err() != nil {
			break
		}
		res := RouteThroughput{Route: route}
		if err := ctrl.SelectOutbound(ctx, selector, route); err != nil {
			res.NAReason = "selector switch failed"
			out = append(out, res)
			// A failed selector change is not a route-speed result: Frostgate
			// could not establish the requested attribution lifecycle. Keep the
			// per-route N/A detail, restore the original selection, and surface a
			// command-level failure to callers.
			if lifecycleErr == nil {
				lifecycleErr = fmt.Errorf("switch selector %q to %q: %w", selector, route, err)
			}
			continue
		}
		// Give the core a beat to settle connections through the new outbound.
		settle := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !settle.Stop() {
				<-settle.C
			}
			res.NAReason = "throughput measurement cancelled"
			out = append(out, res)
			break routeLoop
		case <-settle.C:
		}
		mctx, mcancel := context.WithTimeout(ctx, 30*time.Second)
		mbps, err := measure(mctx, socksAddr, downloadURL, bytesPerRoute)
		mcancel()
		if err != nil {
			res.NAReason = "throughput measurement failed"
			if ctx.Err() != nil {
				out = append(out, res)
				break
			}
		} else {
			res.DownloadMbps = mbps
		}
		out = append(out, res)
	}
	// A named return would obscure the error from the deferred restore. Restore
	// explicitly once here so callers can never treat a failed restore as pass.
	restore()
	if restoreErr != nil {
		return out, fmt.Errorf("restore selector %q: %w", selector, restoreErr)
	}
	if lifecycleErr != nil {
		return out, lifecycleErr
	}
	return out, nil
}
