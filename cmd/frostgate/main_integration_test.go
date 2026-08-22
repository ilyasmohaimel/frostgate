package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ilyasmohaimel/frostgate/internal/benchmark"
	"github.com/ilyasmohaimel/frostgate/internal/config"
	"github.com/ilyasmohaimel/frostgate/internal/history"
	"github.com/ilyasmohaimel/frostgate/internal/metrics"
	"github.com/ilyasmohaimel/frostgate/internal/probe"
)

var integrationBinary string

func TestMain(m *testing.M) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	dir, err := os.MkdirTemp("", "frostgate-cli-test-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	integrationBinary = filepath.Join(dir, "frostgate")
	cmd := exec.Command("go", "build", "-ldflags", "-X main.version=test", "-o", integrationBinary, "./cmd/frostgate")
	cmd.Dir = root
	if out, buildErr := cmd.CombinedOutput(); buildErr != nil {
		fmt.Fprintf(os.Stderr, "build integration binary: %v\n%s", buildErr, out)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

type cliFixture struct {
	home            string
	configPath      string
	benchmarkHits   atomic.Int32
	destinationHits atomic.Int32
	downloadBytes   int64
	secretToken     string
	controllerURL   string
	close           func()
}

func newCLIFixture(t *testing.T) *cliFixture {
	t.Helper()
	f := &cliFixture{home: t.TempDir(), downloadBytes: 1234567, secretToken: "fixture-query-token"}
	content := bytes.Repeat([]byte("x"), int(f.downloadBytes))
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/generate_204":
			w.WriteHeader(http.StatusOK)
		case "/custom":
			if r.URL.Query().Get("bytes") == strconv.FormatInt(f.downloadBytes, 10) {
				f.benchmarkHits.Add(1)
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(content)))
			_, _ = w.Write(content)
		case "/custom-destination":
			f.destinationHits.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))

	var selectorMu sync.Mutex
	selected := "proxy-out"
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		selectorMu.Lock()
		defer selectorMu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/proxies":
			_ = json.NewEncoder(w).Encode(map[string]any{"proxies": map[string]any{
				"proxy-out":          map[string]any{"type": "vless"},
				"frostgate-selector": map[string]any{"type": "selector", "now": selected},
			}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/delay"):
			_ = json.NewEncoder(w).Encode(map[string]int{"delay": 7})
		case r.Method == http.MethodGet && r.URL.Path == "/proxies/frostgate-selector":
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "selector", "now": selected})
		case r.Method == http.MethodPut && r.URL.Path == "/proxies/frostgate-selector":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["name"] != "proxy-out" {
				http.NotFound(w, r)
				return
			}
			selected = body["name"]
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	f.controllerURL = controller.URL

	socksAddr, stopSOCKS := startSOCKSRelay(t)
	xdgConfig := filepath.Join(f.home, "xdg-config")
	xdgData := filepath.Join(f.home, "xdg-data")
	configDir := filepath.Join(xdgConfig, "frostgate")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	f.configPath = filepath.Join(configDir, "config.yaml")
	yaml := fmt.Sprintf(`defaults:
  timeout: 500ms
  samples: 1
  sample_interval: 1ms
singbox:
  controller: %s
  test_url: %s/generate_204
  benchmark_proxy: %s
benchmark:
  download_bytes: %d
  download_url: %s/custom?token=%s
history:
  enabled: false
  retention: 17d
  watch_interval: 13m
network_check:
  destinations:
    - name: custom-test
      url: %s/custom-destination
endpoints:
  - name: direct
    type: http
    url: %s/generate_204
    expected_status: 204
  - name: proxy-display-name
    type: singbox
    outbound: proxy-out
`, controller.URL, target.URL, socksAddr, f.downloadBytes, target.URL, f.secretToken, target.URL, target.URL)
	if err := os.WriteFile(f.configPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	f.close = func() {
		stopSOCKS()
		controller.Close()
		target.Close()
	}
	t.Cleanup(f.close)
	t.Setenv("HOME", f.home)
	t.Setenv("FROSTGATE_TEST_HOME", f.home)
	t.Setenv("XDG_CONFIG_HOME", xdgConfig)
	t.Setenv("XDG_DATA_HOME", xdgData)
	t.Setenv("FROSTGATE_TEST_SKIP_BASE_NETWORK_CHECKS", "1")
	return f
}

func (f *cliFixture) env() []string {
	return []string{
		"HOME=" + f.home,
		"FROSTGATE_TEST_HOME=" + f.home,
		"XDG_CONFIG_HOME=" + filepath.Join(f.home, "xdg-config"),
		"XDG_DATA_HOME=" + filepath.Join(f.home, "xdg-data"),
		"FROSTGATE_TEST_SKIP_BASE_NETWORK_CHECKS=1",
	}
}

func runCLI(t *testing.T, env []string, args ...string) ([]byte, []byte, int) {
	t.Helper()
	cmd := exec.Command(integrationBinary, args...)
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run %v: %v\nstderr: %s", args, err, stderr.String())
		}
		code = exitErr.ExitCode()
	}
	return stdout.Bytes(), stderr.Bytes(), code
}

func requireJSON(t *testing.T, out []byte) any {
	t.Helper()
	var value any
	if err := json.Unmarshal(out, &value); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	return value
}

func TestConfigContractAffectsRuntime(t *testing.T) {
	f := newCLIFixture(t)
	env := f.env()

	stdout, _, _ := runCLI(t, env, "--json")
	doc := requireJSON(t, stdout).(map[string]any)
	direct := doc["direct"].(map[string]any)
	if direct["status"] != "down" || direct["error"] == nil {
		t.Fatalf("expected_status=204 must reject actual 200: %v", direct)
	}
	if _, err := os.Stat(filepath.Join(f.home, "xdg-data", "frostgate", "history.db")); !os.IsNotExist(err) {
		t.Fatalf("history.enabled=false created a DB: %v", err)
	}

	stdout, _, _ = runCLI(t, env, "benchmark", "--json")
	benchmarkDoc := requireJSON(t, stdout).(map[string]any)
	if bytes.Contains(stdout, []byte(f.secretToken)) {
		t.Fatal("benchmark JSON leaked the configured URL query token")
	}
	measured := false
	for _, raw := range benchmarkDoc["results"].([]any) {
		entry := raw.(map[string]any)
		result := entry["result"].(map[string]any)
		if result["name"] == "proxy-display-name" && entry["download_mbps"].(float64) > 0 {
			measured = true
		}
	}
	if !measured {
		t.Fatalf("configured proxy route was not attributed a throughput result: %v", benchmarkDoc["results"])
	}
	if f.benchmarkHits.Load() == 0 {
		t.Fatal("benchmark did not use configured download_url with configured download_bytes through benchmark_proxy")
	}

	stdout, _, code := runCLI(t, env, "network-check", "--json")
	if code != 0 {
		t.Fatalf("network-check exit=%d", code)
	}
	networkDoc := requireJSON(t, stdout).(map[string]any)
	destinations := networkDoc["destinations"].([]any)
	if len(destinations) != 1 || destinations[0] != "custom-test" {
		t.Fatalf("custom destinations not authoritative: %v", destinations)
	}
	if f.destinationHits.Load() == 0 {
		t.Fatal("custom network destination was not requested")
	}
	if _, err := os.Stat(filepath.Join(f.home, "xdg-data", "frostgate", "history.db")); !os.IsNotExist(err) {
		t.Fatalf("disabled history created a DB after runtime commands: %v", err)
	}
}

func TestUnknownRootCommandsFail(t *testing.T) {
	for _, command := range []string{"recomemd", "frobnicate"} {
		stdout, stderr, code := runCLI(t, nil, command)
		if code != 3 || len(stdout) != 0 || !strings.Contains(string(stderr), `unknown command "`+command+`"`) {
			t.Fatalf("%s: code=%d stdout=%q stderr=%q", command, code, stdout, stderr)
		}
	}
	stdout, stderr, code := runCLI(t, nil, "--json", "recomemd")
	if code != 3 || len(stdout) != 0 || !strings.Contains(string(stderr), `unknown command "recomemd"`) {
		t.Fatalf("global flags before unknown command: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestUnknownCommandRedactsShareURL(t *testing.T) {
	link := "vless://11111111-1111-4111-8111-111111111111@203.0.113.10:443?security=reality&pbk=SUPER_SECRET_TOKEN_8F3A"
	stdout, stderr, code := runCLI(t, nil, link)
	if code != 3 || len(stdout) != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if leaked := findSecret(string(stderr), "11111111-1111-4111-8111-111111111111", "SUPER_SECRET_TOKEN_8F3A"); leaked != "" {
		t.Fatalf("unknown command leaked %q: %s", leaked, stderr)
	}
	if !strings.Contains(string(stderr), "unknown command") || !strings.Contains(string(stderr), "REDACTED") {
		t.Fatalf("unknown command was not usefully redacted: %q", stderr)
	}
}

func TestDoctorRejectsMalformedArguments(t *testing.T) {
	for _, args := range [][]string{{"doctor", "--wat"}, {"doctor", "--config"}, {"doctor", "unexpected-positional"}} {
		stdout, stderr, code := runCLI(t, nil, args...)
		if code != 3 || len(stdout) != 0 || len(stderr) == 0 {
			t.Fatalf("%v: code=%d stdout=%q stderr=%q", args, code, stdout, stderr)
		}
	}
	stdout, stderr, code := runCLI(t, nil, "doctor", "--help")
	if code != 0 || !strings.Contains(string(stdout), "usage: frostgate doctor") || len(stderr) != 0 {
		t.Fatalf("doctor --help: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestEmitRedactsVerboseAndJSONErrors(t *testing.T) {
	secret := "SUPER_SECRET_TOKEN_8F3A"
	raw := "request failed vless://11111111-1111-4111-8111-111111111111@203.0.113.10:443?security=reality&pbk=fixture-key#route " +
		"hysteria2://fixture-hysteria-password@203.0.113.20:443?obfs-password=fixture-obfs " +
		"https://example.com/sub?token=" + secret + " Bearer fixture-bearer-token"
	result := probe.Result{
		Endpoint: config.Endpoint{Name: "route", Type: config.ProbeSingBox},
		Status:   probe.StatusDown,
		Err:      errors.New(raw),
		ErrMsg:   raw,
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	outR, outW, errOutR, errOutW, err := openOutputCapture()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = outW, errOutW
	emit([]probe.Result{result}, false, true)
	_ = outW.Close()
	_ = errOutW.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	humanOut, _ := io.ReadAll(outR)
	humanErr, _ := io.ReadAll(errOutR)
	if leaked := findSecret(string(humanOut)+string(humanErr), secret, "11111111-1111-4111-8111-111111111111", "fixture-hysteria-password", "fixture-obfs", "fixture-bearer-token"); leaked != "" {
		t.Fatalf("verbose output leaked %q: stdout=%q stderr=%q", leaked, humanOut, humanErr)
	}

	outR, outW, errOutR, errOutW, err = openOutputCapture()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = outW, errOutW
	emit([]probe.Result{result}, true, true)
	_ = outW.Close()
	_ = errOutW.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	jsonOut, _ := io.ReadAll(outR)
	jsonErr, _ := io.ReadAll(errOutR)
	if leaked := findSecret(string(jsonOut)+string(jsonErr), secret, "11111111-1111-4111-8111-111111111111", "fixture-hysteria-password", "fixture-obfs", "fixture-bearer-token"); leaked != "" {
		t.Fatalf("JSON output leaked %q: stdout=%q stderr=%q", leaked, jsonOut, jsonErr)
	}
	if !bytes.Contains(jsonOut, []byte("REDACTED")) {
		t.Fatalf("JSON error was not useful and redacted: %s", jsonOut)
	}
}

func TestBenchmarkRestoreFailureInvalidatesThroughput(t *testing.T) {
	results := []benchmark.RouteThroughput{{Route: "proxy-out", DownloadMbps: 87.5}}
	invalidateBenchmarkThroughput(results, errors.New("restore selector \"frostgate-selector\": refused"))
	if results[0].DownloadMbps != 0 || results[0].NAReason != "selector restoration failed" {
		t.Fatalf("restore failure must not be reported as throughput: %+v", results[0])
	}
}

func openOutputCapture() (*os.File, *os.File, *os.File, *os.File, error) {
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = outR.Close()
		_ = outW.Close()
		return nil, nil, nil, nil, err
	}
	return outR, outW, errR, errW, nil
}

func findSecret(value string, secrets ...string) string {
	for _, secret := range secrets {
		if strings.Contains(value, secret) {
			return secret
		}
	}
	return ""
}

func TestHistoryAcceptsConfigFlag(t *testing.T) {
	stdout, stderr, code := runCLI(t, nil, "history", "--config", "fixture.yaml")
	if code != 0 || !strings.Contains(string(stdout), "No history") || len(stderr) != 0 {
		t.Fatalf("history config flag failed: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestRecommendHistoryIsScopedToExplicitConfigProfile(t *testing.T) {
	f := newCLIFixture(t)
	profileA := filepath.Join(f.home, "profiles", "a", "config.yaml")
	profileB := filepath.Join(f.home, "profiles", "b", "config.yaml")
	for _, path := range []string{profileA, profileB} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("endpoints: []\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	db, err := history.Open()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		if err := db.Add(history.Record{Route: "same-name", Status: "up", MedianMs: 20, Profile: historyProfileForConfig(profileB)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	enabled := true
	cfg := &config.Config{History: config.HistoryConfig{Enabled: &enabled}}
	results := []probe.Result{{Endpoint: config.Endpoint{Name: "same-name", Type: config.ProbeSingBox}, Status: probe.StatusUp, Metrics: metrics.Metrics{Median: 100 * time.Millisecond}}}
	fromA := recommendFromResults(cfg, results, profileA)
	fromB := recommendFromResults(cfg, results, profileB)
	if fromA.BestOverall.HistoryCount != 0 || fromA.Confidence != "low" {
		t.Fatalf("profile A borrowed profile B history: %+v", fromA)
	}
	if fromB.BestOverall.HistoryCount != 12 || fromB.Confidence != "medium" {
		t.Fatalf("profile B did not see its history: %+v", fromB)
	}
}

func TestCustomConfigKeepsManagedSidecarsIsolated(t *testing.T) {
	f := newCLIFixture(t)
	defaultSidecar := []byte(`{"outbounds":[{"type":"vless","tag":"default-route","server":"203.0.113.10","server_port":443,"uuid":"11111111-1111-4111-8111-111111111111","tls":{"enabled":true,"server_name":"example.com","reality":{"enabled":true,"public_key":"fixture-key"}}}]}`)
	defaultSidecarPath := filepath.Join(filepath.Dir(f.configPath), "sing-box.json")
	if err := os.WriteFile(defaultSidecarPath, defaultSidecar, 0o600); err != nil {
		t.Fatal(err)
	}
	customDir := filepath.Join(f.home, "profiles", "custom")
	if err := os.MkdirAll(customDir, 0o700); err != nil {
		t.Fatal(err)
	}
	customConfig := filepath.Join(customDir, "config.yaml")
	data, err := os.ReadFile(f.configPath)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.ReplaceAll(data, []byte("proxy-display-name"), []byte("custom-hysteria"))
	data = bytes.ReplaceAll(data, []byte("proxy-out"), []byte("custom-hysteria"))
	if err := os.WriteFile(customConfig, data, 0o600); err != nil {
		t.Fatal(err)
	}
	customSidecar := []byte(`{"outbounds":[{"type":"selector","tag":"frostgate-selector","outbounds":["custom-hysteria"],"default":"custom-hysteria"},{"type":"hysteria2","tag":"custom-hysteria","server":"203.0.113.20","server_port":443,"password":"fixture-password","tls":{"enabled":true,"server_name":"example.com"}}]}`)
	if err := os.WriteFile(filepath.Join(customDir, "sing-box.json"), customSidecar, 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runCLI(t, f.env(), "diagnose", "--json", "custom-hysteria", "--config", customConfig)
	if code != 0 {
		t.Fatalf("diagnose exit=%d stderr=%s", code, stderr)
	}
	layers := requireJSON(t, stdout).([]any)
	foundManagedHysteria := false
	for _, raw := range layers {
		layer := raw.(map[string]any)
		if layer["name"] == "Server custom-hysteria" && strings.Contains(layer["detail"].(string), "not independently verified") {
			foundManagedHysteria = true
		}
	}
	if !foundManagedHysteria {
		t.Fatalf("diagnose did not load custom managed sidecar: %v", layers)
	}

	stdout, stderr, code = runCLI(t, f.env(), "network-check", "--json", "--config", customConfig)
	if code != 0 {
		t.Fatalf("network-check exit=%d stderr=%s", code, stderr)
	}
	doc := requireJSON(t, stdout).(map[string]any)
	matrix := doc["matrix"].([]any)
	if len(matrix) == 0 || len(matrix[0].(map[string]any)["routes"].([]any)) != 1 || matrix[0].(map[string]any)["routes"].([]any)[0].(map[string]any)["kind"] != "hysteria2" {
		t.Fatalf("network-check did not load custom managed sidecar: %v", doc["matrix"])
	}
	gotDefault, err := os.ReadFile(defaultSidecarPath)
	if err != nil || !bytes.Equal(gotDefault, defaultSidecar) {
		t.Fatalf("custom config commands changed default sidecar: err=%v", err)
	}
}

func TestBenchmarkDiscoversManagedProxyFromPreV042Config(t *testing.T) {
	f := newCLIFixture(t)
	configData, err := os.ReadFile(f.configPath)
	if err != nil {
		t.Fatal(err)
	}
	proxyAddr := extractBenchmarkProxy(configData)
	_, proxyPort, err := net.SplitHostPort(proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	configData = bytes.Replace(configData, []byte("  benchmark_proxy: "+proxyAddr+"\n"), nil, 1)
	if err := os.WriteFile(f.configPath, configData, 0o600); err != nil {
		t.Fatal(err)
	}
	managed := []byte(`{
  "experimental": {"clash_api": {"external_controller": "` + strings.TrimPrefix(f.controllerURL, "http://") + `"}},
  "inbounds": [{"type": "mixed", "tag": "frostgate-mixed", "listen": "127.0.0.1", "listen_port": ` + proxyPort + `}],
  "route": {"rules": [{"inbound": "frostgate-mixed", "outbound": "frostgate-selector"}], "final": "direct"},
  "outbounds": [
    {"type": "direct", "tag": "direct"},
    {"type": "selector", "tag": "frostgate-selector", "outbounds": ["proxy-out"]},
    {"type": "vless", "tag": "proxy-out", "server": "203.0.113.10", "server_port": 443, "uuid": "11111111-1111-4111-8111-111111111111", "tls": {"enabled": true, "server_name": "example.com", "reality": {"enabled": true, "public_key": "fixture-key"}}}
  ]
}`)
	if err := os.WriteFile(filepath.Join(filepath.Dir(f.configPath), "sing-box.json"), managed, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _, _ := runCLI(t, f.env(), "benchmark", "--json")
	doc := requireJSON(t, stdout).(map[string]any)
	for _, raw := range doc["results"].([]any) {
		entry := raw.(map[string]any)
		result := entry["result"].(map[string]any)
		if result["name"] != "proxy-display-name" {
			continue
		}
		if entry["download_mbps"].(float64) <= 0 || entry["download_na"] != nil {
			t.Fatalf("managed benchmark listener was not used: %v", entry)
		}
		if f.benchmarkHits.Load() == 0 {
			t.Fatal("managed benchmark listener did not carry the download request")
		}
		return
	}
	t.Fatal("proxy result missing")
}

func TestBenchmarkDoesNotGuessProxyForExternalController(t *testing.T) {
	f := newCLIFixture(t)
	configData, err := os.ReadFile(f.configPath)
	if err != nil {
		t.Fatal(err)
	}
	proxyAddr := extractBenchmarkProxy(configData)
	configData = bytes.Replace(configData, []byte("  benchmark_proxy: "+proxyAddr+"\n"), nil, 1)
	if err := os.WriteFile(f.configPath, configData, 0o600); err != nil {
		t.Fatal(err)
	}
	external := []byte(`{
  "experimental": {"clash_api": {"external_controller": "127.0.0.1:9999"}},
  "outbounds": [
    {"type": "direct", "tag": "direct"},
    {"type": "vless", "tag": "proxy-out", "server": "203.0.113.10", "server_port": 443, "uuid": "11111111-1111-4111-8111-111111111111", "tls": {"enabled": true, "server_name": "example.com", "reality": {"enabled": true, "public_key": "fixture-key"}}}
  ]
}`)
	if err := os.WriteFile(filepath.Join(filepath.Dir(f.configPath), "sing-box.json"), external, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _, _ := runCLI(t, f.env(), "benchmark", "--json")
	doc := requireJSON(t, stdout).(map[string]any)
	for _, raw := range doc["results"].([]any) {
		entry := raw.(map[string]any)
		result := entry["result"].(map[string]any)
		if result["name"] == "proxy-display-name" {
			if entry["download_na"] != "benchmark_proxy not configured" || entry["download_mbps"].(float64) != 0 {
				t.Fatalf("external controller was assigned a guessed listener: %v", entry)
			}
			if f.benchmarkHits.Load() != 0 {
				t.Fatal("external controller benchmark made a throughput request")
			}
			return
		}
	}
	t.Fatal("proxy result missing")
}

func TestBenchmarkHumanOutputIsCompact(t *testing.T) {
	f := newCLIFixture(t)
	stdout, _, _ := runCLI(t, f.env(), "benchmark", "--no-speed")
	output := string(stdout)
	if strings.Contains(output, "✓") || strings.Contains(output, "BEST PROXY:") {
		t.Fatalf("benchmark retained decorative formatting:\n%s", output)
	}
	if strings.Count(output, "Best proxy:") != 1 {
		t.Fatalf("benchmark duplicated its proxy summary:\n%s", output)
	}
	if !strings.Contains(output, "Why: ") || !strings.Contains(output, " - ") {
		t.Fatalf("benchmark reasons are not compact:\n%s", output)
	}
}

func extractBenchmarkProxy(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "benchmark_proxy:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "benchmark_proxy:"))
		}
	}
	return ""
}

func TestActualCLIJSONContracts(t *testing.T) {
	f := newCLIFixture(t)
	env := f.env()

	stdout, _, code := runCLI(t, env, "--version")
	if code != 0 || strings.TrimSpace(string(stdout)) != "frostgate test" {
		t.Fatalf("--version: code=%d stdout=%q", code, stdout)
	}
	_, stderr, code := runCLI(t, env, "--help")
	if code != 0 || !bytes.Contains(stderr, []byte("Usage:")) {
		t.Fatalf("--help: code=%d stderr=%q", code, stderr)
	}

	commands := [][]string{
		{"--json"},
		{"history", "--json"},
		{"compare", "--json"},
		{"recommend", "--json"},
		{"network-check", "--json"},
		{"diagnose", "--json"},
	}
	for _, args := range commands {
		args := args
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			stdout, _, _ := runCLI(t, env, args...)
			requireJSON(t, stdout)
		})
	}
}

func TestReadmeDemoAssetsExist(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"README.md", "assets/frostgate-demo.gif", "docs/frostgate-demo.tape", "docs/frostgate-demo-env.sh", "docs/demo-bin/bash"} {
		info, err := os.Stat(filepath.Join(root, path))
		if err != nil || info.Size() == 0 {
			t.Fatalf("README demo asset %s missing or empty: info=%v err=%v", path, info, err)
		}
	}
	if info, err := os.Stat(filepath.Join(root, "docs/demo-bin/bash")); err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("README demo shell wrapper is not executable: info=%v err=%v", info, err)
	}
	readme, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(readme, []byte("assets/frostgate-demo.gif")) {
		t.Fatal("README does not reference the generated demo GIF")
	}
}

func TestCLIFlagOrderParity(t *testing.T) {
	f := newCLIFixture(t)
	env := f.env()
	for _, args := range [][]string{
		{"test", "--json", "not-a-link"},
		{"test", "not-a-link", "--json"},
		{"diagnose", "--json", "missing-route"},
		{"diagnose", "missing-route", "--json"},
		{"add", "--name", "foo", "not-a-link"},
		{"add", "not-a-link", "--name", "foo"},
	} {
		stdout, stderr, _ := runCLI(t, env, args...)
		if bytes.Contains(stderr, []byte("unknown flag")) || bytes.Contains(stderr, []byte("usage:")) {
			t.Fatalf("flag order rejected for %v: %s", args, stderr)
		}
		if args[0] == "diagnose" {
			requireJSON(t, stdout)
		}
		if args[0] == "test" {
			requireJSON(t, stdout)
		}
	}
}

func TestCLIRejectsUnsafeExplicitRouteName(t *testing.T) {
	f := newCLIFixture(t)
	_, stderr, code := runCLI(t, f.env(), "add", "--name", "../unsafe", "not-a-link")
	if code != 3 || !bytes.Contains(stderr, []byte("invalid route name")) {
		t.Fatalf("unsafe name was not rejected: code=%d stderr=%q", code, stderr)
	}
}

func TestCompareJSONDeterministicAndProxyOnly(t *testing.T) {
	f := newCLIFixture(t)
	db, err := history.Open()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, rec := range []history.Record{
		{Timestamp: now, Route: "direct", Type: string(config.ProbeHTTP), Status: "up", MedianMs: 1, Network: "z-network"},
		{Timestamp: now, Route: "proxy-b", Type: string(config.ProbeSingBox), Status: "up", MedianMs: 90, Network: "z-network"},
		{Timestamp: now, Route: "proxy-a", Type: string(config.ProbeSingBox), Status: "up", MedianMs: 80, Network: "a-network"},
		{Timestamp: now, Route: "direct", Type: string(config.ProbeHTTP), Status: "up", MedianMs: 1, Network: "a-network"},
	} {
		if err := db.Add(rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	stdout, _, code := runCLI(t, f.env(), "compare", "--json")
	if code != 0 {
		t.Fatalf("compare exit=%d", code)
	}
	doc := requireJSON(t, stdout).([]any)
	if len(doc) != 2 || doc[0].(map[string]any)["network"] != "a-network" || doc[1].(map[string]any)["network"] != "z-network" {
		t.Fatalf("networks are not deterministic: %v", doc)
	}
	for _, rawNetwork := range doc {
		network := rawNetwork.(map[string]any)
		if network["best_proxy"] == "direct" || network["best_proxy"] == nil {
			t.Fatalf("direct won best_proxy or no proxy selected: %v", network)
		}
		routes := network["routes"].([]any)
		for i := 1; i < len(routes); i++ {
			prev := routes[i-1].(map[string]any)["route"].(string)
			cur := routes[i].(map[string]any)["route"].(string)
			if prev > cur {
				t.Fatalf("routes are not sorted: %v", routes)
			}
		}
	}
}

func TestWatchJSONIsNDJSON(t *testing.T) {
	f := newCLIFixture(t)
	cmd := exec.Command(integrationBinary, "--watch", "--json", "--interval", "30ms")
	cmd.Env = append(os.Environ(), f.env()...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(220 * time.Millisecond)
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watch exit: %v\nstderr: %s", err, stderr.String())
		}
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("watch did not stop after interrupt")
	}
	scanner := bufio.NewScanner(bytes.NewReader(stdout.Bytes()))
	lines := 0
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		requireJSON(t, line)
		lines++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if lines < 2 {
		t.Fatalf("expected multiple NDJSON snapshots, got %d: %s", lines, stdout.String())
	}
}

func startSOCKSRelay(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			go relaySOCKS(conn)
		}
	}()
	return ln.Addr().String(), func() {
		_ = ln.Close()
	}
}

func relaySOCKS(client net.Conn) {
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(10 * time.Second))
	head := make([]byte, 2)
	if _, err := io.ReadFull(client, head); err != nil || head[0] != 5 {
		return
	}
	methods := make([]byte, int(head[1]))
	if _, err := io.ReadFull(client, methods); err != nil {
		return
	}
	if _, err := client.Write([]byte{5, 0}); err != nil {
		return
	}
	req := make([]byte, 4)
	if _, err := io.ReadFull(client, req); err != nil || req[1] != 1 {
		return
	}
	var host string
	switch req[3] {
	case 1:
		addr := make([]byte, net.IPv4len)
		if _, err := io.ReadFull(client, addr); err != nil {
			return
		}
		host = net.IP(addr).String()
	case 3:
		length := make([]byte, 1)
		if _, err := io.ReadFull(client, length); err != nil {
			return
		}
		name := make([]byte, int(length[0]))
		if _, err := io.ReadFull(client, name); err != nil {
			return
		}
		host = string(name)
	case 4:
		addr := make([]byte, net.IPv6len)
		if _, err := io.ReadFull(client, addr); err != nil {
			return
		}
		host = net.IP(addr).String()
	default:
		return
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(client, portBytes); err != nil {
		return
	}
	upstream, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(portBytes)))), 2*time.Second)
	if err != nil {
		_, _ = client.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstream.Close()
	if _, err := client.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	_ = client.SetDeadline(time.Time{})
	done := make(chan struct{}, 1)
	go func() {
		_, _ = io.Copy(upstream, client)
		_ = upstream.(*net.TCPConn).CloseWrite()
		done <- struct{}{}
	}()
	_, _ = io.Copy(client, upstream)
	<-done
}
