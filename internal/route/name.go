package route

import (
	"github.com/ilyasmohaimel/frostgate/internal/routename"
)

// MaxRouteNameLen is the maximum allowed route name length.
const MaxRouteNameLen = routename.MaxLen

// ValidateName validates a route name used for endpoints, link fragments,
// subscription-derived names and the --name flag. It rejects empty names,
// control characters, newlines, slashes and any injection-prone characters.
func ValidateName(name string) error {
	return routename.Validate(name)
}

// SanitizeName coerces an arbitrary string into a valid route name by
// replacing illegal characters with '-' and truncating to MaxRouteNameLen.
// If the result is empty it returns "route".
func SanitizeName(name string) string {
	return routename.Sanitize(name)
}
