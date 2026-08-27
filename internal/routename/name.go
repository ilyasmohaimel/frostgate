// Package routename defines the safe, shared policy for names used as route
// tags and Frostgate endpoint names.
package routename

import (
	"errors"
	"regexp"
	"strings"
	"unicode"
)

var (
	ErrEmpty   = errors.New("route name is empty")
	ErrTooLong = errors.New("route name too long")
	ErrControl = errors.New("route name contains control characters")
	ErrSlash   = errors.New("route name contains slash")
	ErrInvalid = errors.New("route name contains invalid characters")
)

// MaxLen is the maximum allowed route name length.
const MaxLen = 64

var valid = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Validate rejects names that are unsafe in endpoint and sing-box tags.
func Validate(name string) error {
	if name == "" {
		return ErrEmpty
	}
	if len(name) > MaxLen {
		return ErrTooLong
	}
	if err := ValidateControlCharacters(name); err != nil {
		return err
	}
	if strings.ContainsAny(name, "/\\") {
		return ErrSlash
	}
	if !valid.MatchString(name) {
		return ErrInvalid
	}
	return nil
}

// ValidateControlCharacters applies the terminal-safe part of the naming
// policy without imposing the ASCII route-tag policy on display names.
func ValidateControlCharacters(name string) error {
	for _, r := range name {
		if unicode.IsControl(r) || r == 0x7f {
			return ErrControl
		}
	}
	return nil
}

// Sanitize converts arbitrary display text into a safe route name.
func Sanitize(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "route"
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "route"
	}
	if len(out) > MaxLen {
		out = out[:MaxLen]
	}
	return out
}
