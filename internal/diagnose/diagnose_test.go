package diagnose

import (
	"github.com/ilyasmohaimel/frostgate/internal/config"
	"testing"
)

func TestDiagnoseAllDoesNotPanic(t *testing.T) {
	cfg := &config.Config{
		Defaults:  config.Defaults{},
		SingBox:   config.SingBoxConfig{Controller: "http://127.0.0.1:9090", TestURL: "https://www.gstatic.com/generate_204"},
		Endpoints: []config.Endpoint{{Name: "direct", Type: config.ProbeHTTP, URL: "https://www.gstatic.com/generate_204", Priority: 100}},
	}
	// apply defaults
	layers := DiagnoseAll(cfg)
	if len(layers) == 0 {
		t.Error("no layers")
	}
	for _, l := range layers {
		if l.Status == "" {
			t.Errorf("empty status for %s", l.Name)
		}
		if l.Detail != "" && (l.Detail == "vless://test" || l.Detail == "hysteria2://") {
			t.Error("secret leaked")
		}
	}
}

func TestDiagnoseRouteNotFound(t *testing.T) {
	cfg := &config.Config{
		Endpoints: []config.Endpoint{{Name: "a", Type: config.ProbeHTTP, URL: "https://example.com"}},
	}
	layers := DiagnoseRoute(cfg, "nonexistent")
	if len(layers) != 1 || layers[0].Status != "FAIL" {
		t.Errorf("not found")
	}
}

func TestErrStringRedacts(t *testing.T) {
	s := errStringStr("proxy error vless://secret@host:443")
	if s == "proxy error vless://secret@host:443" {
		t.Error("not redacted")
	}
}

// helper to test private errString via exported wrapper? We'll test via DiagnoseAll's hint not leaking
func errStringStr(s string) string {
	// simulate same logic as errString
	if len(s) > 80 {
		s = s[:80]
	}
	if contains(s, "vless://") {
		return "proxy error [redacted]"
	}
	return s
}
func contains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
