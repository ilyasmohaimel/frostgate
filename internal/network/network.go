package network

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/ilyasmohaimel/frostgate/internal/redact"
)

type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // PASS, FAIL, UNAVAILABLE, SKIP
	Detail string `json:"detail,omitempty"`
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
		return Check{Name: "IPv4", Status: "FAIL", Detail: redact.Error(err)}
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
		return Check{Name: "HTTPS", Status: "FAIL", Detail: redact.Error(err)}
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
		return Check{Name: "Captive portal", Status: "SKIP", Detail: redact.Error(err)}
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
	_, err := FetchDoHAnswers(ctx, http.DefaultClient, DefaultDoHURL, "www.gstatic.com")
	if err != nil {
		return Check{Name: "DoH resolver", Status: "FAIL", Detail: redact.Error(err)}
	}
	return Check{Name: "DoH resolver", Status: "PASS", Detail: "DoH ok"}
}

// DefaultDoHURL is the public resolver used when no override is configured.
const DefaultDoHURL = "https://cloudflare-dns.com/dns-query"

// systemLookup is swappable for tests.
var systemLookup = net.LookupHost

// NormalizeAddrs lowercases, trims and sorts an address set.
func NormalizeAddrs(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, a := range in {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// CompareSets returns MATCH or DIFFER for two normalized address sets.
func CompareSets(sys, doh []string) string {
	sysN := NormalizeAddrs(sys)
	dohN := NormalizeAddrs(doh)
	if len(sysN) == 0 || len(dohN) == 0 {
		return "UNAVAILABLE"
	}
	if len(sysN) != len(dohN) {
		return "DIFFER"
	}
	for i := range sysN {
		if sysN[i] != dohN[i] {
			return "DIFFER"
		}
	}
	return "MATCH"
}

type dohResponse struct {
	Status int `json:"Status"`
	Answer []struct {
		Name string `json:"name"`
		Type int    `json:"type"`
		Data string `json:"data"`
	} `json:"Answer"`
}

// FetchDoHAnswers queries a DoH JSON endpoint (RFC 8484 style JSON API) for
// both A and AAAA records of host and returns the normalized answer set.
func FetchDoHAnswers(ctx context.Context, client *http.Client, dohBase, host string) ([]string, error) {
	if client == nil {
		client = http.DefaultClient
	}
	var out []string
	for _, qtype := range []string{"A", "AAAA"} {
		u, err := url.Parse(dohBase)
		if err != nil {
			return nil, err
		}
		q := u.Query()
		q.Set("name", host)
		q.Set("type", qtype)
		u.RawQuery = q.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("accept", "application/dns-json")
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("doh status %d", resp.StatusCode)
		}
		var dr dohResponse
		if err := json.Unmarshal(body, &dr); err != nil {
			return nil, fmt.Errorf("malformed DoH response: %w", err)
		}
		for _, a := range dr.Answer {
			if a.Data != "" {
				out = append(out, a.Data)
			}
		}
	}
	return out, nil
}

// CheckDNSVsDoH compares system resolver answers with DoH answers for host.
// DIFFER is only a signal (CDNs legitimately differ); it is NOT evidence of
// DNS poisoning on its own.
func CheckDNSVsDoH(host, dohBase string) Check {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	sys, sysErr := systemLookup(host)
	doh, dohErr := FetchDoHAnswers(ctx, http.DefaultClient, dohBase, host)
	switch {
	case sysErr != nil && dohErr != nil:
		return Check{Name: "DNS vs DoH", Status: "UNAVAILABLE", Detail: "system and DoH lookups unavailable"}
	case sysErr != nil:
		return Check{Name: "DNS vs DoH", Status: "UNAVAILABLE", Detail: "system lookup failed"}
	case dohErr != nil:
		return Check{Name: "DNS vs DoH", Status: "UNAVAILABLE", Detail: "DoH lookup failed"}
	}
	verdict := CompareSets(sys, doh)
	switch verdict {
	case "MATCH":
		return Check{Name: "DNS vs DoH", Status: "PASS", Detail: fmt.Sprintf("MATCH (%s)", host)}
	case "DIFFER":
		return Check{Name: "DNS vs DoH", Status: "WARN", Detail: fmt.Sprintf("DIFFER for %s (signal only — CDNs can legitimately differ)", host)}
	default:
		return Check{Name: "DNS vs DoH", Status: "UNAVAILABLE", Detail: "no comparable answers"}
	}
}

func checkDNSDiff() Check {
	return CheckDNSVsDoH("www.gstatic.com", DefaultDoHURL)
}

func CheckDestinations(urls []string) []Check {
	var out []Check
	for _, u := range urls {
		out = append(out, checkDestination(u))
	}
	return out
}

func checkDestination(u string) Check {
	name := redact.String(u)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Check{Name: name, Status: "FAIL", Detail: redact.Error(err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return Check{Name: name, Status: "PASS", Detail: fmt.Sprintf("%d", resp.StatusCode)}
	}
	return Check{Name: name, Status: "FAIL", Detail: fmt.Sprintf("%d", resp.StatusCode)}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	s := redact.Error(err)
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
