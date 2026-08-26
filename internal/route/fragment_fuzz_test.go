package route

import (
	"testing"
	"unicode/utf8"

	"github.com/ilyasmohaimel/frostgate/internal/routename"
)

func FuzzDeriveNameFromFragment(f *testing.F) {
	for _, seed := range []string{
		"HongKong+02", "HongKong%2B02", "Hong%20Kong", "%E4%B8%AD%E6%96%87",
		"bad%ZZ", "token%3Dfixture-secret", "A1b2C3d4E5f6G7h8I9j0K1",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, fragment string) {
		name := DeriveNameFromLink("vless://fixture#"+fragment, 1)
		if !utf8.ValidString(name) {
			t.Fatalf("invalid UTF-8 route name %q", name)
		}
		if err := routename.Validate(name); err != nil {
			t.Fatalf("derived route name %q is invalid: %v", name, err)
		}
	})
}
