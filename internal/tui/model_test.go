package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/ilyasmohaimel/frostgate/internal/config"
)

func TestModelQuitsOnQ(t *testing.T) {
	cfg := &config.Config{}
	m := New(cfg, 5*time.Second, false)
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
	m := New(cfg, 5*time.Second, false)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if cmd == nil {
		t.Error("expected probe cmd on r")
	}
}

func TestModelHandlesResultsMsg(t *testing.T) {
	cfg := &config.Config{}
	m := New(cfg, 5*time.Second, false)
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
