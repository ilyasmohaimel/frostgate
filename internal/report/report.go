package report

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
	"github.com/ilyasmohaimel/frostgate/internal/probe"
)

var (
	upStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	degradeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	downStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	borderStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	bestStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42"))
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
)

func statusCell(s probe.Status) string {
	switch s {
	case probe.StatusUp:
		return upStyle.Render("● up")
	case probe.StatusDegraded:
		return degradeStyle.Render("● degraded")
	default:
		return downStyle.Render("✕ down")
	}
}

func latencyCell(d time.Duration, ok bool) string {
	if !ok || d == 0 {
		return "—"
	}
	return formatLatency(d)
}

func formatLatency(d time.Duration) string {
	if d < time.Millisecond {
		return "<1ms"
	}
	return fmt.Sprintf("%dms", d.Milliseconds())
}

func lossCell(metricsLoss float64, ok bool) string {
	if !ok {
		return "—"
	}
	return fmt.Sprintf("%.0f%%", metricsLoss)
}

func Table(results []probe.Result) string {
	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(borderStyle).
		Headers("ROUTE", "TYPE", "STATUS", "LOSS", "MEDIAN", "JITTER", "DETAIL")

	for _, r := range results {
		detail := ""
		if !r.OK() {
			if r.Category != "" {
				detail = r.Category
			} else if r.ErrMsg != "" {
				if len(r.ErrMsg) > 30 {
					detail = r.ErrMsg[:30]
				} else {
					detail = r.ErrMsg
				}
			}
		} else if r.Metrics.Median > 0 {
			detail = dimStyle.Render(shortReason(r))
		}
		t.Row(
			r.Endpoint.Name,
			string(r.Endpoint.Type),
			statusCell(r.Status),
			lossCell(r.Metrics.LossPct, r.Metrics.Attempted > 0),
			latencyCell(r.Metrics.Median, r.Metrics.Succeeded > 0),
			latencyCell(r.Metrics.Jitter, r.Metrics.Succeeded > 1),
			detail,
		)
	}
	out := t.String() + "\n\n"
	if best, ok := probe.Best(results); ok {
		median := formatLatency(best.Metrics.Median)
		if best.Metrics.Succeeded == 0 {
			median = "—"
		}
		out += bestStyle.Render("best route: "+best.Endpoint.Name) + fmt.Sprintf(" (%s)", median)
		diag := diagnosis(results)
		if diag != "" {
			out += "\n" + dimStyle.Render(diag)
		}
	} else {
		out += downStyle.Render("all routes down")
		diag := diagnosis(results)
		if diag != "" {
			out += "\n" + dimStyle.Render(diag)
		}
	}
	return out
}

func shortReason(r probe.Result) string {
	if r.Status == probe.StatusDegraded {
		if r.Metrics.LossPct > 0 {
			return fmt.Sprintf("loss %.0f%%", r.Metrics.LossPct)
		}
		if r.Metrics.Jitter > 50*time.Millisecond {
			return "jitter"
		}
		return "slow"
	}
	return ""
}

func diagnosis(results []probe.Result) string {
	if len(results) == 0 {
		return ""
	}
	// find direct baseline if exists (type http/tcp/tls with name containing direct/baseline)
	var direct *probe.Result
	for i := range results {
		n := results[i].Endpoint.Name
		if n == "direct" || n == "direct-baseline" || n == "baseline" {
			direct = &results[i]
			break
		}
		if results[i].Endpoint.Type == "http" && (contains(n, "direct") || contains(n, "baseline") || contains(n, "cloudflare") || contains(n, "gstatic")) {
			direct = &results[i]
		}
	}
	directUp := direct != nil && direct.OK()
	allDown := true
	anyUp := false
	for _, r := range results {
		if r.OK() {
			allDown = false
			anyUp = true
			break
		}
	}
	// If direct is present, use it for diagnosis
	if direct != nil {
		if !directUp && allDown {
			return "hint: direct baseline + all proxies down — local internet/DNS may be down"
		}
		if directUp && allDown {
			return "hint: direct works but all proxy routes fail — proxy/VPS or control path likely issue"
		}
		if directUp {
			// check if TCP reachable but proxy fails? We don't have separate server reachability vs proxy health in same result currently.
			// Generic: some proxies down
			hasDown := false
			for _, r := range results {
				if r.Endpoint.Name == direct.Endpoint.Name {
					continue
				}
				if !r.OK() {
					hasDown = true
					break
				}
			}
			if hasDown && anyUp {
				return "hint: direct OK, some proxy routes degraded — check those transports"
			}
		}
	} else {
		if allDown {
			return "hint: all routes down — check internet and proxy config"
		}
	}
	return ""
}

func contains(s, sub string) bool {
	if len(s) < len(sub) {
		return false
	}
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
		// case-insensitive quick check
	}
	// fallback case-insensitive by lower
	ls := toLower(s)
	lsub := toLower(sub)
	for i := 0; i <= len(ls)-len(lsub); i++ {
		if ls[i:i+len(lsub)] == lsub {
			return true
		}
	}
	return false
}

func toLower(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}

type jsonResult struct {
	Name      string  `json:"name"`
	Type      string  `json:"type"`
	Status    string  `json:"status"`
	Category  string  `json:"category,omitempty"`
	Attempted int     `json:"attempted"`
	Succeeded int     `json:"succeeded"`
	Failed    int     `json:"failed"`
	LossPct   float64 `json:"loss_pct"`
	MinMs     int64   `json:"min_ms,omitempty"`
	MedianMs  int64   `json:"median_ms"`
	MaxMs     int64   `json:"max_ms,omitempty"`
	MeanMs    int64   `json:"mean_ms,omitempty"`
	JitterMs  int64   `json:"jitter_ms"`
	Priority  int     `json:"priority"`
	Error     string  `json:"error,omitempty"`
	Outbound  string  `json:"outbound,omitempty"`
}

type jsonDoc struct {
	SchemaVersion int          `json:"schema_version"`
	Timestamp     string       `json:"timestamp"`
	Results       []jsonResult `json:"results"`
	Best          string       `json:"best,omitempty"`
	Diagnosis     string       `json:"diagnosis,omitempty"`
}

func JSON(w io.Writer, results []probe.Result) error {
	now := time.Now().UTC().Format(time.RFC3339)
	doc := jsonDoc{
		SchemaVersion: 2,
		Timestamp:     now,
	}
	for _, r := range results {
		jr := jsonResult{
			Name:      r.Endpoint.Name,
			Type:      string(r.Endpoint.Type),
			Status:    string(r.Status),
			Category:  r.Category,
			Attempted: r.Metrics.Attempted,
			Succeeded: r.Metrics.Succeeded,
			Failed:    r.Metrics.Failed,
			LossPct:   r.Metrics.LossPct,
			MedianMs:  r.Metrics.Median.Milliseconds(),
			JitterMs:  r.Metrics.Jitter.Milliseconds(),
			Priority:  r.Endpoint.Priority,
			Error:     r.ErrMsg,
		}
		if r.Endpoint.Outbound != "" {
			jr.Outbound = r.Endpoint.Outbound
		}
		if r.Metrics.Succeeded > 0 {
			jr.MinMs = r.Metrics.Min.Milliseconds()
			jr.MaxMs = r.Metrics.Max.Milliseconds()
			jr.MeanMs = r.Metrics.Mean.Milliseconds()
		}
		// redact secrets - ErrMsg should already be categorized and not contain secrets, but ensure we don't leak controller token
		doc.Results = append(doc.Results, jr)
	}
	if best, ok := probe.Best(results); ok {
		doc.Best = best.Endpoint.Name
	}
	doc.Diagnosis = diagnosis(results)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

func NDJSONLine(results []probe.Result) ([]byte, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	doc := jsonDoc{
		SchemaVersion: 2,
		Timestamp:     now,
	}
	for _, r := range results {
		jr := jsonResult{
			Name:      r.Endpoint.Name,
			Type:      string(r.Endpoint.Type),
			Status:    string(r.Status),
			Category:  r.Category,
			Attempted: r.Metrics.Attempted,
			Succeeded: r.Metrics.Succeeded,
			Failed:    r.Metrics.Failed,
			LossPct:   r.Metrics.LossPct,
			MedianMs:  r.Metrics.Median.Milliseconds(),
			JitterMs:  r.Metrics.Jitter.Milliseconds(),
			Priority:  r.Endpoint.Priority,
			Error:     r.ErrMsg,
		}
		if r.Endpoint.Outbound != "" {
			jr.Outbound = r.Endpoint.Outbound
		}
		if r.Metrics.Succeeded > 0 {
			jr.MinMs = r.Metrics.Min.Milliseconds()
			jr.MaxMs = r.Metrics.Max.Milliseconds()
			jr.MeanMs = r.Metrics.Mean.Milliseconds()
		}
		doc.Results = append(doc.Results, jr)
	}
	if best, ok := probe.Best(results); ok {
		doc.Best = best.Endpoint.Name
	}
	doc.Diagnosis = diagnosis(results)
	return json.Marshal(doc)
}
