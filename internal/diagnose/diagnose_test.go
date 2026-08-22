package diagnose

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ilyasmohaimel/frostgate/internal/config"
	"github.com/ilyasmohaimel/frostgate/internal/managedconfig"
	"github.com/ilyasmohaimel/frostgate/internal/setup"
)

// fakeController simulates the clash-api surface used by the diagnosis engine.
type fakeController struct {
	mu     sync.Mutex
	now    string
	delays map[string]int // outbound tag -> delay ms; missing = 504
}

func newFakeCtrl(delays map[string]int) *fakeController {
	return &fakeController{now: "vless-a", delays: delays}
}

func (f *fakeController) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/proxies":
			proxies := map[string]any{"direct": map[string]any{"type": "direct"}}
			f.mu.Lock()
			for tag := range f.delays {
				proxies[tag] = map[string]any{"type": "vless"}
			}
			f.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"proxies": proxies})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/delay"):
			parts := strings.Split(r.URL.Path, "/")
			tag := parts[2]
			f.mu.Lock()
			d, ok := f.delays[tag]
			f.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusGatewayTimeout)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"delay": d})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func testCfg(t *testing.T, ctrlURL string, eps ...config.Endpoint) *config.Config {
	t.Helper()
	return &config.Config{
		Defaults: config.Defaults{Timeout: config.Duration{Duration: 5 * time.Second}},
		SingBox:  config.SingBoxConfig{Controller: ctrlURL, TestURL: "https://www.gstatic.com/generate_204"},
		Endpoints: append([]config.Endpoint{
			{Name: "direct", Type: config.ProbeHTTP, URL: "https://www.gstatic.com/generate_204", Priority: 100},
		}, eps...),
	}
}

func managedMeta() *managedconfig.Managed {
	return &managedconfig.Managed{
		VLESS: []*setup.VLESSOutbound{{
			Tag: "vless-a", Server: "127.0.0.1", ServerPort: 443,
			UUID: "11111111-2222-3333-4444-555555555555", SNI: "example.com",
			PBK: "SECRET-PBK", SID: "abcd", Flow: "xtls-rprx-vision",
		}},
		Hysteria: []*setup.Hysteria2Outbound{{
			Tag: "hy2-b", Server: "127.0.0.1", ServerPort: 8443,
			Password: "hush-hush", SNI: "hy.example.com",
		}},
	}
}

func findLayer(layers []Layer, name string) *Layer {
	for i := range layers {
		if layers[i].Name == name {
			return &layers[i]
		}
	}
	return nil
}

func TestVLESSMeasuredDNSTCPAndEndToEnd(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctrl := httptest.NewServer(newFakeCtrl(map[string]int{"vless-a": 42}).handler())
	defer ctrl.Close()

	cfg := testCfg(t, ctrl.URL,
		config.Endpoint{Name: "vless-a", Type: config.ProbeSingBox, Outbound: "vless-a"})
	meta := managedMeta()
	meta.VLESS[0].ServerPort = ln.Addr().(*net.TCPAddr).Port
	layers := DiagnoseRoute(cfg, "vless-a", Options{Managed: meta})

	if l := findLayer(layers, "DNS vless-a"); l == nil || l.Status != "PASS" {
		t.Fatalf("DNS layer: %+v", l)
	}
	tcp := findLayer(layers, "Server TCP vless-a")
	if tcp == nil || tcp.Status != "PASS" {
		t.Fatalf("TCP layer must be measured PASS: %+v", tcp)
	}
	if !strings.Contains(tcp.Detail, fmt.Sprintf("tcp/%d reachable", meta.VLESS[0].ServerPort)) {
		t.Fatalf("TCP detail must claim measured reachability: %q", tcp.Detail)
	}
	e2e := findLayer(layers, "End-to-end vless-a")
	if e2e == nil || e2e.Status != "PASS" || !strings.Contains(e2e.Detail, "42ms") {
		t.Fatalf("end-to-end: %+v", e2e)
	}
}

func TestVLESSDeadServerFailsTCP(t *testing.T) {
	// Find a definitely-closed port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	ctrl := httptest.NewServer(newFakeCtrl(map[string]int{}).handler())
	defer ctrl.Close()

	cfg := testCfg(t, ctrl.URL,
		config.Endpoint{Name: "vless-a", Type: config.ProbeSingBox, Outbound: "vless-a"})
	meta := managedMeta()
	meta.VLESS[0].ServerPort = closedPort
	layers := DiagnoseRoute(cfg, "vless-a", Options{Managed: meta})

	tcp := findLayer(layers, "Server TCP vless-a")
	if tcp == nil || tcp.Status != "FAIL" {
		t.Fatalf("closed port must FAIL measured TCP: %+v", tcp)
	}
	e2e := findLayer(layers, "End-to-end vless-a")
	wantHint := "server may be reachable, but REALITY/auth failed; check PBK, SID, or filtering"
	if e2e == nil || e2e.Hint != wantHint {
		t.Fatalf("end-to-end failure hint: %+v", e2e)
	}
}

func TestHysteriaHonestWording(t *testing.T) {
	ctrl := httptest.NewServer(newFakeCtrl(map[string]int{"hy2-b": 77}).handler())
	defer ctrl.Close()

	cfg := testCfg(t, ctrl.URL,
		config.Endpoint{Name: "hy2-b", Type: config.ProbeSingBox, Outbound: "hy2-b"})
	layers := DiagnoseRoute(cfg, "hy2-b", Options{Managed: managedMeta()})

	srv := findLayer(layers, "Server hy2-b")
	if srv == nil || srv.Status != "SKIP" || !strings.Contains(srv.Detail, "not independently verified") {
		t.Fatalf("hysteria reachability wording: %+v", srv)
	}
	e2e := findLayer(layers, "End-to-end Hysteria2 hy2-b")
	if e2e == nil || e2e.Status != "PASS" || !strings.Contains(e2e.Detail, "End-to-end Hysteria2: PASS") {
		t.Fatalf("hysteria end-to-end: %+v", e2e)
	}
}

func TestBackupRoutesActuallyProbed(t *testing.T) {
	fc := newFakeCtrl(map[string]int{"vless-a": 171})
	ctrlSrv := httptest.NewServer(fc.handler())
	defer ctrlSrv.Close()

	cfg := testCfg(t, ctrlSrv.URL,
		config.Endpoint{Name: "primary", Type: config.ProbeSingBox, Outbound: "primary-x"},
		config.Endpoint{Name: "backup-good", Type: config.ProbeSingBox, Outbound: "vless-a"},
	)
	// primary-x not in delay map -> its own e2e fails; backups are what we assert.
	layers := DiagnoseRoute(cfg, "primary", Options{})
	bk := findLayer(layers, "Backup backup-good")
	if bk == nil || bk.Status != "PASS" || !strings.Contains(bk.Detail, "171ms") {
		t.Fatalf("healthy backup must show measured latency: %+v", bk)
	}

	// Broken backup shows FAIL with reason.
	fc2 := newFakeCtrl(map[string]int{})
	ctrl2 := httptest.NewServer(fc2.handler())
	defer ctrl2.Close()
	cfg2 := testCfg(t, ctrl2.URL,
		config.Endpoint{Name: "primary", Type: config.ProbeSingBox, Outbound: "primary-x"},
		config.Endpoint{Name: "backup-dead", Type: config.ProbeSingBox, Outbound: "vless-dead"},
	)
	layers2 := DiagnoseRoute(cfg2, "primary", Options{})
	bd := findLayer(layers2, "Backup backup-dead")
	if bd == nil || bd.Status != "FAIL" {
		t.Fatalf("dead backup must FAIL: %+v", bd)
	}
}

func TestNoComparisonStubLayer(t *testing.T) {
	cfg := testCfg(t, "http://127.0.0.1:1")
	layers := DiagnoseAll(cfg, Options{})
	for _, l := range layers {
		if l.Name == "Comparison" {
			t.Fatal("decorative Comparison layer must not exist")
		}
	}
}

func TestMetadataSecretsNeverLeak(t *testing.T) {
	ctrl := httptest.NewServer(newFakeCtrl(map[string]int{"vless-a": 10, "hy2-b": 20}).handler())
	defer ctrl.Close()
	cfg := testCfg(t, ctrl.URL,
		config.Endpoint{Name: "vless-a", Type: config.ProbeSingBox, Outbound: "vless-a"},
		config.Endpoint{Name: "hy2-b", Type: config.ProbeSingBox, Outbound: "hy2-b"},
	)
	meta := managedMeta()
	secrets := []string{
		meta.VLESS[0].UUID, meta.VLESS[0].PBK, meta.VLESS[0].SID,
		meta.Hysteria[0].Password,
	}
	var out []Layer
	out = append(out, DiagnoseRoute(cfg, "vless-a", Options{Managed: meta})...)
	out = append(out, DiagnoseRoute(cfg, "hy2-b", Options{Managed: meta})...)
	for _, l := range out {
		for _, s := range secrets {
			if s != "" && strings.Contains(l.Name+l.Detail+l.Hint, s) {
				t.Errorf("secret leaked in layer %q", l.Name)
			}
		}
	}
}

func TestDiagnoseRouteNotFound(t *testing.T) {
	cfg := testCfg(t, "http://127.0.0.1:1",
		config.Endpoint{Name: "a", Type: config.ProbeHTTP, URL: "https://example.com"})
	layers := DiagnoseRoute(cfg, "nonexistent", Options{})
	if len(layers) != 1 || layers[0].Status != "FAIL" {
		t.Fatal("want single FAIL layer for unknown route")
	}
}

func TestErrStringRedactsAndTruncates(t *testing.T) {
	long := strings.Repeat("x", 200)
	got := errString(fmt.Errorf("vless://%s@host", long))
	if strings.Contains(got, long) || !strings.Contains(got, "REDACTED") {
		t.Fatalf("share-link error must redact, got %q", got)
	}
	if got := errString(fmt.Errorf("%s", long)); len(got) > 80 {
		t.Fatalf("must truncate to 80, got %d", len(got))
	}
	if errString(nil) != "" {
		t.Fatal("nil error must be empty")
	}
	_ = context.Background()
	_ = time.Second
}
