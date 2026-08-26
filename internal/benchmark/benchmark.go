package benchmark

import (
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

const DefaultDownloadURL = "https://speed.cloudflare.com/__down"
const DefaultBytes = 8 * 1024 * 1024

// buildDownloadURL returns the download URL with the configured byte count
// applied authoritatively as the `bytes` query parameter. Any pre-existing
// `bytes` value in the URL is replaced, while unrelated query parameters are
// preserved. When bytes <= 0 the URL is left untouched.
func BuildDownloadURL(raw string, bytes int64) (string, error) {
	if raw == "" {
		raw = DefaultDownloadURL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if bytes > 0 {
		q := u.Query()
		q.Set("bytes", fmt.Sprintf("%d", bytes))
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

func MeasureDirect(ctx context.Context, downloadURL string, bytes int64) (float64, time.Duration, error) {
	if bytes == 0 {
		bytes = DefaultBytes
	}
	u, err := BuildDownloadURL(downloadURL, bytes)
	if err != nil {
		return 0, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return 0, 0, err
	}
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, 0, fmt.Errorf("http status %d", resp.StatusCode)
	}
	n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, boundedReadLimit(bytes, 1024*1024)))
	if err != nil {
		return 0, 0, err
	}
	if bytes > 0 && n < bytes/2 {
		return 0, 0, fmt.Errorf("short body: got %d bytes want %d", n, bytes)
	}
	if ctx.Err() != nil {
		return 0, 0, ctx.Err()
	}
	elapsed := time.Since(start)
	if elapsed == 0 {
		return 0, elapsed, fmt.Errorf("zero elapsed")
	}
	mbps := float64(n) * 8 / elapsed.Seconds() / 1e6
	return mbps, elapsed, nil
}

func MeasureViaSOCKS(ctx context.Context, socksAddr, downloadURL string, bytes int64) (float64, time.Duration, error) {
	if socksAddr == "" {
		return 0, 0, fmt.Errorf("empty socks address")
	}
	if bytes == 0 {
		bytes = DefaultBytes
	}
	u, err := BuildDownloadURL(downloadURL, bytes)
	if err != nil {
		return 0, 0, err
	}
	dialer, err := proxy.SOCKS5("tcp", socksAddr, nil, contextForwardDialer{})
	if err != nil {
		return 0, 0, fmt.Errorf("socks5: %w", err)
	}
	contextDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		return 0, 0, fmt.Errorf("socks5 dialer does not support context cancellation")
	}
	transport := &http.Transport{
		DialContext: func(_ context.Context, network, address string) (net.Conn, error) {
			// net/http may supply an internal dial context that outlives the
			// request after Do returns. The measurement context is the public
			// cancellation contract and must bound SOCKS TCP + handshake I/O.
			return contextDialer.DialContext(ctx, network, address)
		},
		// Count bytes actually transferred on the benchmark connection, not
		// transparently decompressed application bytes.
		DisableCompression: true,
		ForceAttemptHTTP2:  false,
	}
	// The context watcher attached to the SOCKS connection must not survive a
	// successful one-off measurement through an idle keep-alive connection.
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return 0, 0, err
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, 0, fmt.Errorf("http status %d", resp.StatusCode)
	}
	n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, boundedReadLimit(bytes, 2*1024*1024)))
	if err != nil {
		return 0, 0, err
	}
	if bytes > 0 && n < bytes/2 {
		return 0, 0, fmt.Errorf("short body: got %d want %d", n, bytes)
	}
	if ctx.Err() != nil {
		return 0, 0, ctx.Err()
	}
	elapsed := time.Since(start)
	if elapsed == 0 {
		return 0, elapsed, fmt.Errorf("zero elapsed")
	}
	mbps := float64(n) * 8 / elapsed.Seconds() / 1e6
	return mbps, elapsed, nil
}

// contextForwardDialer gives x/net's SOCKS state machine a connection whose
// read/write operations are interrupted when its context ends. x/net's
// ContextDialer covers the TCP connect, but its SOCKS greeting can otherwise
// block in I/O after a successful connection.
type contextForwardDialer struct{}

func (contextForwardDialer) Dial(network, address string) (net.Conn, error) {
	return net.Dial(network, address)
}

func (contextForwardDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	return watchContextConn(ctx, conn), nil
}

type watchedConn struct {
	net.Conn
	stop      chan struct{}
	closeOnce sync.Once
}

func watchContextConn(ctx context.Context, conn net.Conn) *watchedConn {
	wrapped := &watchedConn{Conn: conn, stop: make(chan struct{})}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetDeadline(time.Now())
		case <-wrapped.stop:
		}
	}()
	return wrapped
}

func (c *watchedConn) Close() error {
	c.closeOnce.Do(func() { close(c.stop) })
	return c.Conn.Close()
}

func boundedReadLimit(bytes, slack int64) int64 {
	if bytes <= 0 || bytes > math.MaxInt64-slack {
		return math.MaxInt64
	}
	return bytes + slack
}

func hasBytesParam(u string) bool {
	parsed, err := url.Parse(u)
	if err != nil {
		return false
	}
	return parsed.Query().Get("bytes") != ""
}

// Exported for tests
func HasBytesParam(u string) bool { return hasBytesParam(u) }
