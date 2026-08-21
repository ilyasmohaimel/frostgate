package probe

import (
	"context"
	"errors"
	"strings"
)

func categorizeError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "outbound-not-found"):
		return "outbound-not-found"
	case strings.Contains(s, "singbox-api"):
		return "singbox-api"
	case strings.Contains(s, "proxy-test"):
		return "proxy-test"
	case strings.Contains(s, "dns"), strings.Contains(s, "no such host"), strings.Contains(s, "name resolution"):
		return "dns"
	case strings.Contains(s, "connection refused"):
		return "connection-refused"
	case strings.Contains(s, "timeout"), strings.Contains(s, "deadline"), strings.Contains(s, "timed out"):
		return "timeout"
	case strings.Contains(s, "tls"), strings.Contains(s, "handshake"), strings.Contains(s, "certificate"):
		return "tls"
	case strings.Contains(s, "http"), strings.Contains(s, "server error"), strings.Contains(s, "status"):
		return "http"
	case strings.Contains(s, "tcp"), strings.Contains(s, "connect"):
		return "tcp"
	case strings.Contains(s, "cancel"):
		return "cancelled"
	default:
		return "unknown"
	}
}
