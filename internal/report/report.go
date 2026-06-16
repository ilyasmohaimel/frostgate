// Package report renders probe results as a colored table or JSON.
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
	upStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))  // green
	degradeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("214")) // amber
	downStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("203")) // red
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("63"))
	borderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	bestStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42"))
)

// Table renders results as a ranked, colored table followed by a summary line.
func Table(results []probe.Result) string {
	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(borderStyle).
		Headers("ROUTE", "TYPE", "STATUS", "LATENCY")

	for _, r := range results {
		t.Row(
			r.Endpoint.Name,
			string(r.Endpoint.Type),
			statusCell(r.Status),
			latencyCell(r),
		)
	}

	out := t.String() + "\n\n"
	if best, ok := probe.Best(results); ok {
		out += bestStyle.Render("best route: "+best.Endpoint.Name) +
			fmt.Sprintf(" (%s)", formatLatency(best.Latency))
	} else {
		out += downStyle.Render("all routes down")
	}
	return out
}

func statusCell(s probe.Status) string {
	switch s {
	case probe.StatusUp:
		return upStyle.Render("● up")
	case probe.StatusDegraded:
		return degradeStyle.Render("● slow")
	default:
		return downStyle.Render("✕ down")
	}
}

func latencyCell(r probe.Result) string {
	if !r.OK() {
		return "—"
	}
	return formatLatency(r.Latency)
}

func formatLatency(d time.Duration) string {
	if d < time.Millisecond {
		return "<1ms"
	}
	return fmt.Sprintf("%dms", d.Milliseconds())
}

// jsonResult is the stable machine-readable shape (decoupled from internals).
type jsonResult struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	LatencyMs int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// JSON writes results as an indented JSON document.
func JSON(w io.Writer, results []probe.Result) error {
	doc := struct {
		Results []jsonResult `json:"results"`
		Best    string       `json:"best,omitempty"`
	}{}

	for _, r := range results {
		doc.Results = append(doc.Results, jsonResult{
			Name:      r.Endpoint.Name,
			Type:      string(r.Endpoint.Type),
			Status:    string(r.Status),
			LatencyMs: r.Latency.Milliseconds(),
			Error:     r.ErrMsg,
		})
	}
	if best, ok := probe.Best(results); ok {
		doc.Best = best.Endpoint.Name
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}
