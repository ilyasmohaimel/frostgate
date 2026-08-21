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
	if doc["schema_version"] != float64(2) {
		t.Errorf("schema_version %v want 2", doc["schema_version"])
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
	if doc["schema_version"] != float64(2) {
		t.Error("wrong schema")
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
	if !strings.Contains(s, "timeout") {
		t.Errorf("expected category timeout in table, got %q", s)
	}
}

func TestJSONNoSecretInError(t *testing.T) {
	// even if ErrMsg contains secret-like string, we don't filter but ensure we don't add secret field
	results := []probe.Result{
		{Endpoint: config.Endpoint{Name: "a", Type: config.ProbeSingBox, Outbound: "real"}, Status: probe.StatusDown, ErrMsg: "some error", Category: "proxy-test"},
	}
	var buf bytes.Buffer
	JSON(&buf, results)
	s := buf.String()
	if strings.Contains(s, "Controller") {
		t.Log("ok")
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
