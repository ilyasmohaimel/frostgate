package report

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
	"github.com/ilyasmohaimel/frostgate/internal/config"
	"github.com/ilyasmohaimel/frostgate/internal/probe"
	"github.com/ilyasmohaimel/frostgate/internal/redact"
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
				safe := redact.String(r.ErrMsg)
				if len(safe) > 30 {
					detail = safe[:30]
				} else {
					detail = safe
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
	if probe.HasProxy(results) {
		if d, ok := probe.DirectBaseline(results); ok {
			lat := "—"
			if d.Metrics.Succeeded > 0 && d.Metrics.Median > 0 {
				lat = formatLatency(d.Metrics.Median)
			}
			out += dimStyle.Render("Direct baseline:") + " " + plainStatus(d.Status) + " " + lat + "\n"
		}
		if b, ok := probe.BestProxy(results); ok {
			median := formatLatency(b.Metrics.Median)
			if b.Metrics.Succeeded == 0 {
				median = "—"
			}
			out += bestStyle.Render("Best proxy: "+b.Endpoint.Name) + fmt.Sprintf(" (%s)", median)
		} else {
			out += dimStyle.Render("Best proxy: none configured")
		}
		diag := diagnosis(results)
		if diag != "" {
			out += "\n" + dimStyle.Render(diag)
		}
		return out
	}
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

func plainStatus(s probe.Status) string {
	switch s {
	case probe.StatusUp:
		return "UP"
	case probe.StatusDegraded:
		return "DEGRADED"
	default:
		return "DOWN"
	}
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
	proxyCount := 0
	proxyUp := 0
	proxyDown := 0
	allDown := true
	for _, r := range results {
		if r.OK() {
			allDown = false
		}
		if r.Endpoint.Type != config.ProbeSingBox {
			continue
		}
		proxyCount++
		if r.OK() {
			proxyUp++
		} else {
			proxyDown++
		}
	}
	// If direct is present, use it for diagnosis
	if direct != nil {
		if !directUp && proxyCount > 0 && proxyDown == proxyCount {
			return "hint: direct baseline + all proxies down; local internet/DNS may be down"
		}
		if directUp && proxyCount > 0 && proxyDown == proxyCount {
			return "hint: direct works but all proxy routes fail; proxy/VPS or control path likely issue"
		}
		if directUp && proxyDown > 0 && proxyUp > 0 {
			return "hint: direct OK, some proxy routes failed; check those transports"
		}
	} else {
		if allDown {
			return "hint: all routes down; check internet and proxy config"
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

type JSONResult struct {
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
	Direct        *JSONResult  `json:"direct,omitempty"`
	BestProxy     *JSONResult  `json:"best_proxy,omitempty"`
	Best          string       `json:"best,omitempty"`
	Results       []JSONResult `json:"results"`
	Diagnosis     string       `json:"diagnosis,omitempty"`
}

func ToJSONResult(r probe.Result) JSONResult {
	jr := JSONResult{
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
		Error:     redact.String(r.ErrMsg),
	}
	if r.Endpoint.Outbound != "" {
		jr.Outbound = r.Endpoint.Outbound
	}
	if r.Metrics.Succeeded > 0 {
		jr.MinMs = r.Metrics.Min.Milliseconds()
		jr.MaxMs = r.Metrics.Max.Milliseconds()
		jr.MeanMs = r.Metrics.Mean.Milliseconds()
	}
	return jr
}

func buildDoc(results []probe.Result, now string) jsonDoc {
	doc := jsonDoc{
		SchemaVersion: 3,
		Timestamp:     now,
	}
	for _, r := range results {
		doc.Results = append(doc.Results, ToJSONResult(r))
	}
	if d, ok := probe.DirectBaseline(results); ok {
		jr := ToJSONResult(d)
		doc.Direct = &jr
	}
	if b, ok := probe.BestProxy(results); ok {
		jr := ToJSONResult(b)
		doc.BestProxy = &jr
	}
	// Legacy field: overall best route of any type (unchanged semantics).
	if best, ok := probe.Best(results); ok {
		doc.Best = best.Endpoint.Name
	}
	doc.Diagnosis = diagnosis(results)
	return doc
}

func JSON(w io.Writer, results []probe.Result) error {
	now := time.Now().UTC().Format(time.RFC3339)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(buildDoc(results, now))
}

func NDJSONLine(results []probe.Result) ([]byte, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	return json.Marshal(buildDoc(results, now))
}
