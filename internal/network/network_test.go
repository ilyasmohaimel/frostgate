package network

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNormalizeAddrs(t *testing.T) {
	got := NormalizeAddrs([]string{"B", "a", "a", " c ", ""})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestCompareSets(t *testing.T) {
	if CompareSets([]string{"1.1.1.1"}, []string{"1.1.1.1"}) != "MATCH" {
		t.Error("equal sets should MATCH")
	}
	if CompareSets([]string{"1.1.1.1"}, []string{"2.2.2.2"}) != "DIFFER" {
		t.Error("different sets should DIFFER")
	}
	if CompareSets([]string{"1.1.1.1"}, nil) != "UNAVAILABLE" {
		t.Error("empty side should be UNAVAILABLE")
	}
}

func dohStub(answers map[string]string, status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		qtype := r.URL.Query().Get("type")
		w.Header().Set("Content-Type", "application/dns-json")
		w.WriteHeader(status)
		data := answers[qtype]
		if data == "" {
			w.Write([]byte(`{"Status":0,"Answer":[]}`))
			return
		}
		w.Write([]byte(`{"Status":0,"Answer":[{"name":"x","type":1,"data":"` + data + `"}]}`))
	}))
}

func TestCheckDNSVsDoHMatch(t *testing.T) {
	srv := dohStub(map[string]string{"A": "203.0.113.50", "AAAA": "2001:db8::1"}, 200)
	defer srv.Close()
	old := systemLookup
	systemLookup = func(host string) ([]string, error) { return []string{"203.0.113.50", "2001:db8::1"}, nil }
	defer func() { systemLookup = old }()
	c := CheckDNSVsDoH("example.com", srv.URL)
	if c.Status != "PASS" || c.Detail[:5] != "MATCH" {
		t.Errorf("want PASS MATCH got %+v", c)
	}
}

func TestCheckDNSVsDoHDiffer(t *testing.T) {
	srv := dohStub(map[string]string{"A": "203.0.113.51"}, 200)
	defer srv.Close()
	old := systemLookup
	systemLookup = func(host string) ([]string, error) { return []string{"203.0.113.52"}, nil }
	defer func() { systemLookup = old }()
	c := CheckDNSVsDoH("example.com", srv.URL)
	if c.Status != "WARN" || c.Detail[:6] != "DIFFER" {
		t.Errorf("want WARN DIFFER got %+v", c)
	}
}

func TestCheckDNSVsDoHUnavailable(t *testing.T) {
	old := systemLookup
	systemLookup = func(host string) ([]string, error) { return nil, context.DeadlineExceeded }
	defer func() { systemLookup = old }()
	c := CheckDNSVsDoH("example.com", "http://127.0.0.1:1/dns-query")
	if c.Status != "UNAVAILABLE" {
		t.Errorf("want UNAVAILABLE got %+v", c)
	}
}

func TestFetchDoHAnswersMalformed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()
	if _, err := FetchDoHAnswers(context.Background(), srv.Client(), srv.URL, "x"); err == nil {
		t.Fatal("expected malformed error")
	}
}

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
