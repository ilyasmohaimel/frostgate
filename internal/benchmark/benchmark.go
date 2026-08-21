package benchmark

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/proxy"
)

const DefaultDownloadURL = "https://speed.cloudflare.com/__down?bytes=8000000"
const DefaultBytes = 8 * 1024 * 1024

func buildDownloadURL(raw string, bytes int64) (string, error) {
	if raw == "" {
		raw = DefaultDownloadURL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	q := u.Query()
	if q.Get("bytes") == "" && bytes > 0 {
		q.Set("bytes", fmt.Sprintf("%d", bytes))
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

func MeasureDirect(ctx context.Context, downloadURL string, bytes int64) (float64, time.Duration, error) {
	if bytes == 0 {
		bytes = DefaultBytes
	}
	u, err := buildDownloadURL(downloadURL, bytes)
	if err != nil {
		return 0, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return 0, 0, err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, 0, fmt.Errorf("http status %d", resp.StatusCode)
	}
	n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, bytes+1024*1024))
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
	mbps := float64(n*8) / elapsed.Seconds() / 1e6
	return mbps, elapsed, nil
}

func MeasureViaSOCKS(ctx context.Context, socksAddr, downloadURL string, bytes int64) (float64, time.Duration, error) {
	if socksAddr == "" {
		return 0, 0, fmt.Errorf("empty socks address")
	}
	if bytes == 0 {
		bytes = DefaultBytes
	}
	u, err := buildDownloadURL(downloadURL, bytes)
	if err != nil {
		return 0, 0, err
	}
	dialer, err := proxy.SOCKS5("tcp", socksAddr, nil, proxy.Direct)
	if err != nil {
		return 0, 0, fmt.Errorf("socks5: %w", err)
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		},
		ForceAttemptHTTP2: false,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
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
	n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, bytes+2*1024*1024))
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
	mbps := float64(n*8) / elapsed.Seconds() / 1e6
	return mbps, elapsed, nil
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
