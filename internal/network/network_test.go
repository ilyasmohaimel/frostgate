package network

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNormalizeAddrs(t *testing.T) {
	got := NormalizeAddrs([]string{"2001:0DB8::1", "203.0.113.2", "203.0.113.2", " 203.0.113.1 ", "bad"})
	want := []string{"2001:db8::1", "203.0.113.1", "203.0.113.2"}
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
		rr := 1
		if qtype == "AAAA" {
			rr = 28
		}
		if data == "" {
			w.Write([]byte(`{"Status":0,"Answer":[]}`))
			return
		}
		w.Write([]byte(fmt.Sprintf(`{"Status":0,"Answer":[{"name":"x","type":%d,"data":"%s"}]}`, rr, data)))
	}))
}

func TestCheckDNSVsDoHMatch(t *testing.T) {
	srv := dohStub(map[string]string{"A": "203.0.113.50", "AAAA": "2001:db8::1"}, 200)
	defer srv.Close()
	old := systemLookup
	systemLookup = func(_ context.Context, host string) ([]string, error) {
		return []string{"203.0.113.50", "2001:db8::1"}, nil
	}
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
	systemLookup = func(_ context.Context, host string) ([]string, error) { return []string{"203.0.113.52"}, nil }
	defer func() { systemLookup = old }()
	c := CheckDNSVsDoH("example.com", srv.URL)
	want := "DIFFER for example.com (signal only - CDNs can return different addresses)"
	if c.Status != "WARN" || c.Detail != want {
		t.Errorf("want WARN %q got %+v", want, c)
	}
}

func TestCheckDNSVsDoHUnavailable(t *testing.T) {
	old := systemLookup
	systemLookup = func(_ context.Context, host string) ([]string, error) { return nil, context.DeadlineExceeded }
	defer func() { systemLookup = old }()
	c := CheckDNSVsDoH("example.com", "http://127.0.0.1:1/dns-query")
	if c.Status != "UNAVAILABLE" {
		t.Errorf("want UNAVAILABLE got %+v", c)
	}
}

func TestSystemDNSLookupUsesBoundedContext(t *testing.T) {
	oldLookup, oldTimeout := systemLookup, systemLookupTimeout
	systemLookupTimeout = 20 * time.Millisecond
	systemLookup = func(ctx context.Context, _ string) ([]string, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	t.Cleanup(func() {
		systemLookup = oldLookup
		systemLookupTimeout = oldTimeout
	})
	start := time.Now()
	check := checkDNS()
	if check.Status != "FAIL" || time.Since(start) > time.Second {
		t.Fatalf("bounded DNS check = %+v after %s", check, time.Since(start))
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

func TestFetchDoHFiltersOnlyAddressRecords(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("type") == "A" {
			_, _ = w.Write([]byte(`{"Status":0,"Answer":[{"type":5,"data":"alias.example"},{"type":1,"data":"203.0.113.9"},{"type":1,"data":"203.0.113.9"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"Status":0,"Answer":[{"type":28,"data":"2001:0db8::9"}]}`))
	}))
	defer srv.Close()
	got, err := FetchDoHAnswers(context.Background(), srv.Client(), srv.URL, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2001:db8::9", "203.0.113.9"}
	got = NormalizeAddrs(got)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("answers %v want %v", got, want)
	}
}

func TestFetchDoHRejectsDNSFailureAndMalformedAddress(t *testing.T) {
	for _, body := range []string{
		`{"Status":3,"Answer":[]}`,
		`{"Status":2,"Answer":[]}`,
		`{"Status":0,"Answer":[{"type":1,"data":"not-an-ip"}]}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
		_, err := FetchDoHAnswers(context.Background(), srv.Client(), srv.URL, "example.com")
		srv.Close()
		if err == nil {
			t.Fatalf("body %s was accepted", body)
		}
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
