package network

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckAllDoesNotPanic(t *testing.T) {
	checks := CheckAll()
	if len(checks) < 4 {
		t.Errorf("checks %d", len(checks))
	}
}

func TestCheckDestinations(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	checks := CheckDestinations([]string{srv.URL})
	if len(checks) != 1 {
		t.Fatal("1")
	}
	if checks[0].Status != "PASS" {
		t.Errorf("status %q", checks[0].Status)
	}
}

func TestCheckDestinationsFail(t *testing.T) {
	checks := CheckDestinations([]string{"http://127.0.0.1:1"})
	if checks[0].Status != "FAIL" {
		t.Errorf("expected fail")
	}
}
