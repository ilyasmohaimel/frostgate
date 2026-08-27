package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/ilyasmohaimel/frostgate/internal/config"
	"github.com/ilyasmohaimel/frostgate/internal/managedconfig"
	"github.com/ilyasmohaimel/frostgate/internal/profile"
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

func TestDirectOnlyAddBootstrapsCompleteManagedSidecar(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	direct, err := yaml.Marshal(setup.BuildDirectOnlyConfig(config.DefaultTestURL))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, direct, 0o600); err != nil {
		t.Fatal(err)
	}
	var candidate map[string]any
	enabled := 0
	runner := &managedconfig.SidecarRunner{
		Validator:    func(data []byte) error { return json.Unmarshal(data, &candidate) },
		DaemonReload: func() error { return nil },
		Restart:      func() error { return nil },
		IsActive:     func() (bool, error) { return true, nil },
		ControllerReady: func(expected []string) (bool, error) {
			if len(expected) != 1 || expected[0] != "first-route" {
				t.Fatalf("controller expectations = %v", expected)
			}
			return true, nil
		},
		Enable: func() error { enabled++; return nil },
	}
	if _, err := addManagedRoute(dir, configPath, "vless://11111111-1111-4111-8111-111111111111@203.0.113.10:443?security=reality&type=tcp&sni=example.com&fp=chrome&pbk=fixture-key&sid=abcd&flow=xtls-rprx-vision", "first-route", runner); err != nil {
		t.Fatalf("add first managed route: %v", err)
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SingBox.Controller == "" || loaded.SingBox.BenchmarkProxy == "" || loaded.SingBox.SecretFile == "" {
		t.Fatalf("direct-only upgrade did not configure managed sidecar: %+v", loaded.SingBox)
	}
	if info, err := os.Stat(loaded.SingBox.SecretFile); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("controller secret permissions = %v, %v", info, err)
	}
	if candidate["experimental"] == nil || candidate["inbounds"] == nil || candidate["route"] == nil {
		t.Fatalf("managed sidecar is incomplete: %v", candidate)
	}
	managed, err := managedconfig.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if addr, ok := managed.BenchmarkProxy(); !ok || addr != loaded.SingBox.BenchmarkProxy {
		t.Fatalf("managed benchmark listener = %q, %t; config=%q", addr, ok, loaded.SingBox.BenchmarkProxy)
	}
	if len(managed.VLESS) != 1 || managed.VLESS[0].Tag != "first-route" {
		t.Fatalf("managed proxy missing: %+v", managed.VLESS)
	}
	if enabled != 1 {
		t.Fatalf("direct-only upgrade did not enable the managed sidecar: %d", enabled)
	}
}

func writeEndpointLimitConfig(t *testing.T, path string, count int) []byte {
	t.Helper()
	var body strings.Builder
	body.WriteString("singbox:\n  controller: http://127.0.0.1:9090\nendpoints:\n")
	for i := 0; i < count; i++ {
		fmt.Fprintf(&body, "  - name: endpoint-%d\n    type: singbox\n    outbound: endpoint-%d\n", i, i)
	}
	data := []byte(body.String())
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestAddEndpointLimitLeavesManagedStateUntouched(t *testing.T) {
	_, dir, configPath := writeExistingManagedFixture(t)
	originalConfig := writeEndpointLimitConfig(t, configPath, config.MaxEndpoints)
	originalSidecar, err := os.ReadFile(filepath.Join(dir, "sing-box.json"))
	if err != nil {
		t.Fatal(err)
	}
	originalSecret, err := os.ReadFile(filepath.Join(dir, "controller-secret"))
	if err != nil {
		t.Fatal(err)
	}

	_, err = addManagedRoute(dir, configPath, "vless://22222222-2222-4222-8222-222222222222@203.0.113.20:443?security=reality&type=tcp&sni=example.net&fp=chrome&pbk=fixture-key&sid=abcd", "overflow-route", nil)
	if err == nil || !strings.Contains(err.Error(), "too many endpoints: 33 configured, maximum is 32") {
		t.Fatalf("add error = %v", err)
	}
	if got, readErr := os.ReadFile(configPath); readErr != nil || !bytes.Equal(got, originalConfig) {
		t.Fatalf("config changed after rejected add: %q, %v", got, readErr)
	}
	if got, readErr := os.ReadFile(filepath.Join(dir, "sing-box.json")); readErr != nil || !bytes.Equal(got, originalSidecar) {
		t.Fatalf("sidecar changed after rejected add: %v", readErr)
	}
	if got, readErr := os.ReadFile(filepath.Join(dir, "controller-secret")); readErr != nil || !bytes.Equal(got, originalSecret) {
		t.Fatalf("secret changed after rejected add: %v", readErr)
	}
}

func TestImportEndpointLimitLeavesManagedStateUntouched(t *testing.T) {
	_, dir, configPath := writeExistingManagedFixture(t)
	originalConfig := writeEndpointLimitConfig(t, configPath, config.MaxEndpoints)
	originalSidecar, err := os.ReadFile(filepath.Join(dir, "sing-box.json"))
	if err != nil {
		t.Fatal(err)
	}
	originalSecret, err := os.ReadFile(filepath.Join(dir, "controller-secret"))
	if err != nil {
		t.Fatal(err)
	}
	subscription := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, "vless://22222222-2222-4222-8222-222222222222@203.0.113.20:443?security=reality&type=tcp&sni=example.net&fp=chrome&pbk=fixture-key&sid=abcd#overflow-route")
	}))
	defer subscription.Close()

	if code := handleImport([]string{"--allow-insecure-http", "--config", configPath, subscription.URL}); code != 3 {
		t.Fatalf("import exit code = %d, want 3", code)
	}
	if got, readErr := os.ReadFile(configPath); readErr != nil || !bytes.Equal(got, originalConfig) {
		t.Fatalf("config changed after rejected import: %q, %v", got, readErr)
	}
	if got, readErr := os.ReadFile(filepath.Join(dir, "sing-box.json")); readErr != nil || !bytes.Equal(got, originalSidecar) {
		t.Fatalf("sidecar changed after rejected import: %v", readErr)
	}
	if got, readErr := os.ReadFile(filepath.Join(dir, "controller-secret")); readErr != nil || !bytes.Equal(got, originalSecret) {
		t.Fatalf("secret changed after rejected import: %v", readErr)
	}
}

func TestCustomProfileAddUsesOnlyItsOwnServiceAndFiles(t *testing.T) {
	oldFind, oldWrite, oldRunner := findSingBox, writeSystemdUnit, newSidecarRunner
	t.Cleanup(func() { findSingBox, writeSystemdUnit, newSidecarRunner = oldFind, oldWrite, oldRunner })

	root := t.TempDir()
	defaultDir := filepath.Join(root, "default")
	customDir := filepath.Join(root, "profiles", "custom")
	if err := os.MkdirAll(defaultDir, 0o700); err != nil {
		t.Fatal(err)
	}
	defaultSidecar := []byte(`{"outbounds":[{"type":"direct","tag":"direct"}]}`)
	if err := os.WriteFile(filepath.Join(defaultDir, "sing-box.json"), defaultSidecar, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(customDir, 0o700); err != nil {
		t.Fatal(err)
	}
	customConfig := filepath.Join(customDir, "config.yaml")
	direct, err := yaml.Marshal(setup.BuildDirectOnlyConfig(config.DefaultTestURL))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(customConfig, direct, 0o600); err != nil {
		t.Fatal(err)
	}
	identity := profile.ForPath(customConfig, false)
	var unitPath, unitBinary, unitDir, runnerService string
	findSingBox = func(string) (string, error) { return "/opt/bin/sing-box", nil }
	writeSystemdUnit = func(path, binary, dir string) error {
		unitPath, unitBinary, unitDir = path, binary, dir
		return nil
	}
	newSidecarRunner = func(controller, secret, service string) *managedconfig.SidecarRunner {
		runnerService = service
		return &managedconfig.SidecarRunner{
			Validator:    func([]byte) error { return nil },
			DaemonReload: func() error { return nil },
			Restart:      func() error { return nil },
			IsActive:     func() (bool, error) { return true, nil },
			ControllerReady: func([]string) (bool, error) {
				return true, nil
			},
		}
	}
	if _, err := addManagedRoute(customDir, customConfig, "hysteria2://fixture-password@203.0.113.20:443?sni=example.com&obfs=salamander&obfs-password=fixture-obfs", "custom-route", nil); err != nil {
		t.Fatal(err)
	}
	if runnerService != identity.Service || runnerService == "frostgate-singbox.service" {
		t.Fatalf("custom profile used wrong service: %q", runnerService)
	}
	if unitBinary != "/opt/bin/sing-box" || unitDir != customDir || filepath.Base(unitPath) != identity.Service {
		t.Fatalf("custom service unit = path:%q binary:%q dir:%q", unitPath, unitBinary, unitDir)
	}
	if got, err := os.ReadFile(filepath.Join(defaultDir, "sing-box.json")); err != nil || !bytes.Equal(got, defaultSidecar) {
		t.Fatalf("custom profile changed default sidecar: %q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(customDir, "controller-secret")); err != nil {
		t.Fatalf("custom controller secret missing: %v", err)
	}
	if other := profile.ForPath(filepath.Join(root, "profiles", "other", "config.yaml"), false); other.Service == identity.Service || other.ControllerAddr == identity.ControllerAddr || other.BenchmarkAddr == identity.BenchmarkAddr {
		t.Fatalf("custom identities collide: %+v %+v", identity, other)
	}
}

func TestCustomProfileRollbackDoesNotTouchDefaultProfile(t *testing.T) {
	root := t.TempDir()
	defaultDir := filepath.Join(root, "default")
	customDir := filepath.Join(root, "profiles", "rollback")
	if err := os.MkdirAll(defaultDir, 0o700); err != nil {
		t.Fatal(err)
	}
	defaultSidecar := []byte(`{"outbounds":[{"type":"direct","tag":"direct"}]}`)
	if err := os.WriteFile(filepath.Join(defaultDir, "sing-box.json"), defaultSidecar, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(customDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(customDir, "config.yaml")
	direct, err := yaml.Marshal(setup.BuildDirectOnlyConfig(config.DefaultTestURL))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, direct, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &managedconfig.SidecarRunner{
		Validator:    func([]byte) error { return nil },
		DaemonReload: func() error { return nil },
		Restart:      func() error { return errors.New("restart rejected") },
	}
	if _, err := addManagedRoute(customDir, configPath, "hysteria2://fixture-password@203.0.113.20:443?sni=example.com", "rollback-route", runner); err == nil {
		t.Fatal("expected custom profile transaction failure")
	}
	if got, err := os.ReadFile(filepath.Join(defaultDir, "sing-box.json")); err != nil || !bytes.Equal(got, defaultSidecar) {
		t.Fatalf("custom rollback changed default sidecar: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(configPath); err != nil || !bytes.Equal(got, direct) {
		t.Fatalf("custom config was not restored: %q err=%v", got, err)
	}
	for _, name := range []string{"sing-box.json", "controller-secret"} {
		if _, err := os.Stat(filepath.Join(customDir, name)); !os.IsNotExist(err) {
			t.Fatalf("rollback left custom %s: %v", name, err)
		}
	}
}

func TestFailedFirstRouteBootstrapRemovesNewProfileUnit(t *testing.T) {
	oldFind, oldWrite, oldRunner := findSingBox, writeSystemdUnit, newSidecarRunner
	t.Cleanup(func() { findSingBox, writeSystemdUnit, newSidecarRunner = oldFind, oldWrite, oldRunner })
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
	dir := filepath.Join(root, "profile")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	direct, err := yaml.Marshal(setup.BuildDirectOnlyConfig(config.DefaultTestURL))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, direct, 0o600); err != nil {
		t.Fatal(err)
	}
	identity := profile.ForPath(configPath, false)
	findSingBox = func(string) (string, error) { return "/opt/bin/sing-box", nil }
	writeSystemdUnit = setup.WriteSystemdUnit
	var stops, disables int
	newSidecarRunner = func(string, string, string) *managedconfig.SidecarRunner {
		return &managedconfig.SidecarRunner{
			Validator: func([]byte) error { return nil },
			Restart:   func() error { return errors.New("restart rejected") },
			Stop:      func() error { stops++; return nil },
			Disable:   func() error { disables++; return nil },
		}
	}
	if _, err := addManagedRoute(dir, configPath, "hysteria2://fixture-password@203.0.113.20:443?sni=example.com", "first-route", nil); err == nil {
		t.Fatal("expected bootstrap failure")
	}
	if _, err := os.Stat(setup.ServiceUnitPath(identity.Service)); !os.IsNotExist(err) {
		t.Fatalf("failed bootstrap left profile unit: %v", err)
	}
	if stops != 1 || disables != 1 {
		t.Fatalf("failed bootstrap did not quiesce transient sidecar: stop=%d disable=%d", stops, disables)
	}
}

func TestFailedExistingCustomProfileKeepsHealthyUnitRunning(t *testing.T) {
	oldFind, oldWrite, oldRunner := findSingBox, writeSystemdUnit, newSidecarRunner
	t.Cleanup(func() { findSingBox, writeSystemdUnit, newSidecarRunner = oldFind, oldWrite, oldRunner })
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
	dir := filepath.Join(root, "profiles", "custom")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	identity := profile.ForPath(configPath, false)
	managed := &managedconfig.Managed{Dir: dir, Secret: "fixture-secret"}
	managed.AddVLESS(&setup.VLESSOutbound{Tag: "existing", Server: "203.0.113.10", ServerPort: 443, UUID: "11111111-1111-4111-8111-111111111111", SNI: "example.com", PBK: "fixture-key", FP: "chrome", Network: "tcp"})
	managed.ConfigureProfile(identity.ControllerAddr, identity.BenchmarkAddr)
	sidecar, err := managed.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sing-box.json"), sidecar, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "controller-secret"), []byte("fixture-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configData := fmt.Sprintf(`singbox:
  controller: http://%s
  secret_file: %s
  benchmark_proxy: %s
endpoints:
  - name: direct
    type: http
    url: https://www.gstatic.com/generate_204
  - name: existing
    type: singbox
    outbound: existing
`, identity.ControllerAddr, filepath.Join(dir, "controller-secret"), identity.BenchmarkAddr)
	if err := os.WriteFile(configPath, []byte(configData), 0o600); err != nil {
		t.Fatal(err)
	}
	unitPath := setup.ServiceUnitPath(identity.Service)
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o700); err != nil {
		t.Fatal(err)
	}
	originalUnit := []byte("existing healthy unit")
	if err := os.WriteFile(unitPath, originalUnit, 0o600); err != nil {
		t.Fatal(err)
	}
	findSingBox = func(string) (string, error) { return "/opt/bin/sing-box", nil }
	writeSystemdUnit = setup.WriteSystemdUnit
	var stops, disables int
	newSidecarRunner = func(_, _, _ string) *managedconfig.SidecarRunner {
		return &managedconfig.SidecarRunner{
			Validator: func([]byte) error { return nil },
			Restart:   func() error { return errors.New("restart rejected") },
			Stop:      func() error { stops++; return nil },
			Disable:   func() error { disables++; return nil },
			DaemonReload: func() error {
				return nil
			},
		}
	}
	if _, err := addManagedRoute(dir, configPath, "hysteria2://fixture-password@203.0.113.20:443?sni=example.com", "new-route", nil); err == nil {
		t.Fatal("expected existing profile transaction failure")
	}
	if stops != 0 || disables != 0 {
		t.Fatalf("healthy existing profile was quiesced: stop=%d disable=%d", stops, disables)
	}
	if got, err := os.ReadFile(unitPath); err != nil || !bytes.Equal(got, originalUnit) {
		t.Fatalf("existing unit was not restored in place: %q err=%v", got, err)
	}
}

func TestManagedUnitRollbackRejectsReplacementSymlink(t *testing.T) {
	oldWrite := writeSystemdUnit
	t.Cleanup(func() { writeSystemdUnit = oldWrite })
	dir := t.TempDir()
	path := filepath.Join(dir, "profile.service")
	target := filepath.Join(dir, "outside.service")
	original := []byte("original unit")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	outside := []byte("outside unit")
	if err := os.WriteFile(target, outside, 0o600); err != nil {
		t.Fatal(err)
	}
	writeSystemdUnit = func(path, _, _ string) error {
		if err := os.Remove(path); err != nil {
			return err
		}
		return os.Symlink(target, path)
	}
	if _, created, err := writeManagedUnit(path, "/usr/bin/sing-box", dir); err == nil || created || !strings.Contains(err.Error(), "does not prove Frostgate ownership") {
		t.Fatalf("unowned unit was not rejected before replacement: created=%v err=%v", created, err)
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, outside) {
		t.Fatalf("rejected unit changed replacement target: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("rejected unit was overwritten: %q err=%v", got, err)
	}
}

func TestMigrationCleanupRequiresMatchingOwner(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, migrationOwnerFile), []byte("owner-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeOwnedMigrationState(dir, "owner-b"); err == nil {
		t.Fatal("migration cleanup ignored a mismatched owner")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("mismatched owner cleanup removed state: %v", err)
	}
	if err := removeOwnedMigrationState(dir, "owner-a"); err != nil {
		t.Fatalf("matching owner cleanup failed: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("matching owner cleanup retained state: %v", err)
	}
}

func TestFinalizeMigrationStateRemovesOnlyItsMarker(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, migrationOwnerFile), []byte("owner-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := finalizeMigrationState(dir, "owner-b"); err == nil {
		t.Fatal("migration finalization ignored a mismatched owner")
	}
	if _, err := os.Stat(filepath.Join(dir, migrationOwnerFile)); err != nil {
		t.Fatalf("mismatched finalization removed marker: %v", err)
	}
	if err := finalizeMigrationState(dir, "owner-a"); err != nil {
		t.Fatalf("migration finalization failed: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("migration finalization removed state directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, migrationOwnerFile)); !os.IsNotExist(err) {
		t.Fatalf("migration finalization retained marker: %v", err)
	}
}

func TestPrepareManagedStateRejectsInProgressMigration(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
	configPath := filepath.Join(root, "profile.yaml")
	_, state := commandPaths(configPath)
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, migrationOwnerFile), []byte("another-owner\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := prepareManagedStateWithRollback(configPath, state); err == nil || !strings.Contains(err.Error(), "in-progress migration") {
		t.Fatalf("concurrent migration was not rejected: %v", err)
	}
}

func TestMigrationRollbackPreservesDisabledInactiveLegacyService(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
	legacyDir := filepath.Join(root, "legacy")
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	identity := profile.LegacyIdentity(filepath.Join(legacyDir, "config.yaml"))
	unitPath := setup.ServiceUnitPath(identity.Service)
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unitPath, []byte(setup.BuildSystemdUnit("/usr/bin/sing-box", legacyDir)), 0o600); err != nil {
		t.Fatal(err)
	}
	oldOutput := runUserSystemctlOutput
	oldSystemctl := runUserSystemctl
	t.Cleanup(func() { runUserSystemctlOutput, runUserSystemctl = oldOutput, oldSystemctl })
	runUserSystemctlOutput = func(args ...string) ([]byte, error) {
		if args[0] == "is-enabled" {
			return []byte("disabled\n"), errors.New("disabled")
		}
		return []byte("inactive\n"), errors.New("inactive")
	}
	var calls []string
	runUserSystemctl = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	state, err := quiesceLegacyService(legacyDir, identity)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.Enabled || state.Active {
		t.Fatalf("legacy service state was not captured: %+v", state)
	}
	if err := restoreLegacyService(*state); err != nil {
		t.Fatal(err)
	}
	if want := []string{"disable --now " + identity.Service, "disable " + identity.Service}; !equalStrings(calls, want) {
		t.Fatalf("legacy disabled/inactive lifecycle = %v, want %v", calls, want)
	}
}

func TestMigrationRollbackPreservesStaticLegacyServiceState(t *testing.T) {
	oldSystemctl := runUserSystemctl
	t.Cleanup(func() { runUserSystemctl = oldSystemctl })
	var calls []string
	runUserSystemctl = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	state := legacyServiceState{Name: "frostgate-static.service", Active: true}
	if err := restoreLegacyService(state); err != nil {
		t.Fatal(err)
	}
	if want := []string{"start frostgate-static.service"}; !equalStrings(calls, want) {
		t.Fatalf("static legacy service lifecycle = %v, want %v", calls, want)
	}
}

func TestQuiesceFailureReturnsLegacyStateForRecovery(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
	legacyDir := filepath.Join(root, "legacy")
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	identity := profile.LegacyIdentity(filepath.Join(legacyDir, "config.yaml"))
	unitPath := setup.ServiceUnitPath(identity.Service)
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unitPath, []byte(setup.BuildSystemdUnit("/usr/bin/sing-box", legacyDir)), 0o600); err != nil {
		t.Fatal(err)
	}
	oldOutput := runUserSystemctlOutput
	oldSystemctl := runUserSystemctl
	t.Cleanup(func() { runUserSystemctlOutput, runUserSystemctl = oldOutput, oldSystemctl })
	runUserSystemctlOutput = func(args ...string) ([]byte, error) {
		if args[0] == "is-enabled" {
			return []byte("enabled\n"), nil
		}
		return []byte("active\n"), nil
	}
	var calls []string
	runUserSystemctl = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		if len(args) > 1 && args[0] == "disable" && args[1] == "--now" {
			return errors.New("disable partially failed")
		}
		return nil
	}
	state, err := quiesceLegacyService(legacyDir, identity)
	if err == nil || state == nil || !state.Enabled || !state.Active {
		t.Fatalf("quiesce failure lost prior state: state=%+v err=%v", state, err)
	}
	if restoreErr := restoreLegacyService(*state); restoreErr != nil {
		t.Fatal(restoreErr)
	}
	if want := []string{"disable --now " + identity.Service, "enable --now " + identity.Service}; !equalStrings(calls, want) {
		t.Fatalf("quiesce failure recovery lifecycle = %v, want %v", calls, want)
	}
}

func TestExistingCustomProfileCanUpdateWhileItsOwnListenersAreActive(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	identity := profile.ForPath(configPath, false)
	controllerHost, controllerPort, err := net.SplitHostPort(identity.ControllerAddr)
	if err != nil {
		t.Fatal(err)
	}
	benchmarkHost, benchmarkPort, err := net.SplitHostPort(identity.BenchmarkAddr)
	if err != nil {
		t.Fatal(err)
	}
	sidecar := fmt.Sprintf(`{"experimental":{"clash_api":{"external_controller":%q,"secret":"fixture-secret"}},"inbounds":[{"type":"mixed","tag":"frostgate-mixed","listen":%q,"listen_port":%s}],"route":{"rules":[{"inbound":"frostgate-mixed","outbound":"frostgate-selector"}]},"outbounds":[{"type":"direct","tag":"direct"},{"type":"selector","tag":"frostgate-selector","outbounds":["existing"],"default":"existing"},{"type":"vless","tag":"existing","server":"203.0.113.10","server_port":443,"uuid":"11111111-1111-4111-8111-111111111111","tls":{"enabled":true,"server_name":"example.com","reality":{"enabled":true,"public_key":"fixture-key","short_id":"abcd"}}}]}`, identity.ControllerAddr, benchmarkHost, benchmarkPort)
	if err := os.WriteFile(filepath.Join(dir, "sing-box.json"), []byte(sidecar), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "controller-secret"), []byte("fixture-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configData := fmt.Sprintf(`singbox:
  controller: http://%s
  secret_file: %s
  benchmark_proxy: %s
endpoints:
  - name: direct
    type: http
    url: https://www.gstatic.com/generate_204
    expected_status: 204
  - name: existing
    type: singbox
    outbound: existing
`, identity.ControllerAddr, filepath.Join(dir, "controller-secret"), identity.BenchmarkAddr)
	if err := os.WriteFile(configPath, []byte(configData), 0o600); err != nil {
		t.Fatal(err)
	}
	controllerListener, err := net.Listen("tcp", net.JoinHostPort(controllerHost, controllerPort))
	if err != nil {
		t.Fatal(err)
	}
	defer controllerListener.Close()
	benchmarkListener, err := net.Listen("tcp", net.JoinHostPort(benchmarkHost, benchmarkPort))
	if err != nil {
		t.Fatal(err)
	}
	defer benchmarkListener.Close()
	runner := &managedconfig.SidecarRunner{Validator: func([]byte) error { return nil }, Restart: func() error { return nil }, IsActive: func() (bool, error) { return true, nil }, ControllerReady: func([]string) (bool, error) { return true, nil }}
	if _, err := addManagedRoute(dir, configPath, "hysteria2://fixture-password@203.0.113.20:443?sni=example.com", "new-route", runner); err != nil {
		t.Fatalf("update custom profile with its own listeners active: %v", err)
	}
}

func TestCustomLegacyBenchmarkMigrationUsesProfileBindings(t *testing.T) {
	root := t.TempDir()
	defaultDir := filepath.Join(root, "default")
	customDir := filepath.Join(root, "profile")
	if err := os.MkdirAll(defaultDir, 0o700); err != nil {
		t.Fatal(err)
	}
	defaultSidecar := []byte(`{"outbounds":[{"type":"direct","tag":"direct"}]}`)
	if err := os.WriteFile(filepath.Join(defaultDir, "sing-box.json"), defaultSidecar, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(customDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(customDir, "config.yaml")
	legacy := []byte(`{"experimental":{"clash_api":{"external_controller":"127.0.0.1:9090","secret":"fixture-secret"}},"outbounds":[{"type":"direct","tag":"direct"},{"type":"block","tag":"block"},{"type":"vless","tag":"legacy-route","server":"203.0.113.10","server_port":443,"uuid":"11111111-1111-4111-8111-111111111111","tls":{"enabled":true,"server_name":"example.com","reality":{"enabled":true,"public_key":"fixture-key","short_id":"abcd"}}}]}`)
	if err := os.WriteFile(filepath.Join(customDir, "sing-box.json"), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(customDir, "controller-secret"), []byte("fixture-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configData := []byte(`singbox:
  controller: http://127.0.0.1:9090
  secret_file: ` + filepath.Join(customDir, "controller-secret") + `
endpoints:
  - name: direct
    type: http
    url: https://www.gstatic.com/generate_204
    expected_status: 204
  - name: legacy-route
    type: singbox
    outbound: legacy-route
`)
	if err := os.WriteFile(configPath, configData, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	runner := &managedconfig.SidecarRunner{Validator: func([]byte) error { return nil }, Restart: func() error { return nil }, IsActive: func() (bool, error) { return true, nil }, ControllerReady: func([]string) (bool, error) { return true, nil }}
	addr, reason := benchmarkProxyAddrWithRunner(cfg, configPath, customDir, runner)
	identity := profile.ForPath(configPath, false)
	if reason != "" || addr != identity.BenchmarkAddr {
		t.Fatalf("custom legacy migration = addr:%q reason:%q want:%q", addr, reason, identity.BenchmarkAddr)
	}
	updated, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if updated.SingBox.Controller != "http://"+identity.ControllerAddr || updated.SingBox.BenchmarkProxy != identity.BenchmarkAddr {
		t.Fatalf("custom profile bindings not persisted: %+v", updated.SingBox)
	}
	if got, err := os.ReadFile(filepath.Join(defaultDir, "sing-box.json")); err != nil || !bytes.Equal(got, defaultSidecar) {
		t.Fatalf("custom migration changed default sidecar: %q err=%v", got, err)
	}
}

func TestAddDoesNotBootstrapOverExternalController(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	configData := []byte(`singbox:
  controller: http://127.0.0.1:9999
endpoints:
  - name: direct
    type: http
    url: https://www.gstatic.com/generate_204
    expected_status: 204
  - name: external-route
    type: singbox
    outbound: external-route
`)
	if err := os.WriteFile(configPath, configData, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &managedconfig.SidecarRunner{Validator: func([]byte) error { t.Fatal("external controller must not be bootstrapped"); return nil }}
	if _, err := addManagedRoute(dir, configPath, "vless://11111111-1111-4111-8111-111111111111@203.0.113.10:443?security=reality&type=tcp&sni=example.com&pbk=fixture-key", "new-route", runner); err == nil || !strings.Contains(err.Error(), "existing external controller") {
		t.Fatalf("external controller bootstrap error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sing-box.json")); !os.IsNotExist(err) {
		t.Fatalf("external controller unexpectedly gained a managed sidecar: %v", err)
	}
	if got, err := os.ReadFile(configPath); err != nil || !bytes.Equal(got, configData) {
		t.Fatalf("external config changed: %q err=%v", got, err)
	}
}

func TestAddRefusesUnownedDefaultSystemdUnit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	dir := setup.ConfigDir(home)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	secretPath := filepath.Join(dir, "controller-secret")
	if err := os.WriteFile(secretPath, []byte("fixture-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sing-box.json"), []byte(existingManagedConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	configData := strings.ReplaceAll(existingFrostgateConfig, "REPLACED_SECRET_FILE", secretPath)
	if err := os.WriteFile(configPath, []byte(configData), 0o600); err != nil {
		t.Fatal(err)
	}
	unitPath := setup.ServiceUnitPath("frostgate-singbox.service")
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unitPath, []byte("[Unit]\nDescription=unrelated service\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	beforeConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeSidecar, err := os.ReadFile(filepath.Join(dir, "sing-box.json"))
	if err != nil {
		t.Fatal(err)
	}
	runner := &managedconfig.SidecarRunner{Validator: func([]byte) error {
		t.Fatal("unowned unit reached sidecar transaction")
		return nil
	}}
	if _, err := addManagedRoute(dir, configPath, "vless://11111111-1111-4111-8111-111111111111@203.0.113.10:443?security=reality&sni=example.com&pbk=fixture-key", "new-route", runner); err == nil || !strings.Contains(err.Error(), "does not prove Frostgate ownership") {
		t.Fatalf("unowned default unit error = %v", err)
	}
	if got, err := os.ReadFile(configPath); err != nil || !bytes.Equal(got, beforeConfig) {
		t.Fatalf("unowned unit changed Frostgate config: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "sing-box.json")); err != nil || !bytes.Equal(got, beforeSidecar) {
		t.Fatalf("unowned unit changed managed state: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(unitPath); err != nil || string(got) != "[Unit]\nDescription=unrelated service\n" {
		t.Fatalf("unowned unit changed: %q err=%v", got, err)
	}
}

func TestPrepareManagedStateAdoptsOnlyProvenLegacyProfile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg"))
	oldSystemctl, oldSystemctlOutput := runUserSystemctl, runUserSystemctlOutput
	var systemctlCalls [][]string
	runUserSystemctl = func(args ...string) error {
		systemctlCalls = append(systemctlCalls, append([]string(nil), args...))
		return nil
	}
	runUserSystemctlOutput = func(args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "is-enabled" {
			return []byte("enabled\n"), nil
		}
		return []byte("active\n"), nil
	}
	t.Cleanup(func() { runUserSystemctl, runUserSystemctlOutput = oldSystemctl, oldSystemctlOutput })
	configPath := filepath.Join(dir, "profile.yaml")
	legacyIdentity := profile.LegacyIdentity(configPath)
	configData := []byte(fmt.Sprintf(`singbox:
  controller: http://%s
  secret_file: %s
  benchmark_proxy: %s
endpoints:
  - name: direct
    type: http
    url: https://example.com/
  - name: route-a
    type: singbox
    outbound: route-a
`, legacyIdentity.ControllerAddr, filepath.Join(dir, "controller-secret"), legacyIdentity.BenchmarkAddr))
	if err := os.WriteFile(configPath, configData, 0o600); err != nil {
		t.Fatal(err)
	}
	legacyManaged := &managedconfig.Managed{Dir: dir, Secret: "legacy-secret"}
	legacyManaged.AddVLESS(&setup.VLESSOutbound{Tag: "route-a", Server: "203.0.113.10", ServerPort: 443, UUID: "11111111-1111-4111-8111-111111111111", SNI: "example.com", PBK: "fixture-key", FP: "chrome", Network: "tcp"})
	legacyManaged.ConfigureProfile("http://"+legacyIdentity.ControllerAddr, legacyIdentity.BenchmarkAddr)
	legacy, err := legacyManaged.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sing-box.json"), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "controller-secret"), []byte("legacy-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unitPath := setup.ServiceUnitPath(legacyIdentity.Service)
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unitPath, []byte(setup.BuildSystemdUnit("/usr/bin/sing-box", dir)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, state := commandPaths(configPath)
	rollback, _, err := prepareManagedStateWithRollback(configPath, state)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sing-box.json", "controller-secret"} {
		copied, err := os.ReadFile(filepath.Join(state, name))
		if err != nil {
			t.Fatalf("migrated %s missing: %v", name, err)
		}
		original, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !bytes.Equal(copied, original) {
			t.Fatalf("legacy %s was not preserved", name)
		}
	}
	if marker, err := os.ReadFile(filepath.Join(state, migrationOwnerFile)); err != nil || strings.TrimSpace(string(marker)) == "" {
		t.Fatalf("migration ownership marker missing: %q err=%v", marker, err)
	}
	if len(systemctlCalls) != 1 || strings.Join(systemctlCalls[0], " ") != "disable --now "+legacyIdentity.Service {
		t.Fatalf("legacy service was not quiesced: %v", systemctlCalls)
	}
	if err := rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("rollback retained migrated state: %v", err)
	}
	if len(systemctlCalls) != 2 || strings.Join(systemctlCalls[1], " ") != "enable --now "+legacyIdentity.Service {
		t.Fatalf("legacy service was not restored: %v", systemctlCalls)
	}
}

func TestPrepareManagedStateRejectsAmbiguousLegacySibling(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "a.yaml")
	other := filepath.Join(dir, "b.yaml")
	for _, path := range []string{configPath, other} {
		if err := os.WriteFile(path, []byte("endpoints: []\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "sing-box.json"), []byte(`{"outbounds":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, state := commandPaths(configPath)
	if err := prepareManagedState(configPath, state); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous legacy state was adopted: %v", err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("ambiguous migration created state: %v", err)
	}
}

func TestBenchmarkMigratesProvenLegacyCustomProfile(t *testing.T) {
	oldFind, oldWrite, oldRunner, oldSystemctl, oldSystemctlOutput := findSingBox, writeSystemdUnit, newSidecarRunner, runUserSystemctl, runUserSystemctlOutput
	t.Cleanup(func() {
		findSingBox, writeSystemdUnit, newSidecarRunner, runUserSystemctl, runUserSystemctlOutput = oldFind, oldWrite, oldRunner, oldSystemctl, oldSystemctlOutput
	})
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
	legacyDir := filepath.Join(root, "profile")
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(legacyDir, "config.yaml")
	legacyIdentity := profile.LegacyIdentity(configPath)
	if err := os.WriteFile(configPath, []byte(fmt.Sprintf(`singbox:
  controller: http://%s
  secret_file: %s
endpoints:
  - name: direct
    type: http
    url: https://example.com/
  - name: legacy-route
    type: singbox
    outbound: legacy-route
`, legacyIdentity.ControllerAddr, filepath.Join(legacyDir, "controller-secret"))), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy := &managedconfig.Managed{Dir: legacyDir, Secret: "fixture-secret"}
	legacy.AddVLESS(&setup.VLESSOutbound{Tag: "legacy-route", Server: "203.0.113.10", ServerPort: 443, UUID: "11111111-1111-4111-8111-111111111111", SNI: "example.com", PBK: "fixture-key", FP: "chrome", Network: "tcp"})
	legacy.ConfigureProfile("http://"+legacyIdentity.ControllerAddr, legacyIdentity.BenchmarkAddr)
	data, err := legacy.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "sing-box.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "controller-secret"), []byte("fixture-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unitPath := setup.ServiceUnitPath(legacyIdentity.Service)
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unitPath, []byte(setup.BuildSystemdUnit("/usr/bin/sing-box", legacyDir)), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls []string
	runUserSystemctl = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	runUserSystemctlOutput = func(args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "is-enabled" {
			return []byte("enabled\n"), nil
		}
		return []byte("active\n"), nil
	}
	findSingBox = func(string) (string, error) { return "/usr/bin/sing-box", nil }
	writeSystemdUnit = setup.WriteSystemdUnit
	newSidecarRunner = func(_, _, _ string) *managedconfig.SidecarRunner {
		return &managedconfig.SidecarRunner{
			Validator:       func([]byte) error { return nil },
			DaemonReload:    func() error { return nil },
			Enable:          func() error { return nil },
			Restart:         func() error { return nil },
			IsActive:        func() (bool, error) { return true, nil },
			ControllerReady: func([]string) (bool, error) { return true, nil },
		}
	}
	state := profile.StateDir(configPath)
	migrated, err := migrateLegacyProfileForBenchmark(configPath, state)
	if err != nil || !migrated {
		t.Fatalf("benchmark migration = %v, %v", migrated, err)
	}
	if got, want := calls, []string{"disable --now " + legacyIdentity.Service}; !equalStrings(got, want) {
		t.Fatalf("legacy service lifecycle = %v, want %v", got, want)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	identity := profile.ForPath(configPath, false)
	if cfg.SingBox.Controller != "http://"+identity.ControllerAddr || cfg.SingBox.BenchmarkProxy != identity.BenchmarkAddr || cfg.SingBox.SecretFile != filepath.Join(state, "controller-secret") {
		t.Fatalf("config retained legacy bindings: %+v", cfg.SingBox)
	}
	if _, err := os.Stat(filepath.Join(state, "sing-box.json")); err != nil {
		t.Fatalf("new profile state missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state, migrationOwnerFile)); !os.IsNotExist(err) {
		t.Fatalf("successful benchmark migration retained ownership marker: %v", err)
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func TestUnitExecStartMatchesOnlyActualLegacyConfig(t *testing.T) {
	configPath := "/tmp/frostgate profile/sing-box.json"
	good := setup.BuildSystemdUnit("/usr/bin/sing-box", "/tmp/frostgate profile")
	if !unitExecStartMatches(good, configPath) {
		t.Fatal("generated legacy unit was not recognized")
	}
	bad := "[Unit]\nDescription=has " + configPath + "\n[Service]\nExecStart=/usr/bin/other run -c \"/tmp/elsewhere/sing-box.json\"\n"
	if unitExecStartMatches(bad, configPath) {
		t.Fatal("unrelated unit was accepted from a comment")
	}
}
