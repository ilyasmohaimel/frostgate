package tui

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/ilyasmohaimel/frostgate/internal/config"
	"github.com/ilyasmohaimel/frostgate/internal/probe"
	"github.com/ilyasmohaimel/frostgate/internal/redact"
	"github.com/ilyasmohaimel/frostgate/internal/report"
)

type Model struct {
	cfg         *config.Config
	results     []probe.Result
	err         error
	interval    time.Duration
	width       int
	height      int
	lastTick    time.Time
	lastRefresh time.Time
	probing     bool
	pending     bool
	quitting    bool
	verbose     bool
	// onSnapshot is invoked after every completed probe snapshot (scheduled
	// tick or manual refresh). The application layer uses it to feed the
	// history recorder; the TUI itself never touches storage.
	onSnapshot func([]probe.Result)
}

type tickMsg time.Time
type resultsMsg struct {
	results   []probe.Result
	err       error
	completed time.Time
}

var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("63"))
	subStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	helpStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
)

// New builds the TUI model. onSnapshot may be nil.
func New(cfg *config.Config, interval time.Duration, verbose bool, onSnapshot func([]probe.Result)) Model {
	return Model{
		cfg:        cfg,
		interval:   interval,
		verbose:    verbose,
		lastTick:   time.Now(),
		probing:    true,
		onSnapshot: onSnapshot,
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.probeCmd(), m.tickCmd())
}

func (m Model) probeCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		results := probe.Run(ctx, m.cfg)
		return resultsMsg{results: results, completed: time.Now()}
	}
}

func (m Model) tickCmd() tea.Cmd {
	return tea.Tick(m.interval, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			m.quitting = true
			return m, tea.Quit
		case "r":
			if m.probing {
				m.pending = true
				return m, nil
			}
			m.probing = true
			return m, m.probeCmd()
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case tickMsg:
		m.lastTick = time.Time(msg)
		if m.probing {
			m.pending = true
			return m, m.tickCmd()
		}
		m.probing = true
		return m, tea.Batch(m.probeCmd(), m.tickCmd())
	case resultsMsg:
		m.probing = false
		m.lastRefresh = msg.completed
		if m.lastRefresh.IsZero() {
			m.lastRefresh = time.Now()
		}
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.results = msg.results
			m.err = nil
			if m.onSnapshot != nil {
				m.onSnapshot(msg.results)
			}
		}
		if m.pending {
			m.pending = false
			m.probing = true
			return m, m.probeCmd()
		}
		return m, nil
	}
	return m, nil
}

func (m Model) View() string {
	if m.quitting {
		return ""
	}
	var b string
	b += titleStyle.Render("FROSTGATE") + "  " + subStyle.Render("Network Route Sentinel") + "\n"
	if m.width > 0 && m.width < 60 {
		b += subStyle.Render("(narrow terminal)") + "\n"
	}
	b += "\n"
	if m.err != nil {
		b += lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Render("error: "+redact.Error(m.err)) + "\n\n"
	}
	if len(m.results) == 0 {
		b += subStyle.Render("probing...") + "\n"
	} else {
		b += renderTable(m.results, m.width)
		b += "\n"
		if probe.HasProxy(m.results) {
			if d, ok := probe.DirectBaseline(m.results); ok {
				lat := "—"
				if d.Metrics.Succeeded > 0 && d.Metrics.Median > 0 {
					lat = formatDur(d.Metrics.Median)
				}
				state := "DOWN"
				switch d.Status {
				case probe.StatusUp:
					state = "UP"
				case probe.StatusDegraded:
					state = "DEGRADED"
				}
				b += subStyle.Render(fmt.Sprintf("Direct baseline: %s %s", state, lat)) + "\n"
			}
			if best, ok := probe.BestProxy(m.results); ok {
				b += lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42")).Render("Best proxy: "+best.Endpoint.Name) + subStyle.Render(fmt.Sprintf("  median %s  loss %.0f%%", formatDur(best.Metrics.Median), best.Metrics.LossPct)) + "\n"
			} else {
				b += lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Render("Best proxy: none usable") + "\n"
			}
		} else if best, ok := probe.Best(m.results); ok {
			b += lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42")).Render("Best route: "+best.Endpoint.Name) + subStyle.Render(fmt.Sprintf("  median %s  loss %.0f%%", formatDur(best.Metrics.Median), best.Metrics.LossPct)) + "\n"
		} else {
			b += lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Render("all routes down") + "\n"
		}
	}
	b += "\n"
	lastRefresh := "never"
	if !m.lastRefresh.IsZero() {
		lastRefresh = m.lastRefresh.Format("15:04:05")
	}
	b += subStyle.Render(fmt.Sprintf("Last refresh: %s", lastRefresh)) + "  " + helpStyle.Render("q quit  r refresh") + "\n"
	if m.verbose && len(m.results) > 0 {
		// show details: each route's samples?
	}
	return b
}

func renderTable(results []probe.Result, width int) string {
	// Simple manual table for TUI to avoid lipgloss table border overhead and for deterministic width handling
	// Use report.Table but strip? Instead we craft compact
	header := fmt.Sprintf("%-20s %-9s %4s %7s %7s  %s", "ROUTE", "STATE", "LOSS", "MEDIAN", "JITTER", "DETAIL")
	lines := []string{subStyle.Render(header)}
	for _, r := range results {
		state := string(r.Status)
		switch r.Status {
		case probe.StatusUp:
			state = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Render("HEALTHY")
		case probe.StatusDegraded:
			state = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render("DEGRADED")
		case probe.StatusDown:
			state = lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Render("DOWN")
		}
		loss := "—"
		if r.Metrics.Attempted > 0 {
			loss = fmt.Sprintf("%.0f%%", r.Metrics.LossPct)
		}
		median := "—"
		if r.Metrics.Succeeded > 0 {
			median = formatDur(r.Metrics.Median)
		}
		jitter := "—"
		if r.Metrics.Succeeded > 1 {
			jitter = formatDur(r.Metrics.Jitter)
		}
		detail := ""
		if !r.OK() {
			if r.Category != "" {
				detail = r.Category
			} else if r.ErrMsg != "" {
				detail = truncate(redact.String(r.ErrMsg), 24)
			}
		}
		line := fmt.Sprintf("%-20s %-9s %4s %7s %7s  %s", truncate(r.Endpoint.Name, 20), state, loss, median, jitter, detail)
		lines = append(lines, line)
	}
	// If narrow, we may truncate; but keep simple
	out := ""
	for i, l := range lines {
		out += l
		if i < len(lines)-1 {
			out += "\n"
		}
	}
	// If we want to use report.Table for rich borders, fallback:
	if width < 0 {
		return report.Table(results)
	}
	return out
}

func formatDur(d time.Duration) string {
	if d < time.Millisecond {
		return "<1ms"
	}
	return fmt.Sprintf("%dms", d.Milliseconds())
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
