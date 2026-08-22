package route

import (
	"regexp"
	"strings"
)

// validNameRe allows a conservative set of characters for route names.
var validNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// MaxRouteNameLen is the maximum allowed route name length.
const MaxRouteNameLen = 64

// ValidateName validates a route name used for endpoints, link fragments,
// subscription-derived names and the --name flag. It rejects empty names,
// control characters, newlines, slashes and any injection-prone characters.
func ValidateName(name string) error {
	if name == "" {
		return ErrRouteNameEmpty
	}
	if len(name) > MaxRouteNameLen {
		return ErrRouteNameTooLong
	}
	if strings.ContainsAny(name, "\n\r\t") {
		return ErrRouteNameControl
	}
	if strings.ContainsAny(name, "/\\") {
		return ErrRouteNameSlash
	}
	if !validNameRe.MatchString(name) {
		return ErrRouteNameInvalid
	}
	return nil
}

// SanitizeName coerces an arbitrary string into a valid route name by
// replacing illegal characters with '-' and truncating to MaxRouteNameLen.
// If the result is empty it returns "route".
func SanitizeName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "route"
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '_' || r == '-':
			b.WriteRune(r)
		case r == ' ' || r == '/' || r == '\\':
			b.WriteRune('-')
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "route"
	}
	if len(out) > MaxRouteNameLen {
		out = out[:MaxRouteNameLen]
	}
	return out
}
