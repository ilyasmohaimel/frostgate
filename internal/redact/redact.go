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

const sensitiveKeyPattern = `uuid|pbk|public[_-]?key|short[_-]?id|sid|password|obfs[_-]?password|controller[_-]?secret|authorization|token|secret|auth|auth[_-]?str|access[_-]?token|refresh[_-]?token|api[_-]?key|client[_-]?secret|private[_-]?key`

var sensitiveKVRe = regexp.MustCompile(`(?i)(["']?\b(?:` + sensitiveKeyPattern + `)\b["']?)\s*[:=]\s*["']?[^\s,;&"']+`)
var sensitiveQuotedDoubleKVRe = regexp.MustCompile(`(?i)(["']?\b(?:` + sensitiveKeyPattern + `)\b["']?\s*[:=]\s*)"(?:\\.|[^"])*"`)
var sensitiveQuotedSingleKVRe = regexp.MustCompile(`(?i)(["']?\b(?:` + sensitiveKeyPattern + `)\b["']?\s*[:=]\s*)'(?:\\.|[^'])*'`)
var authorizationHeaderRe = regexp.MustCompile(`(?i)authorization\s*:\s*[^\r\n]+`)

func URL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "[redacted]"
	}
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "vless://") || strings.HasPrefix(lower, "hysteria2://") || strings.HasPrefix(lower, "hy2://") {
		if u, err := url.Parse(s); err == nil {
			// Share links are credentials. Preserve at most their protocol and
			// host, never individual query keys or fragments which providers
			// commonly use for tokens and names.
			if u.Host != "" {
				return strings.ToLower(u.Scheme) + "://" + u.Host + "/[REDACTED]"
			}
			return strings.ToLower(u.Scheme) + "://[REDACTED]"
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
	s = authorizationHeaderRe.ReplaceAllString(s, "Authorization: [REDACTED]")
	// Do this before the bare token expression so a secret containing spaces
	// cannot leave its trailing words visible in JSON, YAML, or shell errors.
	s = sensitiveQuotedDoubleKVRe.ReplaceAllString(s, `$1"[REDACTED]"`)
	s = sensitiveQuotedSingleKVRe.ReplaceAllString(s, `$1'[REDACTED]'`)
	s = sensitiveKVRe.ReplaceAllString(s, "$1=[REDACTED]")
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
