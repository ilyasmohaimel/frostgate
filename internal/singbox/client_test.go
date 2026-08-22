package singbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeController simulates the sing-box clash-api surface used by Frostgate.
type fakeController struct {
	mu        sync.Mutex
	now       string
	proxies   map[string]any
	putStatus int
	honorPut  bool
}

func newFake() *fakeController {
	return &fakeController{
		now:      "A",
		honorPut: true,
		proxies: map[string]any{
			"frostgate-selector": map[string]any{"type": "selector", "now": "A", "all": []string{"A", "B"}},
			"A":                  map[string]any{"type": "vless"},
			"B":                  map[string]any{"type": "hysteria2"},
		},
		putStatus: http.StatusOK,
	}
}

func (f *fakeController) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/proxies":
			json.NewEncoder(w).Encode(map[string]any{"proxies": f.proxies})
		case r.Method == http.MethodGet:
			name := r.URL.Path[len("/proxies/"):]
			if name == "frostgate-selector" {
				json.NewEncoder(w).Encode(map[string]any{"type": "selector", "now": f.now, "all": []string{"A", "B"}})
			} else if _, ok := f.proxies[name]; ok {
				json.NewEncoder(w).Encode(map[string]any{"type": "direct"})
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		case r.Method == http.MethodPut:
			name := r.URL.Path[len("/proxies/"):]
			if name != "frostgate-selector" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if f.honorPut {
				f.now = body["name"]
			}
			w.WriteHeader(f.putStatus)
		}
	}
}

func TestGetSelectedOutboundValid(t *testing.T) {
	f := newFake()
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := New(srv.URL, "secret")
	got, err := c.GetSelectedOutbound(context.Background(), "frostgate-selector")
	if err != nil {
		t.Fatal(err)
	}
	if got != "A" {
		t.Fatalf("want A got %q", got)
	}
}

func TestGetSelectedOutboundMissingNow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"type": "selector", "all": []string{}})
	}))
	defer srv.Close()
	c := New(srv.URL, "secret")
	if _, err := c.GetSelectedOutbound(context.Background(), "frostgate-selector"); err == nil {
		t.Fatal("expected error for missing now")
	}
}

func TestGetSelectedOutboundEmptyNow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"type": "selector", "now": ""})
	}))
	defer srv.Close()
	c := New(srv.URL, "secret")
	if _, err := c.GetSelectedOutbound(context.Background(), "frostgate-selector"); err == nil {
		t.Fatal("expected error for empty now")
	}
}

func TestGetSelectedOutboundMalformed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()
	c := New(srv.URL, "secret")
	if _, err := c.GetSelectedOutbound(context.Background(), "frostgate-selector"); err == nil {
		t.Fatal("expected malformed error")
	}
}

func TestGetSelectedOutboundStatusCodes(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusNotFound, http.StatusInternalServerError} {
		code := code
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		c := New(srv.URL, "secret")
		if _, err := c.GetSelectedOutbound(context.Background(), "frostgate-selector"); err == nil {
			t.Fatalf("code %d: expected error", code)
		}
		srv.Close()
	}
}

func TestGetSelectedOutboundTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		json.NewEncoder(w).Encode(map[string]any{"proxies": map[string]any{}})
	}))
	defer srv.Close()
	c := New(srv.URL, "secret")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.GetSelectedOutbound(ctx, "frostgate-selector"); err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestSelectOutboundSuccess(t *testing.T) {
	f := newFake()
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := New(srv.URL, "secret")
	if err := c.SelectOutbound(context.Background(), "frostgate-selector", "B"); err != nil {
		t.Fatal(err)
	}
	if f.now != "B" {
		t.Fatalf("selector not switched: %q", f.now)
	}
}

func TestSelectOutboundStatusCodes(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusNotFound, http.StatusInternalServerError} {
		code := code
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		c := New(srv.URL, "secret")
		if err := c.SelectOutbound(context.Background(), "frostgate-selector", "B"); err == nil {
			t.Fatalf("code %d: expected error", code)
		}
		srv.Close()
	}
}

func TestSelectOutboundRefusesSwitch(t *testing.T) {
	f := newFake()
	// PUT returns OK but the controller refuses to actually switch.
	f.putStatus = http.StatusOK
	f.honorPut = false
	f.now = "A" // never updated
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := New(srv.URL, "secret")
	if err := c.SelectOutbound(context.Background(), "frostgate-selector", "B"); err == nil {
		t.Fatal("expected refusal error")
	}
}

func TestSelectOutboundTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := New(srv.URL, "secret")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := c.SelectOutbound(ctx, "frostgate-selector", "B"); err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestSelectOutboundVerifyMalformed(t *testing.T) {
	f := newFake()
	f.putStatus = http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Write([]byte("not json"))
	}))
	defer srv.Close()
	c := New(srv.URL, "secret")
	// After PUT, the GET verify returns malformed JSON -> error.
	_ = f
	if err := c.SelectOutbound(context.Background(), "frostgate-selector", "B"); err == nil {
		t.Fatal("expected verify malformed error")
	}
}
