// Package config loads and validates frostgate's YAML configuration.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration so it can be written in YAML as a human string
// like "5s" or "750ms" instead of a raw nanosecond count.
type Duration struct {
	time.Duration
}

// UnmarshalYAML teaches yaml.v3 how to turn "5s" into a time.Duration.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = parsed
	return nil
}

// ProbeType is how a given endpoint should be tested.
type ProbeType string

const (
	ProbeTCP  ProbeType = "tcp"
	ProbeTLS  ProbeType = "tls"
	ProbeHTTP ProbeType = "http"
)

// Endpoint is one route frostgate probes.
//
// MAKE-IT-YOURS #1 (the schema): this struct is the foundation of the whole
// tool — every probe, table row, and JSON record flows from these fields. A
// working set is defined below so v0 runs today. Once it does, revisit it:
//   - Should Timeout be per-endpoint (override the global default)? Add it.
//   - Should an empty Type default to "tcp", or be a hard error in Validate?
//   - What other metadata would you want (region, weight, protocol note)?
// The `yaml:"...,omitempty"` tags map snake_case config keys onto fields and
// mark the optional ones. See frostgate.example.yaml for the live schema.
type Endpoint struct {
	Name string    `yaml:"name"`
	Host string    `yaml:"host"`
	Port int       `yaml:"port"`
	Type ProbeType `yaml:"type"`
	SNI  string    `yaml:"sni,omitempty"`  // TLS server name; defaults to Host
	Path string    `yaml:"path,omitempty"` // HTTP path; defaults to "/"
}

// Addr returns the host:port dial target.
func (e Endpoint) Addr() string {
	return fmt.Sprintf("%s:%d", e.Host, e.Port)
}

// ServerName returns the SNI to present in a TLS handshake.
func (e Endpoint) ServerName() string {
	if e.SNI != "" {
		return e.SNI
	}
	return e.Host
}

// Defaults are settings applied to every endpoint unless overridden.
type Defaults struct {
	Timeout Duration `yaml:"timeout"`
}

// Config is the top-level shape of frostgate.yaml.
type Config struct {
	Defaults  Defaults   `yaml:"defaults"`
	Endpoints []Endpoint `yaml:"endpoints"`
}

// DefaultTimeout is used when the config omits defaults.timeout.
const DefaultTimeout = 5 * time.Second

// Load reads, parses, and validates a frostgate config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	if cfg.Defaults.Timeout.Duration == 0 {
		cfg.Defaults.Timeout.Duration = DefaultTimeout
	}
	if len(cfg.Endpoints) == 0 {
		return nil, fmt.Errorf("config %s has no endpoints", path)
	}

	for i := range cfg.Endpoints {
		if err := normalize(&cfg.Endpoints[i]); err != nil {
			return nil, fmt.Errorf("endpoint %q: %w", cfg.Endpoints[i].Name, err)
		}
	}

	return &cfg, nil
}

// normalize fills in per-endpoint defaults and rejects invalid endpoints.
func normalize(e *Endpoint) error {
	if e.Name == "" {
		return fmt.Errorf("missing name")
	}
	if e.Host == "" || e.Port == 0 {
		return fmt.Errorf("missing host or port")
	}
	if e.Type == "" {
		e.Type = ProbeTCP
	}
	switch e.Type {
	case ProbeTCP, ProbeTLS, ProbeHTTP:
	default:
		return fmt.Errorf("unknown type %q (want tcp|tls|http)", e.Type)
	}
	return nil
}
