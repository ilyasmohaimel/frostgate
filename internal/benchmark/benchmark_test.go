package benchmark

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"math"
	"net"
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
		w.Write(make([]byte, 123))
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

func TestBoundedReadLimitDoesNotWrap(t *testing.T) {
	if got := boundedReadLimit(math.MaxInt64, 1); got != math.MaxInt64 {
		t.Fatalf("overflowing read limit wrapped: %d", got)
	}
}

func TestMeasureDirectDisablesAutomaticCompression(t *testing.T) {
	payload := make([]byte, 64*1024)
	for i := range payload {
		payload[i] = 'a'
	}
	var advertised string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		advertised = r.Header.Get("Accept-Encoding")
		if advertised == "gzip" {
			w.Header().Set("Content-Encoding", "gzip")
			zw := gzip.NewWriter(w)
			_, _ = zw.Write(payload)
			_ = zw.Close()
			return
		}
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, _, err := MeasureDirect(ctx, srv.URL, int64(len(payload))); err != nil {
		t.Fatal(err)
	}
	if advertised != "" {
		t.Fatalf("benchmark advertised automatic compression: %q", advertised)
	}
}

func TestMeasureDirectStopsAtByteCapWithoutWaitingForEOF(t *testing.T) {
	release := make(chan struct{})
	done := make(chan struct{})
	released := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		w.Header().Set("Content-Length", "1048576")
		_, _ = w.Write(make([]byte, 4096))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-release
	}))
	defer func() {
		if !released {
			close(release)
		}
		srv.Close()
	}()

	start := time.Now()
	_, _, err := MeasureDirect(context.Background(), srv.URL, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("measurement waited for EOF after byte cap: %s", elapsed)
	}
	close(release)
	released = true
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("test server handler did not finish after release")
	}
}

func TestMeasureViaSOCKSRespectsCancellationDuringHandshake(t *testing.T) {
	for _, phase := range []string{"greeting", "connect-reply", "http"} {
		t.Run(phase, func(t *testing.T) {
			addr, closeServer := stalledSOCKSServer(t, phase)
			defer closeServer()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			start := time.Now()
			_, _, err := MeasureViaSOCKS(ctx, addr, "http://example.test/", 1024)
			if err == nil {
				t.Fatal("expected cancellation error")
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("SOCKS %s cancellation took %s", phase, elapsed)
			}
		})
	}
}

func stalledSOCKSServer(t *testing.T, phase string) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if phase == "greeting" {
			io.Copy(io.Discard, conn)
			return
		}
		head := make([]byte, 2)
		if _, err := io.ReadFull(conn, head); err != nil {
			return
		}
		if _, err := io.ReadFull(conn, make([]byte, int(head[1]))); err != nil {
			return
		}
		if _, err := conn.Write([]byte{5, 0}); err != nil {
			return
		}
		if phase == "connect-reply" {
			io.Copy(io.Discard, conn)
			return
		}
		request := make([]byte, 4)
		if _, err := io.ReadFull(conn, request); err != nil {
			return
		}
		switch request[3] {
		case 1:
			_, _ = io.ReadFull(conn, make([]byte, 6))
		case 3:
			length := make([]byte, 1)
			if _, err := io.ReadFull(conn, length); err != nil {
				return
			}
			_, _ = io.ReadFull(conn, make([]byte, int(length[0])+2))
		case 4:
			_, _ = io.ReadFull(conn, make([]byte, 18))
		}
		_, _ = conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
		io.Copy(io.Discard, conn)
	}()
	return ln.Addr().String(), func() {
		_ = ln.Close()
		<-done
	}
}
