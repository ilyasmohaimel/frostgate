package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ilyasmohaimel/frostgate/internal/benchmark"
)

type Duration struct {
	time.Duration
}

func (d Duration) MarshalYAML() (interface{}, error) {
	return d.String(), nil
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		// try numeric?
		var n int64
		if err2 := value.Decode(&n); err2 == nil {
			d.Duration = time.Duration(n)
			return nil
		}
		return err
	}
	parsed, err := parseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = parsed
	return nil
}

func parseDuration(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		days, err := strconv.ParseInt(strings.TrimSuffix(s, "d"), 10, 64)
		if err != nil {
			return 0, err
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

type ProbeType string

const (
	ProbeTCP     ProbeType = "tcp"
	ProbeTLS     ProbeType = "tls"
	ProbeHTTP    ProbeType = "http"
	ProbeSingBox ProbeType = "singbox"
)

type Defaults struct {
	Timeout         Duration `yaml:"timeout"`
	Samples         int      `yaml:"samples"`
	SampleInterval  Duration `yaml:"sample_interval"`
	HealthyLatency  Duration `yaml:"healthy_latency"`
	DegradedLatency Duration `yaml:"degraded_latency"`
	MaxLoss         *float64 `yaml:"max_loss"`
	MaxJitter       Duration `yaml:"max_jitter"`
	Priority        *int     `yaml:"priority"`
}

type SingBoxConfig struct {
	Controller     string `yaml:"controller"`
	Secret         string `yaml:"secret,omitempty"`
	SecretFile     string `yaml:"secret_file,omitempty"`
	TestURL        string `yaml:"test_url,omitempty"`
	BenchmarkProxy string `yaml:"benchmark_proxy,omitempty"`
}

type Endpoint struct {
	Name           string    `yaml:"name"`
	Type           ProbeType `yaml:"type"`
	Host           string    `yaml:"host,omitempty"`
	Port           int       `yaml:"port,omitempty"`
	SNI            string    `yaml:"sni,omitempty"`
	Path           string    `yaml:"path,omitempty"`
	URL            string    `yaml:"url,omitempty"`
	Outbound       string    `yaml:"outbound,omitempty"`
	Priority       int       `yaml:"priority"`
	Timeout        Duration  `yaml:"timeout,omitempty"`
	Samples        int       `yaml:"samples,omitempty"`
	SampleInterval Duration  `yaml:"sample_interval,omitempty"`
	HealthyLatency Duration  `yaml:"healthy_latency,omitempty"`
	MaxJitter      Duration  `yaml:"max_jitter,omitempty"`
	MaxLoss        *float64  `yaml:"max_loss,omitempty"`
	ExpectedStatus int       `yaml:"expected_status,omitempty"`
}

func (e Endpoint) Addr() string {
	return fmt.Sprintf("%s:%d", e.Host, e.Port)
}

func (e Endpoint) ServerName() string {
	if e.SNI != "" {
		return e.SNI
	}
	return e.Host
}

func (e Endpoint) EffectiveURL() string {
	if e.URL != "" {
		return e.URL
	}
	if e.Host == "" {
		return ""
	}
	path := e.Path
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return fmt.Sprintf("https://%s%s", e.Addr(), path)
}

func (e Endpoint) HasCustomTimeout() bool { return e.Timeout.Duration != 0 }
func (e Endpoint) HasCustomSamples() bool { return e.Samples != 0 }

type BenchmarkConfig struct {
	DownloadBytes int64  `yaml:"download_bytes,omitempty"`
	DownloadURL   string `yaml:"download_url,omitempty"`
}

type HistoryConfig struct {
	Enabled       *bool    `yaml:"enabled,omitempty"`
	Retention     Duration `yaml:"retention,omitempty"`
	WatchInterval Duration `yaml:"watch_interval,omitempty"`
}

type Destination struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
}

type NetworkCheckConfig struct {
	Destinations []Destination `yaml:"destinations,omitempty"`
}

type Config struct {
	Defaults     Defaults           `yaml:"defaults"`
	SingBox      SingBoxConfig      `yaml:"singbox"`
	Benchmark    BenchmarkConfig    `yaml:"benchmark,omitempty"`
	History      HistoryConfig      `yaml:"history,omitempty"`
	NetworkCheck NetworkCheckConfig `yaml:"network_check,omitempty"`
	Endpoints    []Endpoint         `yaml:"endpoints"`
}

const (
	DefaultTimeout           = 5 * time.Second
	DefaultSamples           = 4
	DefaultSampleInterval    = 150 * time.Millisecond
	DefaultHealthyLatency    = 800 * time.Millisecond
	DefaultMaxJitter         = 80 * time.Millisecond
	DefaultTestURL           = "https://www.gstatic.com/generate_204"
	DefaultSingBoxController = "http://127.0.0.1:9090"
)

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}
	if err := applyDefaults(&cfg); err != nil {
		return nil, err
	}
	if len(cfg.Endpoints) == 0 {
		return nil, fmt.Errorf("config %s has no endpoints", path)
	}
	seen := make(map[string]struct{})
	for i := range cfg.Endpoints {
		ep := &cfg.Endpoints[i]
		if _, ok := seen[ep.Name]; ok {
			return nil, fmt.Errorf("duplicate endpoint name %q", ep.Name)
		}
		seen[ep.Name] = struct{}{}
		if err := normalize(ep, &cfg); err != nil {
			return nil, fmt.Errorf("endpoint %q: %w", ep.Name, err)
		}
	}
	return &cfg, nil
}

func applyDefaults(cfg *Config) error {
	if cfg.Benchmark.DownloadBytes < 0 {
		return fmt.Errorf("benchmark.download_bytes must be positive")
	}
	if cfg.Benchmark.DownloadBytes == 0 {
		cfg.Benchmark.DownloadBytes = 8 * 1024 * 1024
	}
	if cfg.Benchmark.DownloadURL == "" {
		cfg.Benchmark.DownloadURL = "https://speed.cloudflare.com/__down"
	}
	if err := validateHTTPURL("benchmark.download_url", cfg.Benchmark.DownloadURL); err != nil {
		return err
	}
	if cfg.History.Retention.Duration < 0 {
		return fmt.Errorf("history.retention must be positive")
	}
	if cfg.History.Retention.Duration == 0 {
		cfg.History.Retention.Duration = 90 * 24 * time.Hour
	}
	if cfg.History.WatchInterval.Duration == 0 {
		cfg.History.WatchInterval.Duration = 5 * time.Minute
	}
	if cfg.History.WatchInterval.Duration < 0 {
		return fmt.Errorf("history.watch_interval must be positive")
	}
	if cfg.History.Enabled == nil {
		def := true
		cfg.History.Enabled = &def
	}

	if cfg.Defaults.Timeout.Duration == 0 {
		cfg.Defaults.Timeout.Duration = DefaultTimeout
	}
	if cfg.Defaults.Samples == 0 {
		cfg.Defaults.Samples = DefaultSamples
	}
	if cfg.Defaults.SampleInterval.Duration == 0 {
		cfg.Defaults.SampleInterval.Duration = DefaultSampleInterval
	}
	if cfg.Defaults.HealthyLatency.Duration == 0 {
		cfg.Defaults.HealthyLatency.Duration = DefaultHealthyLatency
	}
	if cfg.Defaults.MaxJitter.Duration == 0 {
		cfg.Defaults.MaxJitter.Duration = DefaultMaxJitter
	}
	if cfg.Defaults.Timeout.Duration < 0 || cfg.Defaults.Samples < 0 || cfg.Defaults.SampleInterval.Duration < 0 || cfg.Defaults.HealthyLatency.Duration < 0 || cfg.Defaults.MaxJitter.Duration < 0 {
		return fmt.Errorf("default durations and samples must be non-negative")
	}
	if cfg.Defaults.MaxLoss != nil && (*cfg.Defaults.MaxLoss < 0 || *cfg.Defaults.MaxLoss > 100) {
		return fmt.Errorf("defaults.max_loss must be 0-100")
	}
	if cfg.SingBox.Controller == "" {
		cfg.SingBox.Controller = DefaultSingBoxController
	}
	if cfg.SingBox.TestURL == "" {
		cfg.SingBox.TestURL = DefaultTestURL
	}
	if err := validateHTTPURL("singbox.test_url", cfg.SingBox.TestURL); err != nil {
		return err
	}
	if cfg.SingBox.Controller != "" {
		if err := validateHTTPURL("singbox.controller", cfg.SingBox.Controller); err != nil {
			return err
		}
	}
	if cfg.SingBox.BenchmarkProxy != "" {
		if err := validateBenchmarkProxy(cfg.SingBox.BenchmarkProxy); err != nil {
			return err
		}
	}
	for i, dest := range cfg.NetworkCheck.Destinations {
		if dest.URL == "" {
			return fmt.Errorf("network_check.destinations[%d].url is required", i)
		}
		if err := validateHTTPURL(fmt.Sprintf("network_check.destinations[%d].url", i), dest.URL); err != nil {
			return err
		}
	}
	// resolve secret file if set
	if cfg.SingBox.SecretFile != "" {
		expanded := expandPath(cfg.SingBox.SecretFile)
		data, err := os.ReadFile(expanded)
		if err != nil {
			return fmt.Errorf("reading singbox secret file %s: %w", expanded, err)
		}
		s := strings.TrimSpace(string(data))
		if s != "" {
			cfg.SingBox.Secret = s
		}
	}
	// env override: FROSTGATE_SINGBOX_SECRET env var wins if set
	if env := os.Getenv("FROSTGATE_SINGBOX_SECRET"); env != "" {
		cfg.SingBox.Secret = env
	}
	return nil
}

func normalize(e *Endpoint, cfg *Config) error {
	if e.Name == "" {
		return fmt.Errorf("missing name")
	}
	if e.Type == "" {
		e.Type = ProbeTCP
	}
	switch e.Type {
	case ProbeTCP, ProbeTLS, ProbeHTTP, ProbeSingBox:
	default:
		return fmt.Errorf("unknown type %q (want tcp|tls|http|singbox)", e.Type)
	}

	switch e.Type {
	case ProbeSingBox:
		if e.Outbound == "" {
			return fmt.Errorf("singbox probe requires outbound")
		}
		if e.Host != "" || e.Port != 0 {
			return fmt.Errorf("singbox probe should not have host/port, use outbound instead")
		}
		if e.URL != "" {
			return fmt.Errorf("singbox probe should not have url, test_url is in singbox section")
		}
	case ProbeHTTP:
		if e.URL != "" {
			if err := validateHTTPURL("url", e.URL); err != nil {
				return err
			}
			if e.Host != "" || e.Port != 0 || e.Path != "" {
				return fmt.Errorf("http probe with url must not have host/port/path")
			}
		} else {
			if e.Host == "" || e.Port == 0 {
				return fmt.Errorf("missing host or port (or url)")
			}
			if e.Outbound != "" {
				return fmt.Errorf("http probe should not have outbound")
			}
		}
	default: // tcp, tls
		if e.Host == "" || e.Port == 0 {
			return fmt.Errorf("missing host or port")
		}
		if e.URL != "" {
			return fmt.Errorf("tcp/tls probe should not have url")
		}
		if e.Outbound != "" {
			return fmt.Errorf("tcp/tls probe should not have outbound")
		}
	}

	// priority default already 0 if unset, keep as is; higher priority number = lower preference? Spec says lower or higher wins documented.
	// We document lower wins (10 beats 20), but allow any int.

	// validate per endpoint durations
	if e.Timeout.Duration < 0 {
		return fmt.Errorf("invalid timeout")
	}
	if e.Samples < 0 {
		return fmt.Errorf("invalid samples")
	}
	if e.SampleInterval.Duration < 0 {
		return fmt.Errorf("invalid sample_interval")
	}
	if e.HealthyLatency.Duration < 0 {
		return fmt.Errorf("invalid healthy_latency")
	}
	if e.MaxJitter.Duration < 0 {
		return fmt.Errorf("invalid max_jitter")
	}
	if e.MaxLoss != nil && (*e.MaxLoss < 0 || *e.MaxLoss > 100) {
		return fmt.Errorf("max_loss must be 0-100")
	}
	if e.ExpectedStatus < 0 || e.ExpectedStatus > 599 {
		return fmt.Errorf("expected_status must be 100-599")
	}
	if e.ExpectedStatus > 0 && e.ExpectedStatus < 100 {
		return fmt.Errorf("expected_status must be 100-599")
	}
	return nil
}

func validateHTTPURL(field, raw string) error {
	u, err := url.ParseRequestURI(raw)
	if err != nil {
		return fmt.Errorf("%s invalid: %w", field, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s must use http or https", field)
	}
	if u.Host == "" {
		return fmt.Errorf("%s must include a host", field)
	}
	return nil
}

func expandPath(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

// IsLoopbackHost reports whether the host part of an address is a loopback
// address suitable for the Frostgate-managed benchmark listener. Public, LAN
// and wildcard addresses are rejected.
func IsLoopbackHost(h string) bool {
	return h == "127.0.0.1" || h == "::1" || h == "localhost"
}

func validateBenchmarkProxy(addr string) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("singbox.benchmark_proxy must be host:port: %q", addr)
	}
	if !IsLoopbackHost(host) {
		return fmt.Errorf("singbox.benchmark_proxy must be a loopback address (127.0.0.1/::1/localhost), got %q", host)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("singbox.benchmark_proxy has invalid port: %q", addr)
	}
	return nil
}

// BenchmarkProxyAddr returns the configured benchmark proxy address. An empty
// string means throughput benchmarking via a proxy listener is not available
// and callers must report Download: N/A rather than guessing a port.
func (c *Config) BenchmarkProxyAddr() string {
	return c.SingBox.BenchmarkProxy
}

// BenchmarkDownloadURL returns the download URL with the configured byte count
// applied authoritatively as the `bytes` query parameter.
func (c *Config) BenchmarkDownloadURL() string {
	u, err := benchmark.BuildDownloadURL(c.Benchmark.DownloadURL, c.Benchmark.DownloadBytes)
	if err != nil {
		return c.Benchmark.DownloadURL
	}
	return u
}

func ResolveConfigPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		p := filepath.Join(xdg, "frostgate", "config.yaml")
		if fileExists(p) {
			return p
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".config", "frostgate", "config.yaml")
		if fileExists(p) {
			return p
		}
	}
	if fileExists("frostgate.yaml") {
		return "frostgate.yaml"
	}
	if fileExists("frostgate.yml") {
		return "frostgate.yml"
	}
	// default to XDG path even if not exists, for error messaging
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "frostgate", "config.yaml")
	}
	return "frostgate.yaml"
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func (c *Config) TimeoutFor(e Endpoint) time.Duration {
	if e.Timeout.Duration != 0 {
		return e.Timeout.Duration
	}
	return c.Defaults.Timeout.Duration
}

func (c *Config) SamplesFor(e Endpoint) int {
	if e.Samples != 0 {
		return e.Samples
	}
	return c.Defaults.Samples
}

func (c *Config) SampleIntervalFor(e Endpoint) time.Duration {
	if e.SampleInterval.Duration != 0 {
		return e.SampleInterval.Duration
	}
	return c.Defaults.SampleInterval.Duration
}

func (c *Config) HealthyLatencyFor(e Endpoint) time.Duration {
	if e.HealthyLatency.Duration != 0 {
		return e.HealthyLatency.Duration
	}
	return c.Defaults.HealthyLatency.Duration
}

func (c *Config) MaxJitterFor(e Endpoint) time.Duration {
	if e.MaxJitter.Duration != 0 {
		return e.MaxJitter.Duration
	}
	return c.Defaults.MaxJitter.Duration
}

func (c *Config) MaxLossFor(e Endpoint) float64 {
	if e.MaxLoss != nil {
		return *e.MaxLoss
	}
	if c.Defaults.MaxLoss != nil {
		return *c.Defaults.MaxLoss
	}
	return 0
}
