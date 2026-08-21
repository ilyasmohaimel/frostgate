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

func MeasureDirect(ctx context.Context, downloadURL string, bytes int64) (float64, time.Duration, error) {
	if downloadURL == "" {
		downloadURL = DefaultDownloadURL
	}
	if bytes == 0 {
		bytes = DefaultBytes
	}
	// Ensure URL has bytes param
	if !hasBytesParam(downloadURL) {
		downloadURL = fmt.Sprintf("%s?bytes=%d", downloadURL, bytes)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return 0, 0, err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, bytes+1024*1024))
	if err != nil {
		return 0, 0, err
	}
	elapsed := time.Since(start)
	if elapsed == 0 {
		return 0, elapsed, fmt.Errorf("zero elapsed")
	}
	mbps := float64(n*8) / elapsed.Seconds() / 1e6
	return mbps, elapsed, nil
}

func MeasureViaSOCKS(ctx context.Context, socksAddr, downloadURL string, bytes int64) (float64, time.Duration, error) {
	if downloadURL == "" {
		downloadURL = DefaultDownloadURL
	}
	if bytes == 0 {
		bytes = DefaultBytes
	}
	if !hasBytesParam(downloadURL) {
		// try to add bytes param if not present and using cloudflare
		if u, err := url.Parse(downloadURL); err == nil {
			q := u.Query()
			if q.Get("bytes") == "" {
				q.Set("bytes", fmt.Sprintf("%d", bytes))
				u.RawQuery = q.Encode()
				downloadURL = u.String()
			}
		}
	}
	// Create SOCKS dialer
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
	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return 0, 0, err
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, bytes+2*1024*1024))
	if err != nil {
		return 0, 0, err
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
