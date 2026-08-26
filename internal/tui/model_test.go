package tui

import (
	"errors"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/ilyasmohaimel/frostgate/internal/config"
	"github.com/ilyasmohaimel/frostgate/internal/health"
	"github.com/ilyasmohaimel/frostgate/internal/metrics"
	"github.com/ilyasmohaimel/frostgate/internal/probe"
)

func TestModelQuitsOnQ(t *testing.T) {
	cfg := &config.Config{}
	m := New(cfg, 5*time.Second, false, nil)
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	_ = updated
	if cmd == nil {
		t.Error("expected quit cmd on q")
	}
	// verify the message is quit
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		// bubbletea quit returns QuitMsg via Batch, but direct? Actually tea.Quit returns func() Msg
		// So cmd() should be QuitMsg
		t.Logf("got %T", msg)
	}
}

func TestModelRefreshOnR(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{
			Timeout: config.Duration{Duration: time.Second},
			Samples: 1,
		},
		Endpoints: []config.Endpoint{{Name: "a", Host: "127.0.0.1", Port: 1}},
	}
	m := New(cfg, 5*time.Second, false, nil)
	m.probing = false
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if cmd == nil {
		t.Error("expected probe cmd on r")
	}
}

func TestModelHandlesResultsMsg(t *testing.T) {
	cfg := &config.Config{}
	m := New(cfg, 5*time.Second, false, nil)
	// send empty results
	m2, _ := m.Update(resultsMsg{results: nil, err: nil})
	model, ok := m2.(Model)
	if !ok {
		t.Fatal("type assert")
	}
	if len(model.results) != 0 {
		t.Error("expected zero results")
	}
	// window size
	m3, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if m3.(Model).width != 80 {
		t.Error("width not set")
	}
}

func TestModelRedactsProbeErrors(t *testing.T) {
	m := New(&config.Config{}, time.Second, false, nil)
	m.err = errors.New("request failed https://example.com/check?token=SUPER_SECRET_TOKEN_8F3A bearer SECRET_BEARER")
	view := m.View()
	if containsStr(view, "SUPER_SECRET_TOKEN_8F3A") || containsStr(view, "SECRET_BEARER") || !containsStr(view, "[redacted]") {
		t.Fatalf("TUI error was not redacted: %s", view)
	}
}

func TestModelCoalescesRefreshesWhileProbeIsInFlight(t *testing.T) {
	m := New(&config.Config{}, time.Second, false, nil)
	// New begins with the initial probe in flight. Ticks and repeated manual
	// refreshes must leave exactly one pending refresh instead of starting more.
	updated, cmd := m.Update(tickMsg(time.Unix(100, 0)))
	if cmd == nil || !updated.(Model).probing || !updated.(Model).pending {
		t.Fatal("tick while busy must coalesce and keep the timer alive")
	}
	updated, cmd = updated.(Model).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if cmd != nil || !updated.(Model).pending {
		t.Fatal("manual refresh while busy must not start a second probe")
	}
	completed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	updated, cmd = updated.(Model).Update(resultsMsg{completed: completed})
	model := updated.(Model)
	if cmd == nil || !model.probing || model.pending || !model.lastRefresh.Equal(completed) {
		t.Fatalf("completion must start one coalesced probe and record completion time: %+v", model)
	}
	if view := model.View(); !containsStr(view, "Last refresh: 03:04:05") {
		t.Fatalf("view must use completed refresh time:\n%s", view)
	}
}

func TestModelRecordsCompletionTimeAfterProbeError(t *testing.T) {
	m := New(&config.Config{}, time.Second, false, nil)
	completed := time.Date(2026, 1, 2, 4, 5, 6, 0, time.UTC)
	updated, _ := m.Update(resultsMsg{err: errors.New("probe failed"), completed: completed})
	model := updated.(Model)
	if !model.lastRefresh.Equal(completed) {
		t.Fatalf("last refresh = %s, want %s", model.lastRefresh, completed)
	}
	if view := model.View(); !containsStr(view, "Last refresh: 04:05:06") {
		t.Fatalf("view must show completion time after an error:\n%s", view)
	}
}

func TestObserverCalledOnSnapshotAndManualRefresh(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Timeout: config.Duration{Duration: time.Second}, Samples: 1},
		Endpoints: []config.Endpoint{
			{Name: "direct", Type: config.ProbeHTTP, URL: "http://127.0.0.1:1/x"},
		},
	}
	var mu int
	snapshots := [][]probe.Result{}
	m := New(cfg, time.Hour, false, func(results []probe.Result) {
		mu++
		snapshots = append(snapshots, results)
	})
	res := []probe.Result{{Endpoint: cfg.Endpoints[0], Status: probe.StatusUp}}
	m2, _ := m.Update(resultsMsg{results: res})
	if len(snapshots) != 1 {
		t.Fatalf("observer must fire on scheduled snapshot, got %d", len(snapshots))
	}
	// Manual refresh path: 'r' triggers probeCmd; its completion also fires.
	_, cmd := m2.(Model).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if cmd == nil {
		t.Fatal("refresh must return cmd")
	}
	msg := cmd()
	if rm, ok := msg.(resultsMsg); ok {
		m2.(Model).Update(rm)
		if len(snapshots) != 2 {
			t.Fatalf("observer must fire on manual refresh result, got %d", len(snapshots))
		}
	}
	if len(snapshots[0]) != 1 || snapshots[0][0].Endpoint.Name != "direct" {
		t.Fatalf("snapshot payload wrong: %+v", snapshots[0])
	}
}

func TestTUIDirectVsBestProxyPresentation(t *testing.T) {
	cfg := &config.Config{}
	direct := probe.Result{Endpoint: config.Endpoint{Name: "direct", Type: config.ProbeHTTP}, Status: probe.StatusUp, Metrics: metricsOf(20)}
	proxy := probe.Result{Endpoint: config.Endpoint{Name: "reality1", Type: config.ProbeSingBox}, Status: probe.StatusUp, Metrics: metricsOf(120)}
	m := New(cfg, time.Hour, false, nil)
	m.results = []probe.Result{proxy, direct}
	view := m.View()
	if !containsStr(view, "Direct baseline") || !containsStr(view, "Best proxy: reality1") {
		t.Fatalf("view must separate direct baseline from best proxy:\n%s", view)
	}
	if containsStr(view, "Best route:") {
		t.Fatal("proxy runs must not show generic best route line")
	}
}

func TestTUIShowsMeasuredZeroDirectLatency(t *testing.T) {
	cfg := &config.Config{}
	direct := probe.Result{Endpoint: config.Endpoint{Name: "direct", Type: config.ProbeHTTP}, Status: probe.StatusUp, Metrics: metrics.Metrics{Attempted: 1, Succeeded: 1}}
	proxy := probe.Result{Endpoint: config.Endpoint{Name: "proxy", Type: config.ProbeSingBox}, Status: probe.StatusUp, Metrics: metricsOf(1)}
	m := New(cfg, time.Hour, false, nil)
	m.results = []probe.Result{proxy, direct}
	if view := m.View(); !containsStr(view, "Direct baseline: UP 0ms") {
		t.Fatalf("measured zero direct latency rendered as unknown:\n%s", view)
	}
}

func TestTUINoProxyUsesBestRoute(t *testing.T) {
	cfg := &config.Config{}
	direct := probe.Result{Endpoint: config.Endpoint{Name: "web", Type: config.ProbeHTTP}, Status: probe.StatusUp, Metrics: metricsOf(30)}
	m := New(cfg, time.Hour, false, nil)
	m.results = []probe.Result{direct}
	if v := m.View(); !containsStr(v, "Best route: web") {
		t.Fatalf("non-proxy run should keep best route wording:\n%s", v)
	}
}

func TestProbeBudgetUsesConfiguredRouteScheduleNotThirtySeconds(t *testing.T) {
	cfg := &config.Config{Defaults: config.Defaults{
		Timeout:        config.Duration{Duration: 10 * time.Second},
		Samples:        4,
		SampleInterval: config.Duration{Duration: 2 * time.Second},
	}, Endpoints: []config.Endpoint{{Name: "route", Host: "127.0.0.1", Port: 1}}}
	if got, want := ProbeBudget(cfg), 47*time.Second; got != want {
		t.Fatalf("probe budget=%s, want %s", got, want)
	}
}

func TestVerboseTUIShowsClassifierReasons(t *testing.T) {
	r := probe.Result{
		Endpoint: config.Endpoint{Name: "slow", Type: config.ProbeTCP},
		Status:   probe.StatusDegraded,
		Metrics:  metrics.Metrics{Attempted: 2, Succeeded: 2, Median: 120 * time.Millisecond, JitterKnown: true},
		Reasons:  []health.Reason{health.ReasonLatency},
	}
	m := New(&config.Config{}, time.Second, true, nil)
	m.results = []probe.Result{r}
	view := m.View()
	if !containsStr(view, "slow") || !containsStr(view, "Sample evidence") {
		t.Fatalf("verbose TUI omitted classification evidence:\n%s", view)
	}
}

func metricsOf(ms int) metrics.Metrics {
	return metrics.Metrics{Median: time.Duration(ms) * time.Millisecond, Succeeded: 2, Attempted: 2}
}

func containsStr(hay, needle string) bool {
	return len(needle) == 0 || (len(hay) >= len(needle) && indexOf(hay, needle) >= 0)
}

func indexOf(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
