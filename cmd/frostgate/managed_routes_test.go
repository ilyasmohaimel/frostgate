package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/ilyasmohaimel/frostgate/internal/managedconfig"
	"github.com/ilyasmohaimel/frostgate/internal/setup"
)

const existingManagedConfig = `{
  "dns": {"servers": [{"tag": "custom-dns", "address": "https://example.com/dns-query"}]},
  "experimental": {"clash_api": {"external_controller": "127.0.0.1:9090", "secret": "fixture-secret"}},
  "inbounds": [{"type": "mixed", "tag": "frostgate-mixed", "listen": "127.0.0.1", "listen_port": 2080}],
  "route": {"rules": [{"inbound": "frostgate-mixed", "outbound": "frostgate-selector"}], "final": "direct"},
  "outbounds": [
    {"type": "direct", "tag": "direct"},
    {"type": "block", "tag": "block"},
    {"type": "selector", "tag": "frostgate-selector", "outbounds": ["primary-reality", "backup-hysteria2"], "default": "primary-reality"},
    {"type": "vless", "tag": "primary-reality", "server": "203.0.113.10", "server_port": 443, "uuid": "11111111-1111-4111-8111-111111111111", "flow": "xtls-rprx-vision", "packet_encoding": "xudp", "tls": {"enabled": true, "server_name": "example.com", "utls": {"enabled": true, "fingerprint": "firefox"}, "reality": {"enabled": true, "public_key": "fixture-pbk", "short_id": "a1b2", "spider_x": "/edge"}}},
    {"type": "hysteria2", "tag": "backup-hysteria2", "server": "198.51.100.10", "server_port": 443, "password": "fixture-password", "obfs": {"type": "salamander", "password": "fixture-obfs"}, "tls": {"enabled": true, "server_name": "example.org", "insecure": true}},
    {"type": "socks", "tag": "user-extra", "server": "192.0.2.10", "server_port": 1080}
  ]
}`

const existingFrostgateConfig = `defaults:
  timeout: 1s
singbox:
  controller: http://127.0.0.1:9090
  secret_file: REPLACED_SECRET_FILE
  test_url: https://www.gstatic.com/generate_204
  benchmark_proxy: 127.0.0.1:2080
history:
  enabled: false
custom_extension:
  keep: true
endpoints:
  - name: direct
    type: http
    url: https://www.gstatic.com/generate_204
    expected_status: 204
  - name: primary-reality
    type: singbox
    outbound: primary-reality
  - name: backup-hysteria2
    type: singbox
    outbound: backup-hysteria2
`

func writeExistingManagedFixture(t *testing.T) (home, dir, configPath string) {
	t.Helper()
	home = t.TempDir()
	xdg := filepath.Join(home, "custom-xdg")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dir = filepath.Join(xdg, "frostgate")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath = filepath.Join(dir, "config.yaml")
	secretPath := filepath.Join(dir, "controller-secret")
	if err := os.WriteFile(secretPath, []byte("fixture-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sing-box.json"), []byte(existingManagedConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	content := strings.ReplaceAll(existingFrostgateConfig, "REPLACED_SECRET_FILE", secretPath)
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return home, dir, configPath
}

func interactiveAdd(t *testing.T, home, dir, configPath, tag string, runner *managedconfig.SidecarRunner) error {
	t.Helper()
	link := "vless://22222222-2222-4222-8222-222222222222@203.0.113.20:443?security=reality&sni=example.net&fp=chrome&pbk=new-fixture-pbk&sid=c3d4&flow=xtls-rprx-vision&spx=%2Fnew&packet_encoding=packetaddr"
	input := "a\n" + link + "\n" + tag + "\n"
	return setup.RunInteractive(setup.Options{
		HomeDir: home,
		Stdin:   bufio.NewReader(strings.NewReader(input)),
		AddRoute: func(gotDir, gotLink, gotTag string) error {
			if gotDir != dir || gotLink != link || gotTag != tag {
				return errors.New("interactive setup passed unexpected route arguments")
			}
			_, err := addManagedRoute(gotDir, configPath, gotLink, gotTag, runner)
			return err
		},
	})
}

func TestInteractiveSetupAddPreservesManagedConfiguration(t *testing.T) {
	home, dir, configPath := writeExistingManagedFixture(t)
	validated := 0
	runner := &managedconfig.SidecarRunner{
		Validator:       func([]byte) error { validated++; return nil },
		DaemonReload:    func() error { return nil },
		Restart:         func() error { return nil },
		IsActive:        func() (bool, error) { return true, nil },
		ControllerReady: func([]string) (bool, error) { return true, nil },
	}
	if err := interactiveAdd(t, home, dir, configPath, "new-reality", runner); err != nil {
		t.Fatalf("interactive add: %v", err)
	}
	if validated != 1 {
		t.Fatalf("candidate was not validated exactly once: %d", validated)
	}

	data, err := os.ReadFile(filepath.Join(dir, "sing-box.json"))
	if err != nil {
		t.Fatal(err)
	}
	var sing map[string]any
	if err := json.Unmarshal(data, &sing); err != nil {
		t.Fatal(err)
	}
	if _, ok := sing["dns"]; !ok {
		t.Fatal("non-managed top-level config was lost")
	}
	wantTags := map[string]bool{"primary-reality": false, "backup-hysteria2": false, "new-reality": false, "user-extra": false}
	for _, raw := range sing["outbounds"].([]any) {
		outbound := raw.(map[string]any)
		tag, _ := outbound["tag"].(string)
		if _, ok := wantTags[tag]; ok {
			wantTags[tag] = true
		}
		if tag == "primary-reality" {
			if outbound["flow"] != "xtls-rprx-vision" || outbound["packet_encoding"] != "xudp" {
				t.Fatalf("VLESS fields lost: %v", outbound)
			}
			tls := outbound["tls"].(map[string]any)
			if tls["server_name"] != "example.com" || tls["utls"].(map[string]any)["fingerprint"] != "firefox" || tls["reality"].(map[string]any)["spider_x"] != "/edge" {
				t.Fatalf("VLESS TLS fields lost: %v", tls)
			}
		}
		if tag == "backup-hysteria2" {
			tls := outbound["tls"].(map[string]any)
			if outbound["password"] != "fixture-password" || outbound["obfs"].(map[string]any)["type"] != "salamander" || outbound["obfs"].(map[string]any)["password"] != "fixture-obfs" || tls["insecure"] != true {
				t.Fatalf("Hysteria2 fields lost: %v", outbound)
			}
		}
	}
	for tag, found := range wantTags {
		if !found {
			t.Fatalf("outbound %q was lost", tag)
		}
	}

	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var configDoc map[string]any
	if err := yaml.Unmarshal(configData, &configDoc); err != nil {
		t.Fatal(err)
	}
	if configDoc["custom_extension"].(map[string]any)["keep"] != true {
		t.Fatal("unknown Frostgate config key was lost")
	}
	if configDoc["history"].(map[string]any)["enabled"] != false {
		t.Fatal("history configuration was changed")
	}
	foundEndpoint := false
	for _, raw := range configDoc["endpoints"].([]any) {
		endpoint := raw.(map[string]any)
		if endpoint["name"] == "new-reality" && endpoint["outbound"] == "new-reality" {
			foundEndpoint = true
		}
	}
	if !foundEndpoint {
		t.Fatal("new route endpoint missing from Frostgate config")
	}
}

func TestInteractiveSetupAddRejectsUnsafeRouteName(t *testing.T) {
	home, dir, configPath := writeExistingManagedFixture(t)
	called := false
	err := setup.RunInteractive(setup.Options{
		HomeDir: home,
		Stdin:   bufio.NewReader(strings.NewReader("a\nvless://uuid@example.com:443?security=reality&sni=example.com&pbk=key\nbad/name\n")),
		AddRoute: func(_, _, _ string) error {
			called = true
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid route name") {
		t.Fatalf("unsafe name error = %v", err)
	}
	if called {
		t.Fatal("unsafe name reached managed route callback")
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("fixture changed unexpectedly: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sing-box.json")); err != nil {
		t.Fatalf("fixture changed unexpectedly: %v", err)
	}
}

func TestInteractiveSetupAddRollsBackBothConfigs(t *testing.T) {
	home, dir, configPath := writeExistingManagedFixture(t)
	beforeSing, err := os.ReadFile(filepath.Join(dir, "sing-box.json"))
	if err != nil {
		t.Fatal(err)
	}
	beforeConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	restarts := 0
	runner := &managedconfig.SidecarRunner{
		Validator:    func([]byte) error { return nil },
		DaemonReload: func() error { return nil },
		Restart: func() error {
			restarts++
			return errors.New("restart failed")
		},
	}
	if err := interactiveAdd(t, home, dir, configPath, "new-reality", runner); err == nil {
		t.Fatal("expected transaction failure")
	}
	afterSing, _ := os.ReadFile(filepath.Join(dir, "sing-box.json"))
	afterConfig, _ := os.ReadFile(configPath)
	if !bytes.Equal(afterSing, beforeSing) || !bytes.Equal(afterConfig, beforeConfig) {
		t.Fatal("transaction failure left a partial route")
	}
	if restarts != 2 {
		t.Fatalf("sidecar was not restarted after rollback: %d calls", restarts)
	}
}
