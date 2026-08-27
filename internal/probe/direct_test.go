package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ilyasmohaimel/frostgate/internal/config"
)

func TestHTTPProbeBypassesProxyEnvironment(t *testing.T) {
	var proxyHits, targetHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyHits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer proxy.Close()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
		t.Setenv(name, proxy.URL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	result := ProbeEndpoint(context.Background(), &config.Config{
		Defaults: config.Defaults{
			Timeout:        config.Duration{Duration: time.Second},
			Samples:        1,
			HealthyLatency: config.Duration{Duration: time.Second},
			MaxJitter:      config.Duration{Duration: time.Second},
		},
		Endpoints: []config.Endpoint{{Name: "direct", Type: config.ProbeHTTP, URL: target.URL}},
	}, config.Endpoint{Name: "direct", Type: config.ProbeHTTP, URL: target.URL})
	if result.Status != StatusUp {
		t.Fatalf("direct probe failed: %+v", result)
	}
	if proxyHits.Load() != 0 || targetHits.Load() != 1 {
		t.Fatalf("proxy hits=%d target hits=%d", proxyHits.Load(), targetHits.Load())
	}
}

func TestHTTPProbeAcceptsUppercaseScheme(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	url := "HTTP://" + strings.TrimPrefix(target.URL, "http://")
	result := ProbeEndpoint(context.Background(), &config.Config{
		Defaults: config.Defaults{
			Timeout:        config.Duration{Duration: time.Second},
			Samples:        1,
			HealthyLatency: config.Duration{Duration: time.Second},
			MaxJitter:      config.Duration{Duration: time.Second},
		},
		Endpoints: []config.Endpoint{{Name: "uppercase", Type: config.ProbeHTTP, URL: url}},
	}, config.Endpoint{Name: "uppercase", Type: config.ProbeHTTP, URL: url})
	if result.Status != StatusUp {
		t.Fatalf("uppercase HTTP probe failed: %+v", result)
	}
}
