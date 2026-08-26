package tui

import (
	"testing"
	"unicode/utf8"
)

func TestTruncateKeepsUTF8Valid(t *testing.T) {
	for _, tc := range []struct {
		input string
		limit int
		want  string
	}{
		{"ASCII route", 4, "ASC…"},
		{"abcd", 4, "abcd"},
		{"abcde", 4, "abc…"},
		{"éclair-route", 4, "écl…"},
		{"中文路由", 4, "中文路由"},
		{"中文路由X", 4, "中文路…"},
		{"emoji-😀-route", 4, "emo…"},
		{"mix-é中文😀", 4, "mix…"},
	} {
		got := truncate(tc.input, tc.limit)
		if !utf8.ValidString(got) {
			t.Fatalf("truncate(%q) returned invalid UTF-8 %q", tc.input, got)
		}
		if len([]rune(got)) > tc.limit {
			t.Fatalf("truncate(%q) exceeded rune limit: %q", tc.input, got)
		}
		if got != tc.want {
			t.Fatalf("truncate(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
