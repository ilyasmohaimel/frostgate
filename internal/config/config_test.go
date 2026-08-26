package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "frostgate.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

func TestLoadDefaultsAndDuration(t *testing.T) {
	path := writeTemp(t, `
defaults:
  timeout: 750ms
endpoints:
  - name: a
    host: example.com
    port: 443
    type: tls
  - name: b           # no type -> should default to tcp
    host: 10.0.0.1
    port: 80
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Defaults.Timeout.Duration != 750*time.Millisecond {
		t.Errorf("timeout = %v, want 750ms", cfg.Defaults.Timeout.Duration)
	}
	if cfg.Endpoints[1].Type != ProbeTCP {
		t.Errorf("missing type defaulted to %q, want tcp", cfg.Endpoints[1].Type)
	}
	if got := cfg.Endpoints[0].ServerName(); got != "example.com" {
		t.Errorf("ServerName() = %q, want example.com", got)
	}
}

func TestLoadRejectsOverflowingDayDuration(t *testing.T) {
	path := writeTemp(t, `
defaults:
  timeout: 999999999999d
endpoints:
  - name: a
    host: example.com
    port: 443
`)
	if _, err := Load(path); err == nil {
		t.Fatal("overflowing day duration was accepted")
	}
}

func TestLoadRejectsUnknownType(t *testing.T) {
	path := writeTemp(t, `
endpoints:
  - name: bad
    host: x
    port: 1
    type: carrier-pigeon
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for unknown probe type, got nil")
	}
}

func TestLoadRejectsEmpty(t *testing.T) {
	path := writeTemp(t, "defaults:\n  timeout: 1s\n")
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for config with no endpoints, got nil")
	}
}
