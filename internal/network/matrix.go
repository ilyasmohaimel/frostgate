package network

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/ilyasmohaimel/frostgate/internal/singbox"
)

// RouteProbe describes one proxy column of the destination matrix.
type RouteProbe struct {
	Name     string
	Outbound string
	Kind     string // "reality", "hysteria2", or "proxy"
}

// MatrixCell is the per-route result for a destination.
type MatrixCell struct {
	Route  string `json:"route"`
	Kind   string `json:"kind"`
	Status string `json:"status"` // PASS / FAIL / UNAVAILABLE
	Detail string `json:"detail,omitempty"`
}

// MatrixRow is one destination tested Direct plus via each proxy route.
type MatrixRow struct {
	Destination  string       `json:"destination"`
	Direct       string       `json:"direct"` // PASS / FAIL
	DirectDetail string       `json:"direct_detail,omitempty"`
	Routes       []MatrixCell `json:"routes"`
}

// DestinationMatrix probes every configured destination directly and through
// each proxy route using sing-box's per-outbound URL-test (Delay). It never
// mutates the benchmark selector.
func DestinationMatrix(ctx context.Context, dests []string, routes []RouteProbe, client *singbox.Client, timeout time.Duration) []MatrixRow {
	rows := make([]MatrixRow, 0, len(dests))
	for _, dest := range dests {
		row := MatrixRow{Destination: dest}
		row.Direct, row.DirectDetail = checkDirectStatus(ctx, dest)
		for _, r := range routes {
			cell := MatrixCell{Route: r.Name, Kind: r.Kind}
			if client == nil || r.Outbound == "" {
				cell.Status = "UNAVAILABLE"
				cell.Detail = "no controller"
				row.Routes = append(row.Routes, cell)
				continue
			}
			dctx, dcancel := context.WithTimeout(ctx, timeout)
			_, err := client.Delay(dctx, r.Outbound, dest, timeout)
			dcancel()
			if err != nil {
				if ctx.Err() != nil {
					cell.Status = "UNAVAILABLE"
					cell.Detail = "cancelled"
				} else {
					cell.Status = "FAIL"
					cell.Detail = errString(err)
				}
			} else {
				cell.Status = "PASS"
			}
			row.Routes = append(row.Routes, cell)
		}
		rows = append(rows, row)
	}
	return rows
}

func checkDirectStatus(ctx context.Context, url string) (string, string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "FAIL", "bad url"
	}
	start := time.Now()
	resp, err := directMatrixClient.Do(req)
	if err != nil {
		return "FAIL", errString(err)
	}
	defer resp.Body.Close()
	ms := time.Since(start).Milliseconds()
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return "PASS", fmt.Sprintf("%d %dms", resp.StatusCode, ms)
	}
	return "FAIL", fmt.Sprintf("status %d", resp.StatusCode)
}

var directMatrixClient = &http.Client{
	Timeout: 6 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// DefaultDestinations is used when no network_check.destinations are
// configured.
var DefaultDestinations = []string{
	"https://github.com/",
	"https://www.google.com/generate_204",
	"https://www.wikipedia.org/",
}
