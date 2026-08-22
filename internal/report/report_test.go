package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ilyasmohaimel/frostgate/internal/config"
	"github.com/ilyasmohaimel/frostgate/internal/metrics"
	"github.com/ilyasmohaimel/frostgate/internal/probe"
)

func mkResult(name string, typ config.ProbeType, status probe.Status, median time.Duration, loss float64) probe.Result {
	return probe.Result{
		Endpoint: config.Endpoint{Name: name, Type: typ, Priority: 10},
		Status:   status,
		Metrics: metrics.Metrics{
			Attempted: 4,
			Succeeded: 4,
			Failed:    0,
			LossPct:   loss,
			Median:    median,
			Jitter:    5 * time.Millisecond,
			Min:       median - 10*time.Millisecond,
			Max:       median + 10*time.Millisecond,
			Mean:      median,
		},
		Category: "",
	}
}

func TestJSONSchema(t *testing.T) {
	results := []probe.Result{
		mkResult("primary", config.ProbeSingBox, probe.StatusUp, 120*time.Millisecond, 0),
		{Endpoint: config.Endpoint{Name: "down-route", Type: config.ProbeTCP}, Status: probe.StatusDown, Metrics: metrics.Metrics{Attempted: 4, Failed: 4, LossPct: 100}, Category: "timeout", ErrMsg: "timeout"},
	}
	var buf bytes.Buffer
	if err := JSON(&buf, results); err != nil {
		t.Fatalf("JSON: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc["schema_version"] != float64(3) {
		t.Errorf("schema_version %v want 3", doc["schema_version"])
	}
	if _, ok := doc["timestamp"]; !ok {
		t.Error("missing timestamp")
	}
	resultsRaw, ok := doc["results"].([]any)
	if !ok || len(resultsRaw) != 2 {
		t.Fatalf("results len %v", doc["results"])
	}
	first := resultsRaw[0].(map[string]any)
	if first["name"] != "primary" {
		t.Errorf("first name %v", first["name"])
	}
	if first["median_ms"] == nil {
		t.Error("missing median_ms")
	}
	// ensure no secret fields
	s := buf.String()
	if strings.Contains(s, "secret") || strings.Contains(s, "uuid") || strings.Contains(s, "private_key") {
		t.Error("json leaks secret")
	}
	if doc["best"] != "primary" {
		t.Errorf("best %v want primary", doc["best"])
	}
}

func TestNDJSONLine(t *testing.T) {
	results := []probe.Result{mkResult("a", config.ProbeTCP, probe.StatusUp, 50*time.Millisecond, 0)}
	line, err := NDJSONLine(results)
	if err != nil {
		t.Fatalf("NDJSON: %v", err)
	}
	if bytes.Contains(line, []byte("\n")) {
		t.Error("NDJSON line should not contain newline")
	}
	var doc map[string]any
	if err := json.Unmarshal(line, &doc); err != nil {
		t.Fatalf("unmarshal ndjson: %v", err)
	}
	if doc["schema_version"] != float64(3) {
		t.Error("wrong schema")
	}
}

func TestJSONDirectVsBestProxy(t *testing.T) {
	results := []probe.Result{
		mkResult("server-tcp", config.ProbeTCP, probe.StatusUp, 1*time.Millisecond, 0),
		mkResult("direct", config.ProbeHTTP, probe.StatusUp, 23*time.Millisecond, 0),
		mkResult("reality", config.ProbeSingBox, probe.StatusUp, 180*time.Millisecond, 0),
		mkResult("hysteria", config.ProbeSingBox, probe.StatusUp, 171*time.Millisecond, 0),
	}
	var buf bytes.Buffer
	if err := JSON(&buf, results); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	direct, ok := doc["direct"].(map[string]any)
	if !ok || direct["name"] != "direct" {
		t.Errorf("direct field wrong: %v", doc["direct"])
	}
	bp, ok := doc["best_proxy"].(map[string]any)
	if !ok || bp["name"] != "hysteria" {
		t.Errorf("best_proxy should be hysteria (lower median), got %v", doc["best_proxy"])
	}
	if doc["best"] != "server-tcp" {
		t.Errorf("legacy best should remain overall best, got %v", doc["best"])
	}
}

func TestJSONNoProxy(t *testing.T) {
	results := []probe.Result{mkResult("direct", config.ProbeHTTP, probe.StatusUp, 20*time.Millisecond, 0)}
	var buf bytes.Buffer
	if err := JSON(&buf, results); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	json.Unmarshal(buf.Bytes(), &doc)
	if _, ok := doc["best_proxy"]; ok {
		t.Errorf("best_proxy must be absent with no proxy routes")
	}
}

func TestTableDirectAndBestProxy(t *testing.T) {
	results := []probe.Result{
		mkResult("direct", config.ProbeHTTP, probe.StatusUp, 23*time.Millisecond, 0),
		mkResult("hysteria", config.ProbeSingBox, probe.StatusUp, 171*time.Millisecond, 0),
	}
	s := Table(results)
	if !strings.Contains(s, "Direct baseline:") {
		t.Errorf("missing direct baseline line: %q", s)
	}
	if !strings.Contains(s, "Best proxy: hysteria") {
		t.Errorf("missing best proxy line: %q", s)
	}
}

func TestTableNoProxyConfigured(t *testing.T) {
	results := []probe.Result{mkResult("direct", config.ProbeHTTP, probe.StatusUp, 20*time.Millisecond, 0)}
	s := Table(results)
	if strings.Contains(s, "Best proxy") {
		t.Errorf("no-proxy table must use legacy best route view: %q", s)
	}
}

func TestTableContainsBest(t *testing.T) {
	results := []probe.Result{
		mkResult("fast", config.ProbeTCP, probe.StatusUp, 50*time.Millisecond, 0),
		mkResult("slow", config.ProbeTCP, probe.StatusUp, 100*time.Millisecond, 0),
	}
	s := Table(results)
	if !strings.Contains(s, "fast") {
		t.Error("table missing fast")
	}
	if !strings.Contains(s, "best route: fast") {
		t.Errorf("table missing best: %q", s)
	}
}

func TestTableDownCase(t *testing.T) {
	results := []probe.Result{
		{Endpoint: config.Endpoint{Name: "dead", Type: config.ProbeTCP}, Status: probe.StatusDown, Metrics: metrics.Metrics{Attempted: 4, Failed: 4, LossPct: 100}, Category: "timeout"},
	}
	s := Table(results)
	if !strings.Contains(s, "all routes down") {
		t.Errorf("expected all routes down, got %q", s)
	}
	if !strings.Contains(s, "hint: all routes down; check internet and proxy config") {
		t.Errorf("expected plain diagnosis hint, got %q", s)
	}
	if !strings.Contains(s, "timeout") {
		t.Errorf("expected category timeout in table, got %q", s)
	}
}

func TestDiagnosisDirectUpAllProxiesDown(t *testing.T) {
	results := []probe.Result{
		mkResult("direct", config.ProbeHTTP, probe.StatusUp, 20*time.Millisecond, 0),
		mkResult("proxy-a", config.ProbeSingBox, probe.StatusDown, 0, 100),
		mkResult("proxy-b", config.ProbeSingBox, probe.StatusDown, 0, 100),
	}
	want := "hint: direct works but all proxy routes fail; proxy/VPS or control path likely issue"
	if got := diagnosis(results); got != want {
		t.Fatalf("diagnosis = %q, want %q", got, want)
	}
}

func TestDiagnosisDirectUpMixedProxyHealth(t *testing.T) {
	results := []probe.Result{
		mkResult("direct", config.ProbeHTTP, probe.StatusUp, 20*time.Millisecond, 0),
		mkResult("proxy-up", config.ProbeSingBox, probe.StatusUp, 100*time.Millisecond, 0),
		mkResult("proxy-down", config.ProbeSingBox, probe.StatusDown, 0, 100),
	}
	want := "hint: direct OK, some proxy routes failed; check those transports"
	if got := diagnosis(results); got != want {
		t.Fatalf("diagnosis = %q, want %q", got, want)
	}
}

func TestDiagnosisDirectDownAllProxiesDown(t *testing.T) {
	results := []probe.Result{
		mkResult("direct", config.ProbeHTTP, probe.StatusDown, 0, 100),
		mkResult("proxy-a", config.ProbeSingBox, probe.StatusDown, 0, 100),
	}
	want := "hint: direct baseline + all proxies down; local internet/DNS may be down"
	if got := diagnosis(results); got != want {
		t.Fatalf("diagnosis = %q, want %q", got, want)
	}
}

func TestDiagnosisMissingDirectAllProxiesDown(t *testing.T) {
	results := []probe.Result{
		mkResult("proxy-a", config.ProbeSingBox, probe.StatusDown, 0, 100),
		mkResult("proxy-b", config.ProbeSingBox, probe.StatusDown, 0, 100),
	}
	want := "hint: all routes down; check internet and proxy config"
	if got := diagnosis(results); got != want {
		t.Fatalf("diagnosis = %q, want %q", got, want)
	}
}

func TestDiagnosisDirectOnlyAndEmpty(t *testing.T) {
	for name, results := range map[string][]probe.Result{
		"direct only": {mkResult("direct", config.ProbeHTTP, probe.StatusUp, 20*time.Millisecond, 0)},
		"empty":       nil,
	} {
		t.Run(name, func(t *testing.T) {
			if got := diagnosis(results); got != "" {
				t.Fatalf("diagnosis = %q, want empty", got)
			}
		})
	}
}

func TestJSONNoSecretInError(t *testing.T) {
	secret := "fixture-secret-token"
	results := []probe.Result{
		{Endpoint: config.Endpoint{Name: "a", Type: config.ProbeSingBox, Outbound: "real"}, Status: probe.StatusDown, ErrMsg: "Get \"https://provider.example/path?token=" + secret + "\": timeout", Category: "proxy-test"},
	}
	var buf bytes.Buffer
	JSON(&buf, results)
	s := buf.String()
	if strings.Contains(s, secret) {
		t.Fatal("JSON leaked a URL query token")
	}
	if table := Table(results); strings.Contains(table, secret) {
		t.Fatal("human table leaked a URL query token")
	}
	// Ensure outbound is present but not secret
	var doc map[string]any
	json.Unmarshal(buf.Bytes(), &doc)
	resultsArr := doc["results"].([]any)
	first := resultsArr[0].(map[string]any)
	if first["outbound"] != "real" {
		t.Errorf("outbound not preserved")
	}
}
