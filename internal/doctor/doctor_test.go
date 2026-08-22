package doctor

import (
	"os"
	"path/filepath"
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

func TestPrivateFileCheckRejectsAllGroupWorldPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		mode os.FileMode
		want string
	}{{0o600, "PASS"}, {0o640, "WARN"}, {0o644, "WARN"}, {0o660, "WARN"}, {0o604, "WARN"}, {0o666, "WARN"}} {
		if err := os.Chmod(path, tt.mode); err != nil {
			t.Fatal(err)
		}
		if got := privateFileCheck("test", path).Status; got != tt.want {
			t.Errorf("mode %04o status %s want %s", tt.mode, got, tt.want)
		}
	}
}
