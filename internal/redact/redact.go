package redact

import (
	"net/url"
	"regexp"
	"strings"
)

var uuidRe = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
var proxyURLRe = regexp.MustCompile(`(?i)(vless|hysteria2|hy2)://[^\s"']+`)
var httpURLRe = regexp.MustCompile(`(?i)https?://[^\s"']+`)
var bearerRe = regexp.MustCompile(`(?i)bearer\s+[^\s,"']+`)

func URL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "[redacted]"
	}
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "vless://") || strings.HasPrefix(lower, "hysteria2://") || strings.HasPrefix(lower, "hy2://") {
		if u, err := url.Parse(s); err == nil {
			u.User = url.UserPassword("REDACTED", "REDACTED")
			q := u.Query()
			for _, k := range []string{"pbk", "sid", "sni", "fp", "flow", "type", "security", "encryption", "host", "path", "serviceName", "alpn", "fp", "pbk", "sid", "spx", "sni", "obfs", "obfs-password", "obfsPassword", "pinSHA256", "pin", "insecure"} {
				if q.Get(k) != "" {
					q.Set(k, "REDACTED")
				}
			}
			u.RawQuery = q.Encode()
			if u.Fragment != "" {
				u.Fragment = "REDACTED"
			}
			return u.String()
		}
		return lower[:6] + "[redacted]"
	}
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		if u, err := url.Parse(s); err == nil {
			// Keep scheme and host, redact rest
			if u.Host != "" {
				// Return scheme://host/[redacted] and preserve port if present
				return u.Scheme + "://" + u.Host + "/[redacted]"
			}
			return u.Scheme + "://[redacted]"
		}
		return "[redacted http url]"
	}
	// Generic errors often embed URLs after a verb such as `Get`. Replace the
	// entire credential-bearing proxy URL, and keep only scheme/host for HTTP.
	s = proxyURLRe.ReplaceAllString(s, "[REDACTED-PROXY-URL]")
	s = httpURLRe.ReplaceAllStringFunc(s, func(raw string) string {
		trimmed := strings.TrimRight(raw, ").]}")
		suffix := raw[len(trimmed):]
		u, err := url.Parse(trimmed)
		if err != nil || u.Host == "" {
			return "[redacted http url]" + suffix
		}
		return u.Scheme + "://" + u.Host + "/[redacted]" + suffix
	})
	s = bearerRe.ReplaceAllString(s, "Bearer [REDACTED]")
	s = uuidRe.ReplaceAllString(s, "REDACTED-UUID")
	// If it looks like a URL with query, redact query
	if idx := strings.Index(s, "?"); idx != -1 {
		s = s[:idx] + "?[redacted]"
	}
	if len(s) > 120 {
		return s[:80] + "...[redacted]"
	}
	return s
}

func Error(err error) string {
	if err == nil {
		return ""
	}
	return URL(err.Error())
}

func String(s string) string {
	return URL(s)
}
