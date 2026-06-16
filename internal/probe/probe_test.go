package probe

import (
	"context"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/ilyasmohaimel/frostgate/internal/config"
)

func TestClassify(t *testing.T) {
	const timeout = 1000 * time.Millisecond
	tests := []struct {
		name    string
		err     error
		latency time.Duration
		want    Status
	}{
		{"error is down", errors.New("boom"), 10 * time.Millisecond, StatusDown},
		{"fast is up", nil, 100 * time.Millisecond, StatusUp},
		{"slow is degraded", nil, 800 * time.Millisecond, StatusDegraded},
		{"at threshold is up", nil, 600 * time.Millisecond, StatusUp},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.err, tt.latency, timeout); got != tt.want {
				t.Errorf("Classify(%v, %v) = %q, want %q", tt.err, tt.latency, got, tt.want)
			}
		})
	}
}

func TestRankOrder(t *testing.T) {
	mk := func(name string, s Status, lat time.Duration) Result {
		return Result{Endpoint: config.Endpoint{Name: name}, Status: s, Latency: lat}
	}
	results := []Result{
		mk("down", StatusDown, 5*time.Millisecond),
		mk("slow", StatusDegraded, 400*time.Millisecond),
		mk("fast", StatusUp, 50*time.Millisecond),
		mk("medium", StatusUp, 120*time.Millisecond),
	}

	Rank(results)

	wantOrder := []string{"fast", "medium", "slow", "down"}
	for i, want := range wantOrder {
		if results[i].Endpoint.Name != want {
			t.Errorf("rank position %d = %q, want %q", i, results[i].Endpoint.Name, want)
		}
	}

	best, ok := Best(results)
	if !ok || best.Endpoint.Name != "fast" {
		t.Errorf("Best() = (%q, %v), want (fast, true)", best.Endpoint.Name, ok)
	}
}

func TestProbeTCP(t *testing.T) {
	// A real listener gives us a guaranteed-open port (hermetic, no network).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())

	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}

	up := config.Endpoint{Name: "open", Host: "127.0.0.1", Port: port, Type: config.ProbeTCP}
	if r := Probe(context.Background(), up, time.Second); r.Status != StatusUp {
		t.Errorf("open port: got %q (%v), want up", r.Status, r.Err)
	}

	down := config.Endpoint{Name: "closed", Host: "127.0.0.1", Port: 1, Type: config.ProbeTCP}
	if r := Probe(context.Background(), down, time.Second); r.Status != StatusDown {
		t.Errorf("closed port: got %q, want down", r.Status)
	}
}
