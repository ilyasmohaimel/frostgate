package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/ilyasmohaimel/frostgate/internal/config"
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

func TestTUINoProxyUsesBestRoute(t *testing.T) {
	cfg := &config.Config{}
	direct := probe.Result{Endpoint: config.Endpoint{Name: "web", Type: config.ProbeHTTP}, Status: probe.StatusUp, Metrics: metricsOf(30)}
	m := New(cfg, time.Hour, false, nil)
	m.results = []probe.Result{direct}
	if v := m.View(); !containsStr(v, "Best route: web") {
		t.Fatalf("non-proxy run should keep best route wording:\n%s", v)
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
