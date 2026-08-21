package route

import (
	"crypto/sha256"
	"fmt"
	"net/url"
	"strings"

	"github.com/ilyasmohaimel/frostgate/internal/setup"
)

type ImportedRoute struct {
	Name        string
	Protocol    string // vless, hysteria2
	Tag         string
	Server      string
	Port        int
	Fingerprint string // sha256 of canonical params, no secrets exposed
	VLESS       *setup.VLESSOutbound
	Hysteria    *setup.Hysteria2Outbound
}

func FingerprintVLESS(v *setup.VLESSOutbound) string {
	canonical := fmt.Sprintf("vless:%s:%d:%s:%s:%s:%s", v.Server, v.ServerPort, v.UUID, v.SNI, v.PBK, v.SID)
	h := sha256.Sum256([]byte(canonical))
	return fmt.Sprintf("%x", h[:8])
}

func FingerprintHysteria(h *setup.Hysteria2Outbound) string {
	canonical := fmt.Sprintf("hysteria2:%s:%d:%s:%s", h.Server, h.ServerPort, h.Password, h.ObfsPassword)
	hash := sha256.Sum256([]byte(canonical))
	return fmt.Sprintf("%x", hash[:8])
}

func ParseShareLink(link, tag string) (*ImportedRoute, error) {
	link = strings.TrimSpace(link)
	lower := strings.ToLower(link)
	if strings.HasPrefix(lower, "vless://") {
		v, err := setup.ParseVLESSShareLink(link, tag)
		if err != nil {
			return nil, err
		}
		return &ImportedRoute{
			Name:        tag,
			Protocol:    "vless",
			Tag:         tag,
			Server:      v.Server,
			Port:        v.ServerPort,
			Fingerprint: FingerprintVLESS(v),
			VLESS:       v,
		}, nil
	}
	if strings.HasPrefix(lower, "hysteria2://") || strings.HasPrefix(lower, "hy2://") {
		h, err := setup.ParseHysteria2ShareLink(link, tag)
		if err != nil {
			return nil, err
		}
		return &ImportedRoute{
			Name:        tag,
			Protocol:    "hysteria2",
			Tag:         tag,
			Server:      h.Server,
			Port:        h.ServerPort,
			Fingerprint: FingerprintHysteria(h),
			Hysteria:    h,
		}, nil
	}
	return nil, fmt.Errorf("unsupported scheme (want vless:// or hysteria2://)")
}

func DeriveNameFromLink(link string, idx int) string {
	link = strings.TrimSpace(link)
	// Try fragment
	if idxHash := strings.LastIndex(link, "#"); idxHash != -1 && idxHash+1 < len(link) {
		frag := strings.TrimSpace(link[idxHash+1:])
		if frag != "" {
			// URL decode fragment
			if decoded, err := parseFragment(frag); err == nil && decoded != "" {
				frag = decoded
			}
			frag = strings.ReplaceAll(frag, " ", "-")
			frag = sanitizeTag(frag)
			if frag != "" {
				return frag
			}
		}
	}
	lower := strings.ToLower(link)
	if strings.HasPrefix(lower, "vless://") {
		return fmt.Sprintf("vless-%02d", idx)
	}
	if strings.HasPrefix(lower, "hysteria2://") || strings.HasPrefix(lower, "hy2://") {
		return fmt.Sprintf("hysteria-%02d", idx)
	}
	return fmt.Sprintf("route-%02d", idx)
}

func parseFragment(frag string) (string, error) {
	if decoded, err := url.QueryUnescape(frag); err == nil {
		return decoded, nil
	}
	if decoded, err := url.PathUnescape(frag); err == nil {
		return decoded, nil
	}
	return frag, nil
}

func sanitizeTag(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else if r == ' ' {
			b.WriteRune('-')
		} else {
			b.WriteRune('-')
		}
	}
	out := b.String()
	out = strings.Trim(out, "-")
	if len(out) > 32 {
		out = out[:32]
	}
	out = strings.ReplaceAll(out, "--", "-")
	if out == "" {
		return ""
	}
	return out
}
