package benchmark

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMeasureDirect(t *testing.T) {
	// Serve 1 MiB
	data := make([]byte, 1024*1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(data)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	mbps, dur, err := MeasureDirect(ctx, srv.URL, int64(len(data)))
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if mbps <= 0 {
		t.Errorf("mbps %f", mbps)
	}
	if dur == 0 {
		t.Error("dur 0")
	}
}

func TestMeasureDirectTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.Write([]byte("hi"))
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, _, err := MeasureDirect(ctx, srv.URL, 1024)
	if err == nil {
		t.Error("expected timeout")
	}
}

func TestHasBytesParam(t *testing.T) {
	if !HasBytesParam("https://example.com/__down?bytes=8000000") {
		t.Error("should have")
	}
	if HasBytesParam("https://example.com/down") {
		t.Error("should not")
	}
}

func TestMeasureViaSOCKSFail(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, _, err := MeasureViaSOCKS(ctx, "127.0.0.1:1", "https://example.com/__down?bytes=1000", 1000)
	if err == nil {
		t.Error("expected fail for bad socks")
	}
}

func TestMeasureDirectWithBytesParam(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("bytes") == "" {
			t.Error("bytes param missing")
		}
		w.Write([]byte("x"))
	}))
	defer srv.Close()
	ctx := context.Background()
	_, _, err := MeasureDirect(ctx, srv.URL+"?bytes=123", 123)
	if err != nil {
		t.Fatalf("with bytes: %v", err)
	}
}

func TestMeasureDirectZeroElapsed(t *testing.T) {
	// Just ensuring not panic
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello")
	}))
	defer srv.Close()
	ctx := context.Background()
	mbps, _, _ := MeasureDirect(ctx, srv.URL, 5)
	if mbps < 0 {
		t.Error("mbps negative")
	}
}
