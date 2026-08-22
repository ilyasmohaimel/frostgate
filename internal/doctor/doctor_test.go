package doctor

import (
	"strings"
	"testing"
)

func TestFormatIsPlain(t *testing.T) {
	got := Format([]Check{{Name: "Config", Status: "PASS", Detail: "exists"}})
	if !strings.Contains(got, "Config") || !strings.Contains(got, "PASS") {
		t.Fatalf("missing check in %q", got)
	}
	if strings.Contains(got, "\x1b[") {
		t.Fatalf("plain format contains ANSI escapes: %q", got)
	}
}
