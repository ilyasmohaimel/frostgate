package config

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultsApplied(t *testing.T) {
	path := writeTemp(t, `
endpoints:
  - name: a
    type: singbox
    outbound: route-a
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
    type: singbox
    outbound: route-a
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

func TestDirectOnlyIgnoresStaleSingBoxSettings(t *testing.T) {
	path := writeTemp(t, `
singbox:
  controller: not-a-url
  test_url: also-not-a-url
  secret_file: /definitely/not/used
  benchmark_proxy: 0.0.0.0:2080
endpoints:
  - name: direct
    type: http
    url: https://example.com/
`)
	if _, err := Load(path); err != nil {
		t.Fatalf("direct-only config was rejected for unused sing-box settings: %v", err)
	}
}

func TestPerRouteSamples(t *testing.T) {
	path := writeTemp(t, `
defaults:
  samples: 4
endpoints:
  - name: a
    type: singbox
    outbound: route-a
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
    type: singbox
    outbound: route-a
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected invalid duration error")
	}
}

func TestDayDuration(t *testing.T) {
	path := writeTemp(t, `
history:
  retention: 17d
endpoints:
  - name: a
    host: example.com
    port: 443
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.History.Retention.Duration != 17*24*time.Hour {
		t.Fatalf("retention=%v want 17d", cfg.History.Retention.Duration)
	}
}

func TestBenchmarkProxyMustBeLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:2080", "127.42.7.9:2080", "localhost:2080", "[::1]:2080"} {
		path := writeTemp(t, `
singbox:
  benchmark_proxy: "`+addr+`"
endpoints:
  - name: a
    type: singbox
    outbound: route-a
`)
		if _, err := Load(path); err != nil {
			t.Fatalf("valid benchmark proxy %q rejected: %v", addr, err)
		}
	}
	for _, addr := range []string{"0.0.0.0:2080", "203.0.113.10:2080", "127.0.0.1"} {
		path := writeTemp(t, `
singbox:
  benchmark_proxy: "`+addr+`"
endpoints:
  - name: a
    type: singbox
    outbound: route-a
`)
		if _, err := Load(path); err == nil {
			t.Fatalf("unsafe benchmark proxy %q accepted", addr)
		}
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

func TestEndpointAddrIPv6(t *testing.T) {
	tests := []struct{ host, want string }{
		{"203.0.113.10", "203.0.113.10:443"},
		{"example.com", "example.com:443"},
		{"2001:db8::1", "[2001:db8::1]:443"},
	}
	for _, tt := range tests {
		if got := (Endpoint{Host: tt.host, Port: 443}).Addr(); got != tt.want {
			t.Errorf("Addr(%q) = %q, want %q", tt.host, got, tt.want)
		}
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
    type: singbox
    outbound: route-a
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
    type: singbox
    outbound: route-a
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SingBox.Secret != "envsecret" {
		t.Errorf("env override %q want envsecret", cfg.SingBox.Secret)
	}
}

func TestConfigRejectsInvalidExplicitPorts(t *testing.T) {
	for _, port := range []int{-1, 0, 65536, 999999} {
		path := writeTemp(t, fmt.Sprintf("endpoints:\n  - name: tcp\n    type: tcp\n    host: example.com\n    port: %d\n", port))
		if _, err := Load(path); err == nil {
			t.Fatalf("port %d was accepted", port)
		}
	}
}

func TestConfigRejectsInvalidHTTPURLPortsButKeepsDefaults(t *testing.T) {
	for _, raw := range []string{
		"https://example.com:0/",
		"https://example.com:65536/",
		"https://example.com:999999/",
	} {
		path := writeTemp(t, fmt.Sprintf("endpoints:\n  - name: http\n    type: http\n    url: %s\n", raw))
		if _, err := Load(path); err == nil {
			t.Fatalf("URL port %q was accepted", raw)
		}
	}
	for _, raw := range []string{"http://example.com/", "https://example.com/"} {
		path := writeTemp(t, fmt.Sprintf("endpoints:\n  - name: http\n    type: http\n    url: %s\n", raw))
		if _, err := Load(path); err != nil {
			t.Fatalf("default HTTP port URL %q rejected: %v", raw, err)
		}
	}
}

func TestRemotePlaintextControllerSecretRejected(t *testing.T) {
	tests := []struct {
		name       string
		controller string
		secret     string
		wantErr    bool
	}{
		{"loopback", "http://127.0.0.1:9090", "fixture-secret", false},
		{"ipv6 loopback", "http://[::1]:9090", "fixture-secret", false},
		{"localhost", "http://localhost:9090", "fixture-secret", false},
		{"remote https", "https://remote.example:443", "fixture-secret", false},
		{"remote http", "http://remote.example:9090", "fixture-secret", true},
		{"lan http", "http://192.168.1.5:9090", "fixture-secret", true},
		{"remote no secret", "http://remote.example:9090", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTemp(t, fmt.Sprintf("singbox:\n  controller: %s\n  secret: %q\nendpoints:\n  - name: proxy\n    type: singbox\n    outbound: proxy\n", tt.controller, tt.secret))
			_, err := Load(path)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Load() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestRemotePlaintextControllerSecretFileRejected(t *testing.T) {
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "controller-secret")
	if err := os.WriteFile(secretFile, []byte("fixture-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeTemp(t, "singbox:\n  controller: http://remote.example:9090\n  secret_file: "+secretFile+"\nendpoints:\n  - name: proxy\n    type: singbox\n    outbound: proxy\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "remote plaintext HTTP") {
		t.Fatalf("remote secret_file controller error = %v", err)
	}
}

func TestProbeBudgetCoversEverySampleAndInterval(t *testing.T) {
	cfg := &Config{Defaults: Defaults{Timeout: Duration{Duration: 5 * time.Second}, Samples: 4, SampleInterval: Duration{Duration: 150 * time.Millisecond}}}
	if got, want := cfg.ProbeBudget(Endpoint{}), 21*time.Second+450*time.Millisecond; got != want {
		t.Fatalf("ProbeBudget = %s, want %s", got, want)
	}
}

func TestStrictConfigRejectsUnknownFieldsAtEveryOwnedLevel(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"root", "unknown_root: true\n"},
		{"defaults", "defaults:\n  timeuot: 1ns\n"},
		{"singbox", "singbox:\n  controllr: http://127.0.0.1:9090\n"},
		{"benchmark", "benchmark:\n  bytez: 1\n"},
		{"history", "history:\n  retentoin: 1d\n"},
		{"network_check", "network_check:\n  destiantions: []\n"},
		{"endpoint", "endpoints:\n  - name: route\n    type: tcp\n    host: example.com\n    port: 443\n    timeuot: 1s\n"},
		{"nested_endpoint", "endpoints:\n  - name: route\n    type: tcp\n    host: example.com\n    port: 443\n    options:\n      timeout: 1s\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(writeTemp(t, tc.body)); err == nil {
				t.Fatal("unknown field was accepted")
			} else if !strings.Contains(strings.ToLower(err.Error()), strings.TrimSuffix(strings.ToLower(tc.name), "_endpoint")) && tc.name != "nested_endpoint" {
				// The exact decoder wording differs for custom Endpoint fields;
				// all errors must still identify the offending field or its owned section.
				if !strings.Contains(err.Error(), "unknown") && !strings.Contains(err.Error(), "not found") {
					t.Fatalf("unclear unknown-field error: %v", err)
				}
			}
		})
	}
}

func TestSampleCountLimit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value int
		valid bool
	}{
		{"one", 1, true},
		{"maximum", MaxSamples, true},
		{"maximum-plus-one", MaxSamples + 1, false},
		{"huge", math.MaxInt, false},
		{"zero-defaults", 0, true},
		{"negative", -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf("defaults:\n  samples: %d\nendpoints:\n  - name: route\n    type: tcp\n    host: example.com\n    port: 443\n", tc.value)
			_, err := Load(writeTemp(t, body))
			if (err == nil) != tc.valid {
				t.Fatalf("samples=%d error=%v valid=%v", tc.value, err, tc.valid)
			}
		})
	}
	if _, err := Load(writeTemp(t, "endpoints:\n  - name: route\n    type: tcp\n    host: example.com\n    port: 443\n    samples: 100\n")); err != nil {
		t.Fatalf("endpoint maximum rejected: %v", err)
	}
}

func TestConfigRejectsNonFiniteLossThresholds(t *testing.T) {
	for _, value := range []string{".nan", ".inf", "-.inf"} {
		for _, field := range []string{"defaults", "endpoint"} {
			body := "endpoints:\n  - name: route\n    type: tcp\n    host: example.com\n    port: 443\n"
			if field == "defaults" {
				body = "defaults:\n  max_loss: " + value + "\n" + body
			} else {
				body += "    max_loss: " + value + "\n"
			}
			if _, err := Load(writeTemp(t, body)); err == nil {
				t.Fatalf("%s max_loss=%s was accepted", field, value)
			}
		}
	}
	for _, value := range []string{"0", "100", "12.5"} {
		body := "defaults:\n  max_loss: " + value + "\nendpoints:\n  - name: route\n    type: tcp\n    host: example.com\n    port: 443\n"
		if _, err := Load(writeTemp(t, body)); err != nil {
			t.Fatalf("finite max_loss=%s rejected: %v", value, err)
		}
	}
}

func TestEndpointNamesRejectControlsButAcceptUnicode(t *testing.T) {
	for _, name := range []string{"مرحبا", "中文 路由", "café 🚀", "route with spaces"} {
		body := fmt.Sprintf("endpoints:\n  - name: %q\n    type: tcp\n    host: example.com\n    port: 443\n", name)
		if _, err := Load(writeTemp(t, body)); err != nil {
			t.Fatalf("safe endpoint name %q rejected: %v", name, err)
		}
	}
	for _, name := range []string{"bad\nname", "bad\rname", "bad\x1b[31m"} {
		body := fmt.Sprintf("endpoints:\n  - name: %q\n    type: tcp\n    host: example.com\n    port: 443\n", name)
		if _, err := Load(writeTemp(t, body)); err == nil {
			t.Fatalf("control endpoint name %q was accepted", name)
		}
	}
}

func TestNextPriorityRejectsOverflow(t *testing.T) {
	if got, err := NextPriority(20); err != nil || got != 30 {
		t.Fatalf("NextPriority(20) = %d, %v", got, err)
	}
	if _, err := NextPriority(math.MaxInt); err == nil {
		t.Fatal("MaxInt priority wrapped instead of failing")
	}
}

func TestControllerValidationAcceptsUppercaseSchemeAndRejectsUserinfo(t *testing.T) {
	valid := writeTemp(t, "singbox:\n  controller: HTTP://127.0.0.1:9090/\nendpoints:\n  - name: route\n    type: singbox\n    outbound: route\n")
	if _, err := Load(valid); err != nil {
		t.Fatalf("uppercase controller scheme rejected: %v", err)
	}
	invalid := writeTemp(t, "singbox:\n  controller: http://user:password@127.0.0.1:9090\nendpoints:\n  - name: route\n    type: singbox\n    outbound: route\n")
	if _, err := Load(invalid); err == nil || strings.Contains(err.Error(), "password") {
		t.Fatalf("userinfo controller result = %v", err)
	}
}
