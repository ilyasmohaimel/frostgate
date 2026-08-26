package route

import "testing"

func TestDeriveNameFromFragmentPreservesPlusBeforeSanitization(t *testing.T) {
	tests := []struct {
		fragment string
		want     string
	}{
		{"HongKong+02", "hongkong-02"},
		{"HongKong%2B02", "hongkong-02"},
		{"Hong%20Kong", "hong-kong"},
		{"%E4%B8%AD%E6%96%87", "vless-01"},
		{"bad%ZZ", "vless-01"},
		{"token%3Dfixture-secret", "vless-01"},
	}
	for _, tt := range tests {
		link := "vless://example#" + tt.fragment
		if got := DeriveNameFromLink(link, 1); got != tt.want {
			t.Fatalf("DeriveNameFromLink(%q) = %q, want %q", tt.fragment, got, tt.want)
		}
	}
}
