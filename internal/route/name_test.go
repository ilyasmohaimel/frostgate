package route

import "testing"

func TestValidateName(t *testing.T) {
	valid := []string{"route1", "My_Route", "a.b-c", "X9", "vless-01", "hysteria2.node"}
	for _, n := range valid {
		if err := ValidateName(n); err != nil {
			t.Errorf("expected %q valid, got %v", n, err)
		}
	}
	invalid := map[string]error{
		"":             ErrRouteNameEmpty,
		"has space":    ErrRouteNameInvalid,
		"slash/in":     ErrRouteNameSlash,
		"back\\slash":  ErrRouteNameSlash,
		"new\nline":    ErrRouteNameControl,
		"carriage\r":   ErrRouteNameControl,
		"semi;colon":   ErrRouteNameInvalid,
		"dollar$sign":  ErrRouteNameInvalid,
		"pipe|char":    ErrRouteNameInvalid,
		"dot.eq=uals":  ErrRouteNameInvalid,
		"../../escape": ErrRouteNameSlash,
	}
	for n, want := range invalid {
		if err := ValidateName(n); err != want {
			t.Errorf("name %q: want %v, got %v", n, want, err)
		}
	}
	long := ""
	for i := 0; i < MaxRouteNameLen+10; i++ {
		long += "a"
	}
	if err := ValidateName(long); err != ErrRouteNameTooLong {
		t.Errorf("long name: want ErrRouteNameTooLong, got %v", err)
	}
}

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"My Route":        "My-Route",
		"a/b\\c":          "a-b-c",
		"foo bar baz":     "foo-bar-baz",
		"!!weird@@":       "weird",
		"":                "route",
		"vless#1":         "vless-1",
		"trailing space ": "trailing-space",
	}
	for in, want := range cases {
		if got := SanitizeName(in); got != want {
			t.Errorf("SanitizeName(%q)=%q want %q", in, got, want)
		}
	}
}
