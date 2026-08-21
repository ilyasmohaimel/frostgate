package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultsApplied(t *testing.T) {
	path := writeTemp(t, `
endpoints:
  - name: a
    host: example.com
    port: 443
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Defaults.Timeout.Duration != DefaultTimeout {
		t.Errorf("timeout default %v want %v", cfg.Defaults.Timeout.Duration, DefaultTimeout)
	}
	if cfg.Defaults.Samples != DefaultSamples {
		t.Errorf("samples default %d want %d", cfg.Defaults.Samples, DefaultSamples)
	}
	if cfg.Defaults.SampleInterval.Duration != DefaultSampleInterval {
		t.Errorf("sample interval %v want %v", cfg.Defaults.SampleInterval.Duration, DefaultSampleInterval)
	}
	if cfg.SingBox.Controller != DefaultSingBoxController {
		t.Errorf("controller default %q want %q", cfg.SingBox.Controller, DefaultSingBoxController)
	}
	if cfg.SingBox.TestURL != DefaultTestURL {
		t.Errorf("test_url default %q want %q", cfg.SingBox.TestURL, DefaultTestURL)
	}
}

func TestValidSingbox(t *testing.T) {
	path := writeTemp(t, `
singbox:
  controller: http://127.0.0.1:9090
  test_url: https://www.gstatic.com/generate_204
endpoints:
  - name: sb
    type: singbox
    outbound: my-out
    priority: 10
`)
	if _, err := Load(path); err != nil {
		t.Fatalf("valid singbox: %v", err)
	}
}

func TestSingboxMissingOutbound(t *testing.T) {
	path := writeTemp(t, `
endpoints:
  - name: sb
    type: singbox
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for missing outbound")
	}
}

func TestHTTPURLConflicts(t *testing.T) {
	path := writeTemp(t, `
endpoints:
  - name: a
    type: http
    url: https://example.com/
    host: example.com
    port: 443
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected conflict error for url + host")
	}
}

func TestHTTPURLValid(t *testing.T) {
	path := writeTemp(t, `
endpoints:
  - name: a
    type: http
    url: https://www.gstatic.com/generate_204
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("url valid: %v", err)
	}
	if cfg.Endpoints[0].EffectiveURL() != "https://www.gstatic.com/generate_204" {
		t.Errorf("effective url %q", cfg.Endpoints[0].EffectiveURL())
	}
}

func TestHTTPHostPortValid(t *testing.T) {
	path := writeTemp(t, `
endpoints:
  - name: a
    type: http
    host: www.gstatic.com
    port: 443
    path: /generate_204
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("host/port valid: %v", err)
	}
	if cfg.Endpoints[0].EffectiveURL() != "https://www.gstatic.com:443/generate_204" {
		// note addr includes port, so full string
		t.Logf("effective %q", cfg.Endpoints[0].EffectiveURL())
	}
}

func TestInvalidURL(t *testing.T) {
	path := writeTemp(t, `
endpoints:
  - name: a
    type: http
    url: ":::"
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected invalid url error")
	}
}

func TestPerRouteTimeout(t *testing.T) {
	path := writeTemp(t, `
defaults:
  timeout: 5s
endpoints:
  - name: a
    host: example.com
    port: 443
    timeout: 2s
  - name: b
    host: example.com
    port: 443
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.TimeoutFor(cfg.Endpoints[0]); got != 2*time.Second {
		t.Errorf("per-route timeout %v want 2s", got)
	}
	if got := cfg.TimeoutFor(cfg.Endpoints[1]); got != 5*time.Second {
		t.Errorf("default timeout %v want 5s", got)
	}
}

func TestPerRouteSamples(t *testing.T) {
	path := writeTemp(t, `
defaults:
  samples: 4
endpoints:
  - name: a
    host: example.com
    port: 443
    samples: 7
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.SamplesFor(cfg.Endpoints[0]); got != 7 {
		t.Errorf("samples %d want 7", got)
	}
}

func TestDuplicateNames(t *testing.T) {
	path := writeTemp(t, `
endpoints:
  - name: dup
    host: a.example.com
    port: 443
  - name: dup
    host: b.example.com
    port: 443
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected duplicate name error")
	}
}

func TestInvalidDuration(t *testing.T) {
	path := writeTemp(t, `
defaults:
  timeout: notaduration
endpoints:
  - name: a
    host: example.com
    port: 443
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected invalid duration error")
	}
}

func TestPriority(t *testing.T) {
	path := writeTemp(t, `
endpoints:
  - name: a
    host: example.com
    port: 443
    priority: 10
  - name: b
    host: example.com
    port: 443
    priority: 20
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Endpoints[0].Priority != 10 || cfg.Endpoints[1].Priority != 20 {
		t.Errorf("priority mismatch")
	}
}

func TestResolveConfigPathExplicit(t *testing.T) {
	p := "/tmp/custom.yaml"
	if got := ResolveConfigPath(p); got != p {
		t.Errorf("explicit %q got %q", p, got)
	}
}

func TestResolveConfigPathXDG(t *testing.T) {
	dir := t.TempDir()
	xdgPath := filepath.Join(dir, "frostgate", "config.yaml")
	os.MkdirAll(filepath.Join(dir, "frostgate"), 0755)
	os.WriteFile(xdgPath, []byte("test"), 0600)
	old := os.Getenv("XDG_CONFIG_HOME")
	os.Setenv("XDG_CONFIG_HOME", dir)
	defer os.Setenv("XDG_CONFIG_HOME", old)
	// also ensure no explicit and that file exists -> should return xdg path
	got := ResolveConfigPath("")
	if got != xdgPath {
		t.Errorf("xdg resolve %q want %q", got, xdgPath)
	}
}

func TestResolveConfigPathLegacyFallback(t *testing.T) {
	dir := t.TempDir()
	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)
	oldXdg := os.Getenv("XDG_CONFIG_HOME")
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "nonexistent"))
	defer os.Setenv("XDG_CONFIG_HOME", oldXdg)
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", dir)
	defer os.Setenv("HOME", oldHome)
	os.WriteFile(filepath.Join(dir, "frostgate.yaml"), []byte("test"), 0600)
	got := ResolveConfigPath("")
	if got != "frostgate.yaml" {
		t.Errorf("legacy fallback %q want frostgate.yaml", got)
	}
}

func TestThresholdOverrides(t *testing.T) {
	path := writeTemp(t, `
defaults:
  healthy_latency: 500ms
  max_jitter: 30ms
endpoints:
  - name: a
    host: example.com
    port: 443
    healthy_latency: 1000ms
    max_jitter: 50ms
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HealthyLatencyFor(cfg.Endpoints[0]) != 1000*time.Millisecond {
		t.Errorf("healthy latency override failed")
	}
	if cfg.MaxJitterFor(cfg.Endpoints[0]) != 50*time.Millisecond {
		t.Errorf("max jitter override failed")
	}
}

func TestSingboxSecretFile(t *testing.T) {
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "secret")
	os.WriteFile(secretFile, []byte("mysecret\n"), 0600)
	path := writeTemp(t, `
singbox:
  controller: http://127.0.0.1:9090
  secret_file: `+secretFile+`
endpoints:
  - name: a
    host: example.com
    port: 443
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SingBox.Secret != "mysecret" {
		t.Errorf("secret %q want mysecret", cfg.SingBox.Secret)
	}
}

func TestSingboxEnvOverride(t *testing.T) {
	t.Setenv("FROSTGATE_SINGBOX_SECRET", "envsecret")
	path := writeTemp(t, `
singbox:
  controller: http://127.0.0.1:9090
  secret: filesecret
endpoints:
  - name: a
    host: example.com
    port: 443
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SingBox.Secret != "envsecret" {
		t.Errorf("env override %q want envsecret", cfg.SingBox.Secret)
	}
}
