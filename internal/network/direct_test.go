package network

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestDestinationCheckBypassesProxyEnvironment(t *testing.T) {
	var proxyHits, targetHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyHits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer proxy.Close()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
		t.Setenv(name, proxy.URL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	checks := CheckDestinations([]string{target.URL})
	if len(checks) != 1 || checks[0].Status != "PASS" {
		t.Fatalf("direct destination check = %+v", checks)
	}
	if proxyHits.Load() != 0 || targetHits.Load() != 1 {
		t.Fatalf("proxy hits=%d target hits=%d", proxyHits.Load(), targetHits.Load())
	}
}
