package probe

import (
	"testing"

	"github.com/ilyasmohaimel/frostgate/internal/config"
)

func TestDirectBaselineRequiresExplicitIdentity(t *testing.T) {
	tests := []struct {
		name string
		ep   config.Endpoint
		want bool
	}{
		{"setup direct", config.Endpoint{Name: "direct", Type: config.ProbeHTTP}, true},
		{"legacy direct baseline", config.Endpoint{Name: "direct-baseline", Type: config.ProbeHTTP}, true},
		{"unrelated http", config.Endpoint{Name: "github-health", Type: config.ProbeHTTP}, false},
		{"named tcp", config.Endpoint{Name: "direct", Type: config.ProbeTCP}, false},
		{"named tls", config.Endpoint{Name: "direct-baseline", Type: config.ProbeTLS}, false},
		{"tcp", config.Endpoint{Name: "tcp-check", Type: config.ProbeTCP}, false},
		{"tls", config.Endpoint{Name: "tls-check", Type: config.ProbeTLS}, false},
		{"proxy", config.Endpoint{Name: "direct", Type: config.ProbeSingBox}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok := DirectBaseline([]Result{{Endpoint: tt.ep}})
			if ok != tt.want {
				t.Fatalf("DirectBaseline(%+v) = %v, want %v", tt.ep, ok, tt.want)
			}
		})
	}
}
