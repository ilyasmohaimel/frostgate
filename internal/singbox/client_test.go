package singbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDelaySuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/proxies/test-out/delay" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if r.URL.Query().Get("url") == "" {
			t.Error("missing url param")
		}
		if r.URL.Query().Get("timeout") == "" {
			t.Error("missing timeout param")
		}
		json.NewEncoder(w).Encode(map[string]any{"delay": 123})
	}))
	defer srv.Close()
	c := New(srv.URL, "")
	c.HTTP = srv.Client()
	d, err := c.Delay(context.Background(), "test-out", "https://example.com", 2*time.Second)
	if err != nil {
		t.Fatalf("Delay: %v", err)
	}
	if d != 123*time.Millisecond {
		t.Errorf("delay=%v want 123ms", d)
	}
}

func TestDelayUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := New(srv.URL, "bad")
	c.HTTP = srv.Client()
	_, err := c.Delay(context.Background(), "x", "https://example.com", time.Second)
	if err == nil || !contains(err.Error(), "unauthorized") {
		t.Fatalf("expected unauthorized, got %v", err)
	}
}

func TestDelayNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"message": "not found"})
	}))
	defer srv.Close()
	c := New(srv.URL, "")
	c.HTTP = srv.Client()
	_, err := c.Delay(context.Background(), "missing", "https://example.com", time.Second)
	if err == nil || !contains(err.Error(), "outbound-not-found") {
		t.Fatalf("expected outbound-not-found, got %v", err)
	}
}

func TestDelayTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGatewayTimeout)
	}))
	defer srv.Close()
	c := New(srv.URL, "")
	c.HTTP = srv.Client()
	_, err := c.Delay(context.Background(), "slow", "https://example.com", time.Second)
	if err == nil || !contains(err.Error(), "timeout") {
		t.Fatalf("expected timeout, got %v", err)
	}
}

func TestDelayServiceUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := New(srv.URL, "")
	c.HTTP = srv.Client()
	_, err := c.Delay(context.Background(), "broken", "https://example.com", time.Second)
	if err == nil || !contains(err.Error(), "proxy-test") {
		t.Fatalf("expected proxy-test, got %v", err)
	}
}

func TestDelayMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()
	c := New(srv.URL, "")
	c.HTTP = srv.Client()
	_, err := c.Delay(context.Background(), "x", "https://example.com", time.Second)
	if err == nil || !contains(err.Error(), "malformed") {
		t.Fatalf("expected malformed, got %v", err)
	}
}

func TestDelayControllerUnreachable(t *testing.T) {
	c := New("http://127.0.0.1:1", "")
	// use short timeout client
	c.HTTP = &http.Client{Timeout: 200 * time.Millisecond}
	_, err := c.Delay(context.Background(), "x", "https://example.com", 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected error for unreachable controller")
	}
}

func TestDelayContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		json.NewEncoder(w).Encode(map[string]int{"delay": 10})
	}))
	defer srv.Close()
	c := New(srv.URL, "")
	c.HTTP = srv.Client()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Delay(ctx, "x", "https://example.com", 5*time.Second)
	if err == nil {
		t.Fatal("expected cancellation error")
	}
}

func TestDelayAuthHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(map[string]int{"delay": 10})
	}))
	defer srv.Close()
	c := New(srv.URL, "s3cr3t")
	c.HTTP = srv.Client()
	_, err := c.Delay(context.Background(), "x", "https://example.com", time.Second)
	if err != nil {
		t.Fatalf("Delay: %v", err)
	}
	if gotAuth != "Bearer s3cr3t" {
		t.Errorf("auth header = %q want Bearer s3cr3t", gotAuth)
	}
}

func TestGetProxiesSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"proxies": map[string]any{"out1": map[string]string{"type": "vless"}, "out2": map[string]string{"type": "hysteria2"}}})
	}))
	defer srv.Close()
	c := New(srv.URL, "")
	c.HTTP = srv.Client()
	proxies, err := c.GetProxies(context.Background())
	if err != nil {
		t.Fatalf("GetProxies: %v", err)
	}
	if len(proxies) != 2 {
		t.Errorf("proxies count=%d want 2", len(proxies))
	}
}

func TestGetProxiesMalformed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{bad"))
	}))
	defer srv.Close()
	c := New(srv.URL, "")
	c.HTTP = srv.Client()
	_, err := c.GetProxies(context.Background())
	if err == nil {
		t.Fatal("expected malformed error")
	}
}

func TestCheckOutboundFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"proxies": map[string]any{"my-out": map[string]string{}}})
	}))
	defer srv.Close()
	c := New(srv.URL, "")
	c.HTTP = srv.Client()
	if err := c.CheckOutbound(context.Background(), "my-out"); err != nil {
		t.Fatalf("CheckOutbound: %v", err)
	}
}

func TestCheckOutboundMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"proxies": map[string]any{"other": map[string]string{}}})
	}))
	defer srv.Close()
	c := New(srv.URL, "")
	c.HTTP = srv.Client()
	if err := c.CheckOutbound(context.Background(), "missing"); err == nil {
		t.Fatal("expected missing error")
	}
}

func TestDelayServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := New(srv.URL, "")
	c.HTTP = srv.Client()
	_, err := c.Delay(context.Background(), "x", "https://example.com", time.Second)
	if err == nil || !contains(err.Error(), "500") {
		t.Fatalf("expected 500, got %v", err)
	}
}
