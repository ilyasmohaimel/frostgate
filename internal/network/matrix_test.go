package network

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ilyasmohaimel/frostgate/internal/singbox"
)

// matrixFake emulates the clash-api delay endpoint per outbound, recording
// which outbound+URL pairs were probed. It never exposes a selector switch
// endpoint, proving network-check cannot mutate benchmark state.
type matrixFake struct {
	mu     sync.Mutex
	probed []string
	delays map[string]int // outbound -> ms; absent => fail (504)
}

func (f *matrixFake) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/delay"):
			parts := strings.Split(r.URL.Path, "/")
			tag := parts[2]
			urlParam := r.URL.Query().Get("url")
			f.mu.Lock()
			f.probed = append(f.probed, tag+"|"+urlParam)
			_, ok := f.delays[tag]
			f.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"delay": 42})
		case r.Method == http.MethodPut:
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func (f *matrixFake) Probed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.probed...)
}

func TestDestinationMatrix(t *testing.T) {
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	defer dest.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer bad.Close()

	fake := &matrixFake{delays: map[string]int{"reality-out": 42}}
	ctrlSrv := httptest.NewServer(fake.handler())
	defer ctrlSrv.Close()
	client := singbox.New(ctrlSrv.URL, "")

	routes := []RouteProbe{
		{Name: "vless-main", Outbound: "reality-out", Kind: "reality"},
		{Name: "hy2-alt", Outbound: "hy2-out", Kind: "hysteria2"},
	}
	rows := DestinationMatrix(context.Background(), []string{dest.URL + "/", bad.URL + "/"}, routes, client, 2*time.Second)

	if len(rows) != 2 {
		t.Fatalf("want 2 rows got %d", len(rows))
	}
	ok := rows[0]
	if ok.Direct != "PASS" || !strings.Contains(ok.DirectDetail, "204") {
		t.Fatalf("direct col: %+v", ok)
	}
	if len(ok.Routes) != 2 {
		t.Fatalf("route cols: %+v", ok.Routes)
	}
	if ok.Routes[0].Status != "PASS" || ok.Routes[0].Kind != "reality" {
		t.Fatalf("reality cell: %+v", ok.Routes[0])
	}
	if ok.Routes[1].Status != "FAIL" || ok.Routes[1].Kind != "hysteria2" {
		t.Fatalf("hysteria cell must fail when controller says so: %+v", ok.Routes[1])
	}
	fail := rows[1]
	if fail.Direct != "FAIL" {
		t.Fatalf("500 destination direct must FAIL: %+v", fail)
	}

	// Per-outbound URL-test used the actual destination URLs.
	probed := fake.Probed()
	want := map[string]bool{
		"reality-out|" + dest.URL + "/": false,
		"hy2-out|" + dest.URL + "/":     false,
		"reality-out|" + bad.URL + "/":  false,
		"hy2-out|" + bad.URL + "/":      false,
	}
	for _, p := range probed {
		if _, seen := want[p]; seen {
			want[p] = true
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("expected probe %q missing (got %v)", k, probed)
		}
	}
}

func TestMatrixNoSelectorMutation(t *testing.T) {
	// Any PUT to this fake fails 404; if network-check tried to switch a
	// selector the singbox client would error. This test proves no PUT is ever
	// issued.
	var puts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			puts++
		}
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{"delay": 5})
	}))
	defer srv.Close()
	client := singbox.New(srv.URL, "")
	DestinationMatrix(context.Background(), DefaultDestinations,
		[]RouteProbe{{Name: "x", Outbound: "x", Kind: "proxy"}}, client, time.Second)
	if puts != 0 {
		t.Fatalf("network-check must not mutate selector state, saw %d PUTs", puts)
	}
}
