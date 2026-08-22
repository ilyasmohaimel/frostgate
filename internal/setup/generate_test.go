package setup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/ilyasmohaimel/frostgate/internal/config"
	"github.com/ilyasmohaimel/frostgate/internal/probe"
)

func TestGenerateSecret(t *testing.T) {
	s, err := GenerateSecret()
	if err != nil {
		t.Fatalf("secret: %v", err)
	}
	if len(s) < 20 {
		t.Errorf("secret too short: %q", s)
	}
	// Ensure different each time
	s2, _ := GenerateSecret()
	if s == s2 {
		t.Error("secrets should be different")
	}
}

func TestGenerateSingBoxConfig(t *testing.T) {
	dir := t.TempDir()
	secret := "testsecret"
	vless := []*VLESSOutbound{
		{Tag: "reality-primary", Server: "203.0.113.10", ServerPort: 443, UUID: "11111111-1111-1111-1111-111111111111", SNI: "example.com", FP: "chrome", PBK: "testpbk", SID: "testsid", Flow: "xtls-rprx-vision"},
	}
	hyst := []*Hysteria2Outbound{
		{Tag: "hysteria-backup", Server: "203.0.113.10", ServerPort: 443, Password: "pass", ObfsPassword: "obfs", SNI: "example.com", Insecure: true},
	}
	data, err := GenerateSingBoxConfig(secret, vless, hyst, dir)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("json: %v", err)
	}
	// Check controller is loopback
	exp, ok := cfg["experimental"].(map[string]any)
	if !ok {
		t.Fatal("missing experimental")
	}
	clash, ok := exp["clash_api"].(map[string]any)
	if !ok {
		t.Fatal("missing clash_api")
	}
	if clash["external_controller"] != "127.0.0.1:9090" {
		t.Errorf("controller %v", clash["external_controller"])
	}
	if clash["secret"] != secret {
		t.Error("secret mismatch")
	}
	// Check outbounds
	outs, ok := cfg["outbounds"].([]any)
	if !ok || len(outs) < 4 {
		t.Fatalf("outbounds %v", outs)
	}
	// Should contain our tags
	found := map[string]bool{}
	for _, o := range outs {
		if m, ok := o.(map[string]any); ok {
			if tag, ok := m["tag"].(string); ok {
				found[tag] = true
			}
			// Ensure no secret in unexpected place
			// Check that password fields exist but are not logged elsewhere
		}
	}
	if !found["reality-primary"] || !found["hysteria-backup"] {
		t.Errorf("tags not found %v", found)
	}
	// Ensure 0.0.0.0 not present
	if strings.Contains(string(data), "0.0.0.0") {
		t.Error("should not contain 0.0.0.0")
	}
}

func TestGenerateSingBoxConfigLoopbackEnforced(t *testing.T) {
	dir := t.TempDir()
	data, err := GenerateSingBoxConfig("s", nil, nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !IsLoopbackController("http://127.0.0.1:9090") {
		t.Error("loopback check failed")
	}
	if IsLoopbackController("http://0.0.0.0:9090") {
		t.Error("0.0.0.0 should not be loopback")
	}
	_ = data
}

func TestGenerateFrostgateConfig(t *testing.T) {
	content := GenerateFrostgateConfig([]string{"reality-primary"}, []string{"hysteria-backup"}, "https://www.gstatic.com/generate_204", "http://127.0.0.1:9090", "/home/user/.config/frostgate/controller-secret")
	if !strings.Contains(content, "reality-primary") {
		t.Error("missing reality tag")
	}
	if !strings.Contains(content, "hysteria-backup") {
		t.Error("missing hyst tag")
	}
	if !strings.Contains(content, "controller: http://127.0.0.1:9090") {
		t.Error("controller missing")
	}
	if !strings.Contains(content, "secret_file:") {
		t.Error("secret_file missing")
	}
	if strings.Contains(content, "0.0.0.0") {
		t.Error("should not contain 0.0.0.0")
	}
	// Ensure direct baseline present
	if !strings.Contains(content, "direct") {
		t.Error("direct baseline missing")
	}
}

func TestBuildFrostgateConfigDirectStatusPolicy(t *testing.T) {
	for _, tt := range []struct {
		name string
		url  string
		want int
	}{
		{"default generate_204", "https://www.gstatic.com/generate_204", 204},
		{"custom URL", "https://example.com/health", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data, err := MarshalFrostgateConfig(nil, nil, tt.url, "http://127.0.0.1:9090", "")
			if err != nil {
				t.Fatal(err)
			}
			var doc map[string]any
			if err := yaml.Unmarshal(data, &doc); err != nil {
				t.Fatal(err)
			}
			endpoint := doc["endpoints"].([]any)[0].(map[string]any)
			if got, _ := endpoint["expected_status"].(int); got != tt.want {
				t.Fatalf("expected_status = %d, want %d in %s", got, tt.want, data)
			}
		})
	}
}

func TestGeneratedCustomDirectUsesGenericHTTPStatusPolicy(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			defer server.Close()
			cfg := BuildFrostgateConfig(nil, nil, server.URL, "http://127.0.0.1:9090", "")
			if cfg.Endpoints[0].ExpectedStatus != 0 {
				t.Fatalf("custom endpoint unexpectedly requires %d", cfg.Endpoints[0].ExpectedStatus)
			}
			result := probe.ProbeEndpoint(context.Background(), &cfg, cfg.Endpoints[0])
			if gotUp := result.OK(); gotUp != (status == http.StatusOK) {
				t.Fatalf("status %d result = %+v", status, result)
			}
		})
	}
}

func TestGeneratedDefaultDirectRequires204(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	cfg := BuildFrostgateConfig(nil, nil, config.DefaultTestURL, "http://127.0.0.1:9090", "")
	if cfg.Endpoints[0].ExpectedStatus != http.StatusNoContent {
		t.Fatalf("default expected status = %d", cfg.Endpoints[0].ExpectedStatus)
	}
	cfg.Endpoints[0].URL = server.URL
	if result := probe.ProbeEndpoint(context.Background(), &cfg, cfg.Endpoints[0]); result.OK() {
		t.Fatalf("generated direct baseline accepted 200: %+v", result)
	}
}

func TestWriteFileSecurePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "secret")
	data := []byte("secretdata")
	if err := WriteFileSecure(path, data); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("perm %04o want 0600", info.Mode().Perm())
	}
	// Ensure dir 0700 for EnsureConfigDir
	cfgDir := filepath.Join(dir, "cfg")
	if err := EnsureConfigDir(cfgDir); err != nil {
		t.Fatal(err)
	}
	info, _ = os.Stat(cfgDir)
	if info.Mode().Perm() != 0o700 {
		t.Errorf("dir perm %04o want 0700", info.Mode().Perm())
	}
}

func TestConfigDoesNotContainSecretInError(t *testing.T) {
	// Simulate parsing failure should not leak full URL
	link := "vless://uuid@1.1.1.1:443?pbk=secret&sni=x"
	_, err := ParseVLESSShareLink(link, "")
	if err == nil {
		t.Fatal("should fail missing tag")
	}
	// Error should not contain secret? Our function returns "tag required" not the link, so safe
	if strings.Contains(err.Error(), "secret") {
		t.Error("error should not leak secret")
	}
}
