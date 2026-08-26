package route

import (
	"crypto/sha256"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/ilyasmohaimel/frostgate/internal/routename"
	"github.com/ilyasmohaimel/frostgate/internal/setup"
)

var (
	ErrRouteNameEmpty   = routename.ErrEmpty
	ErrRouteNameTooLong = routename.ErrTooLong
	ErrRouteNameControl = routename.ErrControl
	ErrRouteNameSlash   = routename.ErrSlash
	ErrRouteNameInvalid = routename.ErrInvalid
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
	canonical := fmt.Sprintf("vless:%s:%d:%s:reality:%s:%s:%s:%s:%s:%s:%s:%t:%s", v.Server, v.ServerPort, v.UUID, v.SNI, v.PBK, v.SID, v.FP, v.Flow, v.SpiderX, v.PacketEncoding, v.PacketEncodingSet, v.Network)
	h := sha256.Sum256([]byte(canonical))
	return fmt.Sprintf("%x", h[:])
}

func FingerprintHysteria(h *setup.Hysteria2Outbound) string {
	canonical := fmt.Sprintf("hysteria2:%s:%d:%s:%s:%s:%s:%t:%s:%s", h.Server, h.ServerPort, h.Password, h.ObfsType, h.ObfsPassword, h.SNI, h.Insecure, h.PinSHA256, h.Network)
	hash := sha256.Sum256([]byte(canonical))
	return fmt.Sprintf("%x", hash[:])
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
			rawFrag := frag
			// URL decode fragment
			if decoded, err := parseFragment(frag); err == nil && decoded != "" {
				frag = decoded
			}
			if !secretLikeFragmentAtAnyEncoding(rawFrag) && !secretLikeFragmentAtAnyEncoding(frag) {
				frag = strings.ReplaceAll(frag, " ", "-")
				frag = sanitizeTag(frag)
				if frag != "" {
					return frag
				}
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

var secretLikeFragmentRe = regexp.MustCompile(`(?i)(^|[^a-z0-9])(?:token|secret|password|passwd|auth|authorization|api[-_]?key|access[-_]?token|refresh[-_]?token|uuid|private[-_]?key|public[-_]?key|pbk|sid)(?:$|[^a-z0-9])`)
var uuidFragmentRe = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var compactFragmentRe = regexp.MustCompile(`^[A-Za-z0-9+/=_-]{24,}$`)
var shortOpaqueFragmentRe = regexp.MustCompile(`^[A-Za-z0-9+/=_-]{16,23}$`)

const (
	maxFragmentDecodeDepth = 3
	maxFragmentDecodeBytes = 4096
)

func secretLikeFragmentAtAnyEncoding(fragment string) bool {
	seen := make(map[string]struct{}, maxFragmentDecodeDepth+1)
	for depth := 0; depth <= maxFragmentDecodeDepth; depth++ {
		if len(fragment) > maxFragmentDecodeBytes {
			return true
		}
		if secretLikeFragment(fragment) {
			return true
		}
		if _, ok := seen[fragment]; ok {
			return false
		}
		seen[fragment] = struct{}{}
		decoded, err := parseFragment(fragment)
		if err != nil || decoded == fragment || decoded == "" || len(decoded) > maxFragmentDecodeBytes {
			return false
		}
		fragment = decoded
	}
	return secretLikeFragment(fragment)
}

func secretLikeFragment(fragment string) bool {
	fragment = strings.TrimSpace(fragment)
	if fragment == "" || secretLikeFragmentRe.MatchString(fragment) || secretLikeFragmentRe.MatchString(splitCamelCase(fragment)) || uuidFragmentRe.MatchString(fragment) {
		return true
	}
	// Percent-encoded human labels are checked again after decoding by the
	// caller. Keep a valid escape sequence out of the opaque-name grammar so
	// that a label such as Home%2DOffice can still be retained.
	if strings.Contains(fragment, "%") {
		for i := 0; i < len(fragment); i++ {
			if fragment[i] != '%' {
				continue
			}
			if i+2 >= len(fragment) || !isHex(fragment[i+1]) || !isHex(fragment[i+2]) {
				return true
			}
			i += 2
		}
		return false
	}
	// A fragment is used as an implicit route name only when it has a small,
	// human-readable grammar. Opaque provider identifiers must not be copied
	// into route names, even when they happen to be short enough to look safe.
	if humanDelimitedFragment(fragment) || humanWordFragment(fragment) {
		return false
	}
	if !compactFragmentRe.MatchString(fragment) && !shortOpaqueFragmentRe.MatchString(fragment) {
		return true
	}
	return true
}

func humanDelimitedFragment(fragment string) bool {
	if !strings.ContainsAny(fragment, "-_+ ") {
		return false
	}
	parts := strings.FieldsFunc(fragment, func(r rune) bool {
		return r == '-' || r == '_' || r == '+' || r == ' '
	})
	if len(parts) < 2 {
		return false
	}
	hasWord := false
	for _, part := range parts {
		if part == "" {
			continue
		}
		if allDigits(part) {
			if len(part) > 4 {
				return false
			}
			continue
		}
		if len(part) > maxHumanWordBytes || !humanWord(part) {
			return false
		}
		hasWord = true
	}
	return hasWord
}

func humanWordFragment(fragment string) bool {
	return !strings.ContainsAny(fragment, "-_^+ ") && len(fragment) <= maxHumanWordBytes && humanWord(fragment)
}

const maxHumanWordBytes = 15

func humanWord(word string) bool {
	if len(word) < 1 {
		return false
	}
	for i, r := range word {
		if r >= 'a' && r <= 'z' {
			continue
		}
		if i == 0 && r >= 'A' && r <= 'Z' {
			continue
		}
		return false
	}
	return true
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isHex(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F'
}

func splitCamelCase(s string) string {
	var out strings.Builder
	out.Grow(len(s) + 4)
	for i := 0; i < len(s); i++ {
		if i > 0 && s[i] >= 'A' && s[i] <= 'Z' && s[i-1] >= 'a' && s[i-1] <= 'z' {
			out.WriteByte('-')
		}
		out.WriteByte(s[i])
	}
	return out.String()
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
