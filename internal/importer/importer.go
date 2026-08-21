package importer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Result struct {
	Links   []string
	Skipped []Skipped
}

type Skipped struct {
	Reason string
	Line   string
}

func FetchSubscription(ctx context.Context, subURL string) ([]byte, error) {
	return FetchSubscriptionWithOpts(ctx, subURL, false)
}

func FetchSubscriptionWithOpts(ctx context.Context, subURL string, allowInsecure bool) ([]byte, error) {
	if subURL == "" {
		return nil, fmt.Errorf("empty url")
	}
	u, err := url.Parse(subURL)
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("unsupported scheme %q (want https)", u.Scheme)
	}
	if u.Scheme == "http" && !allowInsecure {
		return nil, fmt.Errorf("http subscription requires --allow-insecure-http (use https)")
	}
	if strings.EqualFold(u.Scheme, "file") || strings.EqualFold(u.Scheme, "data") || strings.EqualFold(u.Scheme, "javascript") {
		return nil, fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx2, "GET", subURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "frostgate/0.4.0")
	origIsHTTPS := u.Scheme == "https"
	client := &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			if req.URL.Scheme != "https" && req.URL.Scheme != "http" {
				return fmt.Errorf("unsupported redirect scheme %q", req.URL.Scheme)
			}
			if origIsHTTPS && req.URL.Scheme == "http" && !allowInsecure {
				return fmt.Errorf("https to http downgrade blocked (use --allow-insecure-http)")
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 2*1024*1024 {
		return nil, fmt.Errorf("response too large (>2MiB)")
	}
	return data, nil
}

func ParseSubscription(data []byte) Result {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return Result{}
	}
	// Try base64 first if looks like base64 and not obviously link/yaml/json
	if decoded, err := tryBase64Decode(data); err == nil {
		if isSingBoxJSON(decoded) {
			links, skipped := parseSingBoxJSON(decoded)
			return Result{Links: links, Skipped: skipped}
		}
		if isClashYAML(decoded) {
			links, skipped := parseClashYAML(decoded)
			return Result{Links: links, Skipped: skipped}
		}
		if isLinkList(decoded) {
			return parseLinkList(decoded)
		}
	}
	if isSingBoxJSON(data) {
		links, skipped := parseSingBoxJSON(data)
		return Result{Links: links, Skipped: skipped}
	}
	if isClashYAML(data) {
		links, skipped := parseClashYAML(data)
		return Result{Links: links, Skipped: skipped}
	}
	if isLinkList(data) {
		return parseLinkList(data)
	}
	return parseLinkList(data)
}

func isSingBoxJSON(data []byte) bool {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return false
	}
	if _, ok := m["outbounds"]; ok {
		return true
	}
	return false
}

func isClashYAML(data []byte) bool {
	s := string(data)
	return strings.Contains(s, "proxies:") || (strings.Contains(s, "Proxy:") && strings.Contains(s, "name:"))
}

func isLinkList(data []byte) bool {
	s := strings.TrimSpace(string(data))
	if s == "" {
		return false
	}
	lines := strings.Split(s, "\n")
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		ll := strings.ToLower(l)
		if strings.HasPrefix(ll, "vless://") || strings.HasPrefix(ll, "hysteria2://") || strings.HasPrefix(ll, "hy2://") || strings.HasPrefix(ll, "trojan://") || strings.HasPrefix(ll, "ss://") || strings.HasPrefix(ll, "vmess://") {
			return true
		}
	}
	return false
}

func tryBase64Decode(data []byte) ([]byte, error) {
	s := strings.TrimSpace(string(data))
	s = strings.ReplaceAll(s, "\n", "")
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "\t", "")
	if s == "" {
		return nil, fmt.Errorf("empty")
	}
	// must be base64 alphabet only
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
		if decoded, err := enc.DecodeString(s); err == nil {
			if len(decoded) > 10 && isLikelyDecoded(decoded) {
				return decoded, nil
			}
		}
	}
	return nil, fmt.Errorf("not base64")
}

func isLikelyDecoded(b []byte) bool {
	s := string(b)
	return strings.Contains(s, "vless://") || strings.Contains(s, "hysteria2://") || strings.Contains(s, "hy2://") || strings.Contains(s, "proxies:") || strings.Contains(s, "outbounds")
}

func parseLinkList(data []byte) Result {
	var links []string
	var skipped []Skipped
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "vless://") || strings.HasPrefix(lower, "hysteria2://") || strings.HasPrefix(lower, "hy2://") {
			links = append(links, line)
		} else if strings.HasPrefix(lower, "trojan://") || strings.HasPrefix(lower, "ss://") || strings.HasPrefix(lower, "vmess://") || strings.HasPrefix(lower, "ssr://") {
			skipped = append(skipped, Skipped{Reason: "unsupported protocol: " + strings.Split(lower, "://")[0], Line: redactLine(line)})
		} else {
			skipped = append(skipped, Skipped{Reason: "unsupported or malformed", Line: redactLine(line)})
		}
	}
	return Result{Links: links, Skipped: skipped}
}

func parseClashYAML(data []byte) (links []string, skipped []Skipped) {
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		skipped = append(skipped, Skipped{Reason: "invalid clash yaml: " + err.Error(), Line: ""})
		return
	}
	proxies, ok := m["proxies"]
	if !ok {
		proxies, ok = m["Proxy"]
		if !ok {
			skipped = append(skipped, Skipped{Reason: "no proxies in clash yaml", Line: ""})
			return
		}
	}
	list, ok := proxies.([]any)
	if !ok {
		skipped = append(skipped, Skipped{Reason: "invalid proxies", Line: ""})
		return
	}
	for _, p := range list {
		mm, ok := p.(map[string]any)
		if !ok {
			skipped = append(skipped, Skipped{Reason: "invalid proxy entry", Line: ""})
			continue
		}
		typ, _ := mm["type"].(string)
		name, _ := mm["name"].(string)
		typLower := strings.ToLower(typ)
		switch typLower {
		case "vless":
			uuid, _ := mm["uuid"].(string)
			server, _ := mm["server"].(string)
			portVal := mm["port"]
			port := toInt(portVal)
			if server == "" || port == 0 || uuid == "" {
				skipped = append(skipped, Skipped{Reason: "malformed VLESS route", Line: typ})
				continue
			}
			flow, _ := mm["flow"].(string)
			sni := ""
			if v, ok := mm["servername"].(string); ok {
				sni = v
			}
			if sni == "" {
				if v, ok := mm["sni"].(string); ok {
					sni = v
				}
			}
			pbk := ""
			sid := ""
			fp := "chrome"
			if v, ok := mm["client-fingerprint"].(string); ok && v != "" {
				fp = v
			}
			if v, ok := mm["fingerprint"].(string); ok && v != "" {
				fp = v
			}
			if opts, ok := mm["reality-opts"].(map[string]any); ok {
				if pk, ok := opts["public-key"].(string); ok {
					pbk = pk
				}
				if si, ok := opts["short-id"].(string); ok {
					sid = si
				}
				if pb, ok := opts["public_key"].(string); ok && pbk == "" {
					pbk = pb
				}
				if si2, ok := opts["short_id"].(string); ok && sid == "" {
					sid = si2
				}
			}
			if pbk == "" {
				if v, ok := mm["public-key"].(string); ok {
					pbk = v
				}
			}
			if pbk == "" {
				skipped = append(skipped, Skipped{Reason: "unsupported VLESS transport or missing reality pbk", Line: typ})
				continue
			}
			if sni == "" {
				skipped = append(skipped, Skipped{Reason: "malformed VLESS route missing sni", Line: typ})
				continue
			}
			q := url.Values{}
			q.Set("encryption", "none")
			q.Set("security", "reality")
			q.Set("sni", sni)
			q.Set("pbk", pbk)
			if sid != "" {
				q.Set("sid", sid)
			}
			q.Set("fp", fp)
			if flow != "" {
				q.Set("flow", flow)
			}
			q.Set("type", "tcp")
			fragment := url.QueryEscape(name)
			link := fmt.Sprintf("vless://%s@%s:%d?%s#%s", uuid, server, port, q.Encode(), fragment)
			links = append(links, link)
		case "hysteria2", "hysteria":
			server, _ := mm["server"].(string)
			port := toInt(mm["port"])
			password, _ := mm["password"].(string)
			if password == "" {
				if v, ok := mm["auth"].(string); ok {
					password = v
				}
			}
			if server == "" || port == 0 || password == "" {
				skipped = append(skipped, Skipped{Reason: "malformed Hysteria2 route", Line: typ})
				continue
			}
			sni, _ := mm["sni"].(string)
			if sni == "" {
				if v, ok := mm["servername"].(string); ok {
					sni = v
				}
			}
			obfs, _ := mm["obfs"].(string)
			obfsPass, _ := mm["obfs-password"].(string)
			if obfsPass == "" {
				if v, ok := mm["obfsPassword"].(string); ok {
					obfsPass = v
				}
			}
			q := url.Values{}
			if sni != "" {
				q.Set("sni", sni)
			}
			if obfs != "" {
				q.Set("obfs", obfs)
				if obfs == "salamander" && obfsPass != "" {
					q.Set("obfs-password", obfsPass)
				}
			}
			if v, ok := mm["skip-cert-verify"].(bool); ok && v {
				q.Set("insecure", "1")
			}
			fragment := url.QueryEscape(name)
			u := fmt.Sprintf("hysteria2://%s@%s:%d", url.QueryEscape(password), server, port)
			if len(q) > 0 {
				u += "?" + q.Encode()
			}
			u += "#" + fragment
			links = append(links, u)
		default:
			skipped = append(skipped, Skipped{Reason: "unsupported clash type: " + typ, Line: typ})
		}
	}
	return
}

func toInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case string:
		n, _ := strconv.Atoi(x)
		return n
	}
	return 0
}

func parseSingBoxJSON(data []byte) (links []string, skipped []Skipped) {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		skipped = append(skipped, Skipped{Reason: "invalid json: " + err.Error(), Line: ""})
		return
	}
	outbounds, ok := m["outbounds"].([]any)
	if !ok {
		skipped = append(skipped, Skipped{Reason: "no outbounds", Line: ""})
		return
	}
	for _, ob := range outbounds {
		mm, ok := ob.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := mm["type"].(string)
		tag, _ := mm["tag"].(string)
		switch typ {
		case "vless":
			server, _ := mm["server"].(string)
			port := toInt(mm["server_port"])
			uuid, _ := mm["uuid"].(string)
			if server == "" || port == 0 || uuid == "" {
				skipped = append(skipped, Skipped{Reason: "malformed VLESS route", Line: typ})
				continue
			}
			flow, _ := mm["flow"].(string)
			sni := ""
			pbk := ""
			sid := ""
			fp := "chrome"
			if tlsRaw, ok := mm["tls"].(map[string]any); ok {
				if sn, ok := tlsRaw["server_name"].(string); ok {
					sni = sn
				}
				if utls, ok := tlsRaw["utls"].(map[string]any); ok {
					if f, ok := utls["fingerprint"].(string); ok && f != "" {
						fp = f
					}
				}
				if reality, ok := tlsRaw["reality"].(map[string]any); ok {
					if pk, ok := reality["public_key"].(string); ok {
						pbk = pk
					}
					if si, ok := reality["short_id"].(string); ok {
						sid = si
					}
				}
			}
			if pbk == "" {
				skipped = append(skipped, Skipped{Reason: "unsupported VLESS transport or missing pbk", Line: typ})
				continue
			}
			if sni == "" {
				skipped = append(skipped, Skipped{Reason: "malformed VLESS route missing sni", Line: typ})
				continue
			}
			q := url.Values{}
			q.Set("encryption", "none")
			q.Set("security", "reality")
			q.Set("sni", sni)
			q.Set("pbk", pbk)
			if sid != "" {
				q.Set("sid", sid)
			}
			q.Set("fp", fp)
			if flow != "" {
				q.Set("flow", flow)
			}
			q.Set("type", "tcp")
			fname := tag
			if fname == "" {
				fname = fmt.Sprintf("vless-%d", port)
			}
			link := fmt.Sprintf("vless://%s@%s:%d?%s#%s", uuid, server, port, q.Encode(), url.QueryEscape(fname))
			links = append(links, link)
		case "hysteria2":
			server, _ := mm["server"].(string)
			port := toInt(mm["server_port"])
			password, _ := mm["password"].(string)
			if server == "" || port == 0 || password == "" {
				skipped = append(skipped, Skipped{Reason: "malformed Hysteria2 route", Line: typ})
				continue
			}
			sni := ""
			if tlsRaw, ok := mm["tls"].(map[string]any); ok {
				if sn, ok := tlsRaw["server_name"].(string); ok {
					sni = sn
				}
			}
			obfsPass := ""
			obfsType := ""
			if obfs, ok := mm["obfs"].(map[string]any); ok {
				if t, ok := obfs["type"].(string); ok {
					obfsType = t
				}
				if p, ok := obfs["password"].(string); ok {
					obfsPass = p
				}
			}
			q := url.Values{}
			if sni != "" {
				q.Set("sni", sni)
			}
			if obfsType != "" {
				q.Set("obfs", obfsType)
				if obfsPass != "" {
					q.Set("obfs-password", obfsPass)
				}
			}
			fname := tag
			if fname == "" {
				fname = fmt.Sprintf("hysteria-%d", port)
			}
			u := fmt.Sprintf("hysteria2://%s@%s:%d", url.QueryEscape(password), server, port)
			if len(q) > 0 {
				u += "?" + q.Encode()
			}
			u += "#" + url.QueryEscape(fname)
			links = append(links, u)
		default:
			if typ != "direct" && typ != "block" && typ != "selector" && typ != "urltest" {
				skipped = append(skipped, Skipped{Reason: "unsupported outbound: " + typ, Line: typ})
			}
		}
	}
	return
}

func redactLine(line string) string {
	if len(line) > 60 {
		return line[:30] + "...[redacted]"
	}
	if idx := strings.Index(line, "://"); idx != -1 {
		return line[:idx+3] + "[redacted]"
	}
	return "[redacted]"
}

func ExtractLinks(data []byte) []string {
	var out []string
	s := string(data)
	for _, prefix := range []string{"vless://", "hysteria2://", "hy2://"} {
		idx := 0
		for {
			pos := strings.Index(s[idx:], prefix)
			if pos == -1 {
				break
			}
			start := idx + pos
			end := start
			for end < len(s) && s[end] != '\n' && s[end] != '\r' && s[end] != ' ' && s[end] != '"' && s[end] != '\'' {
				end++
			}
			out = append(out, s[start:end])
			idx = end
		}
	}
	return out
}

func HasBytesParam(u string) bool {
	parsed, err := url.Parse(u)
	if err != nil {
		return false
	}
	return parsed.Query().Get("bytes") != ""
}

func IsSubscriptionURL(urlStr string) bool {
	u, err := url.Parse(urlStr)
	if err != nil {
		return false
	}
	return u.Scheme == "https" || u.Scheme == "http"
}
