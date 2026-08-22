package benchmark

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ilyasmohaimel/frostgate/internal/singbox"
)

// attrController is a hermetic clash-api double that records every selector
// switch so tests can prove which route was active at each moment.
type attrController struct {
	mu      sync.Mutex
	now     string
	history []string
}

func (c *attrController) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/proxies":
			json.NewEncoder(w).Encode(map[string]any{
				"proxies": map[string]any{
					"frostgate-selector": map[string]any{"type": "selector", "now": c.now},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/proxies/frostgate-selector":
			json.NewEncoder(w).Encode(map[string]any{"type": "selector", "now": c.now})
		case r.Method == http.MethodPut && r.URL.Path == "/proxies/frostgate-selector":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			c.now = body["name"]
			c.history = append(c.history, c.now)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func (c *attrController) Now() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *attrController) Switches() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.history...)
}

// socksBackend is a minimal loopback SOCKS5 server whose HTTP responses are
// tagged by whichever route the controller currently has selected. Route A
// serves the full payload; brokenRoute refuses every connection, proving that
// throughput measured while B is selected cannot silently inherit A's data.
type socksBackend struct {
	mu          sync.Mutex
	ln          net.Listener
	ctrl        *attrController
	goodRoute   string
	brokenRoute string
	servedBy    map[string]int
	refusals    int
	payload     []byte
}

func newSocksBackend(ctrl *attrController, good, broken string, payloadLen int) *socksBackend {
	b := &socksBackend{
		ctrl:        ctrl,
		goodRoute:   good,
		brokenRoute: broken,
		servedBy:    map[string]int{},
		payload:     make([]byte, payloadLen),
	}
	for i := range b.payload {
		b.payload[i] = byte('x')
	}
	return b
}

func (b *socksBackend) Start(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b.ln = ln
	go b.acceptLoop()
	return ln.Addr().String()
}

func (b *socksBackend) acceptLoop() {
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			return
		}
		go b.handle(conn)
	}
}

func (b *socksBackend) handle(conn net.Conn) {
	defer conn.Close()
	buf := make([]byte, 2)
	if _, err := io.ReadFull(conn, buf); err != nil { // VER NMETHODS
		return
	}
	nmethods := int(buf[1])
	methods := make([]byte, nmethods)
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil { // no-auth
		return
	}
	head := make([]byte, 4) // VER CMD RSV ATYP
	if _, err := io.ReadFull(conn, head); err != nil {
		return
	}
	// consume target address + port
	switch head[3] {
	case 0x01:
		io.ReadFull(conn, make([]byte, 6))
	case 0x04:
		io.ReadFull(conn, make([]byte, 18))
	case 0x03:
		l := make([]byte, 1)
		io.ReadFull(conn, l)
		io.ReadFull(conn, make([]byte, int(l[0])+2))
	}
	// Success reply; tunnel established from the client's perspective.
	conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	route := b.ctrl.Now()
	b.mu.Lock()
	if route == b.brokenRoute {
		b.refusals++
		b.mu.Unlock()
		return // drop the tunnel: this route cannot carry traffic
	}
	b.servedBy[route]++
	b.mu.Unlock()
	drainHTTPRequest(conn)
	body := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", len(b.payload))
	conn.Write([]byte(body))
	conn.Write(b.payload)
}

// drainHTTPRequest reads until end of headers so closing the connection never
// sends RST with unread data in flight (which would truncate the payload).
func drainHTTPRequest(conn net.Conn) {
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4096)
	var total int
	for total < 64*1024 {
		n, err := conn.Read(buf)
		total += n
		if err != nil || bytes.Contains(buf[:n], []byte("\r\n\r\n")) {
			break
		}
	}
	conn.SetReadDeadline(time.Time{})
}

func (b *socksBackend) ServedBy(route string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.servedBy[route]
}

func (b *socksBackend) Refusals() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.refusals
}

func (b *socksBackend) Close() { b.ln.Close() }

// slowSocksBackend delays the body so tests can cancel mid-measurement.
type slowSocksBackend struct {
	*socksBackend
	delay time.Duration
}

func (b *slowSocksBackend) handle(conn net.Conn) {
	defer conn.Close()
	buf := make([]byte, 2)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return
	}
	io.ReadFull(conn, make([]byte, int(buf[1])))
	conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	head := make([]byte, 4)
	io.ReadFull(conn, head)
	switch head[3] {
	case 0x01:
		io.ReadFull(conn, make([]byte, 6))
	case 0x04:
		io.ReadFull(conn, make([]byte, 18))
	case 0x03:
		l := make([]byte, 1)
		io.ReadFull(conn, l)
		io.ReadFull(conn, make([]byte, int(l[0])+2))
	}
	route := b.ctrl.Now()
	b.mu.Lock()
	if route == b.brokenRoute {
		b.refusals++
		b.mu.Unlock()
		return
	}
	b.servedBy[route]++
	b.mu.Unlock()
	drainHTTPRequest(conn)
	conn.Write([]byte(fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", len(b.payload))))
	time.Sleep(b.delay)
	conn.Write(b.payload)
}

const attrPayloadBytes = 512 * 1024

func attrSetup(t *testing.T) (*singbox.Client, *attrController, *socksBackend, string) {
	t.Helper()
	ctrl := &attrController{now: "A"}
	srv := httptest.NewServer(ctrl.handler())
	t.Cleanup(srv.Close)
	be := newSocksBackend(ctrl, "A", "B", attrPayloadBytes)
	addr := be.Start(t)
	t.Cleanup(be.Close)
	return singbox.New(srv.URL, ""), ctrl, be, addr
}

// TestAttributionAB proves the v0.4.0 wrong-route bug stays fixed: measuring
// A actually uses A, switching to B measures B (which is broken here), and
// the selector is restored to the original afterwards.
func TestAttributionAB(t *testing.T) {
	client, _, backend, socksAddr := attrSetup(t)

	res := BenchRoutes(context.Background(), client, "frostgate-selector", socksAddr,
		"http://speed.test/down", attrPayloadBytes, []string{"A", "B"}, MeasureSOCKSMbps)

	if len(res) != 2 {
		t.Fatalf("want 2 results got %+v", res)
	}
	if res[0].Route != "A" || res[0].NAReason != "" || res[0].DownloadMbps <= 0 {
		t.Fatalf("A must measure successfully: %+v", res[0])
	}
	if res[1].Route != "B" || res[1].DownloadMbps != 0 || res[1].NAReason != "throughput measurement failed" {
		t.Fatalf("B must fail without inheriting A throughput: %+v", res[1])
	}
	if backend.ServedBy("A") != 1 {
		t.Fatalf("backend should have served exactly one payload for A, got %d", backend.ServedBy("A"))
	}
	if backend.ServedBy("B") != 0 {
		t.Fatal("B must never receive payload")
	}
	if backend.Refusals() < 1 {
		t.Fatal("broken route refusals not observed")
	}
}

func TestAttributionRestoreAfterSuccessAndError(t *testing.T) {
	client, ctrl, _, socksAddr := attrSetup(t)

	res := BenchRoutes(context.Background(), client, "frostgate-selector", socksAddr,
		"http://speed.test/down", attrPayloadBytes, []string{"A"}, MeasureSOCKSMbps)
	if len(res) != 1 || res[0].DownloadMbps <= 0 {
		t.Fatalf("success case must pass: %+v", res)
	}
	if now := ctrl.Now(); now != "A" {
		t.Fatalf("selector must restore after success, now=%q", now)
	}

	// Original selection is X; benchmarking a broken route must leave X in place.
	ctrl.mu.Lock()
	ctrl.now = "X"
	ctrl.history = nil
	ctrl.mu.Unlock()
	res = BenchRoutes(context.Background(), client, "frostgate-selector", socksAddr,
		"http://speed.test/down", attrPayloadBytes, []string{"B"}, MeasureSOCKSMbps)
	if len(res) != 1 || res[0].NAReason == "" {
		t.Fatalf("error case must report N/A reason: %+v", res)
	}
	if now := ctrl.Now(); now != "X" {
		t.Fatalf("selector must restore after throughput error, now=%q", now)
	}
	switches := ctrl.Switches()
	if len(switches) < 2 || switches[len(switches)-1] != "X" {
		t.Fatalf("switch history must end restoring X: %v", switches)
	}
}

func TestAttributionRestoreAfterCancel(t *testing.T) {
	ctrl := &attrController{now: "ORIG"}
	srv := httptest.NewServer(ctrl.handler())
	defer srv.Close()
	be := &slowSocksBackend{socksBackend: newSocksBackend(ctrl, "SLOW", "NONE", attrPayloadBytes), delay: 10 * time.Second}
	addr := be.Start(t)
	defer be.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan []RouteThroughput, 1)
	go func() {
		done <- BenchRoutes(ctx, singbox.New(srv.URL, ""), "frostgate-selector", addr,
			"http://speed.test/down", attrPayloadBytes, []string{"SLOW"}, MeasureSOCKSMbps)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("BenchRoutes did not return promptly after cancellation")
	}
	if now := ctrl.Now(); now != "ORIG" {
		t.Fatalf("selector must restore after cancellation, now=%q", now)
	}
	switches := ctrl.Switches()
	if len(switches) < 2 || switches[len(switches)-1] != "ORIG" {
		t.Fatalf("restore switch missing after cancel: %v", switches)
	}
}
