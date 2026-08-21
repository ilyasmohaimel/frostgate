package network

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

type Check struct {
	Name   string
	Status string // PASS, FAIL, UNAVAILABLE, SKIP
	Detail string
}

func CheckAll() []Check {
	var out []Check
	out = append(out, checkIPv4())
	out = append(out, checkIPv6())
	out = append(out, checkHTTPS())
	out = append(out, checkCaptive())
	out = append(out, checkDNS())
	out = append(out, checkDoH())
	out = append(out, checkDNSDiff())
	// Proxy route checks will be added by caller
	return out
}

func checkIPv4() Check {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", "1.1.1.1:80")
	if err != nil {
		return Check{Name: "IPv4", Status: "FAIL", Detail: err.Error()}
	}
	conn.Close()
	return Check{Name: "IPv4", Status: "PASS", Detail: "tcp 1.1.1.1:80 ok"}
}

func checkIPv6() Check {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", "[2606:4700:4700::1111]:80")
	if err != nil {
		return Check{Name: "IPv6", Status: "UNAVAILABLE", Detail: "no IPv6 route"}
	}
	conn.Close()
	return Check{Name: "IPv6", Status: "PASS", Detail: "tcp [2606:4700:4700::1111]:80 ok"}
}

func checkHTTPS() Check {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://www.gstatic.com/generate_204", nil)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Check{Name: "HTTPS", Status: "FAIL", Detail: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode == 204 {
		return Check{Name: "HTTPS", Status: "PASS", Detail: "generate_204 204 ok"}
	}
	return Check{Name: "HTTPS", Status: "FAIL", Detail: fmt.Sprintf("status %d", resp.StatusCode)}
}

func checkCaptive() Check {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://www.gstatic.com/generate_204", nil)
	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return Check{Name: "Captive portal", Status: "SKIP", Detail: err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode == 204 && len(body) == 0 {
		return Check{Name: "Captive portal", Status: "PASS", Detail: "NO (204 empty)"}
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return Check{Name: "Captive portal", Status: "FAIL", Detail: "redirect detected"}
	}
	if len(body) > 0 {
		return Check{Name: "Captive portal", Status: "FAIL", Detail: "unexpected body"}
	}
	return Check{Name: "Captive portal", Status: "PASS", Detail: "NO"}
}

func checkDNS() Check {
	addrs, err := net.LookupHost("www.gstatic.com")
	if err != nil || len(addrs) == 0 {
		return Check{Name: "System resolver", Status: "FAIL", Detail: errString(err)}
	}
	return Check{Name: "System resolver", Status: "PASS", Detail: strings.Join(addrs[:min(2, len(addrs))], ",")}
}

func checkDoH() Check {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Use Cloudflare DoH JSON
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://cloudflare-dns.com/dns-query?name=www.gstatic.com&type=A", nil)
	req.Header.Set("accept", "application/dns-json")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Check{Name: "DoH resolver", Status: "FAIL", Detail: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return Check{Name: "DoH resolver", Status: "FAIL", Detail: fmt.Sprintf("status %d", resp.StatusCode)}
	}
	return Check{Name: "DoH resolver", Status: "PASS", Detail: "cloudflare DoH ok"}
}

func checkDNSDiff() Check {
	// Compare system vs DoH: for MVP, just report if both passed
	sys, _ := net.LookupHost("www.gstatic.com")
	// DoH result would need parsing, but for MVP assume differ if sys fails?
	if len(sys) == 0 {
		return Check{Name: "Answers differ", Status: "SKIP", Detail: "no data"}
	}
	return Check{Name: "Answers differ", Status: "SKIP", Detail: "not directly tested (CDN may differ)"}
}

func CheckDestinations(urls []string) []Check {
	var out []Check
	for _, u := range urls {
		out = append(out, checkDestination(u))
	}
	return out
}

func checkDestination(u string) Check {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Check{Name: u, Status: "FAIL", Detail: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return Check{Name: u, Status: "PASS", Detail: fmt.Sprintf("%d", resp.StatusCode)}
	}
	return Check{Name: u, Status: "FAIL", Detail: fmt.Sprintf("%d", resp.StatusCode)}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
