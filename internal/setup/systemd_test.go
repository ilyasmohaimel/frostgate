package setup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeSystemd struct {
	calls  []string
	runErr map[string]error
	output []byte
	outErr error
}

func (f *fakeSystemd) Run(name string, args ...string) error {
	f.calls = append(f.calls, strings.Join(append([]string{name}, args...), " "))
	return f.runErr[strings.Join(args, " ")]
}

func (f *fakeSystemd) Output(name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(append([]string{name}, args...), " "))
	return f.output, f.outErr
}

func TestBuildSystemdUnitUsesActualPathsAndQuotes(t *testing.T) {
	unit := BuildSystemdUnit(`/home/frost user/.local/bin/sing-box`, `/tmp/xdg config/frostgate%1`)
	if !strings.Contains(unit, `ExecStart="/home/frost user/.local/bin/sing-box" run -c "/tmp/xdg config/frostgate%%1/sing-box.json"`) {
		t.Fatalf("unit does not quote actual ExecStart paths:\n%s", unit)
	}
	if !strings.Contains(unit, `ReadWritePaths="/tmp/xdg config/frostgate%%1"`) {
		t.Fatalf("unit does not quote actual ReadWritePaths:\n%s", unit)
	}
}

func TestInstallSystemdServiceSurfacesCommandFailures(t *testing.T) {
	for _, failure := range []string{"daemon-reload", "enable", "restart"} {
		t.Run(failure, func(t *testing.T) {
			key := "--user daemon-reload"
			if failure == "enable" {
				key = "--user enable --now frostgate-singbox.service"
			} else if failure == "restart" {
				key = "--user restart frostgate-singbox.service"
			}
			runner := &fakeSystemd{runErr: map[string]error{key: errors.New("boom")}}
			path := filepath.Join(t.TempDir(), "systemd", "user", "frostgate-singbox.service")
			err := InstallSystemdService(path, "/bin/sing-box", "/tmp/frostgate", runner)
			if err == nil || !strings.Contains(err.Error(), failure) {
				t.Fatalf("error = %v, want %s failure", err, failure)
			}
			if _, statErr := os.Stat(path); statErr != nil {
				t.Fatalf("unit was not written before command failure: %v", statErr)
			}
		})
	}
}

func TestInstallSystemdServiceSurfacesInactiveAndStatusFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		runner *fakeSystemd
		want   string
	}{
		{"is-active command", &fakeSystemd{output: []byte(""), outErr: errors.New("boom")}, "check frostgate-singbox.service"},
		{"inactive status", &fakeSystemd{output: []byte("failed\n")}, "not active"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "frostgate.service")
			err := InstallSystemdService(path, "/bin/sing-box", "/tmp/frostgate", tc.runner)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestInstallSystemdServiceSuccess(t *testing.T) {
	runner := &fakeSystemd{output: []byte("active\n")}
	path := filepath.Join(t.TempDir(), "systemd", "user", "frostgate-singbox.service")
	if err := InstallSystemdService(path, "/bin/sing-box", "/tmp/frostgate", runner); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 4 {
		t.Fatalf("calls = %v, want daemon-reload, enable, restart, is-active", runner.calls)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `ExecStart="/bin/sing-box" run -c "/tmp/frostgate/sing-box.json"`) {
		t.Fatalf("unexpected unit:\n%s", data)
	}
}

func TestWriteSystemdUnitRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "unrelated.service")
	original := []byte("unrelated unit\n")
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "frostgate.service")
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := WriteSystemdUnit(path, "/bin/sing-box", dir); err == nil || !strings.Contains(err.Error(), "symlinked") {
		t.Fatalf("symlinked unit write = %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != string(original) {
		t.Fatalf("symlink target changed: %q err=%v", got, err)
	}
}

func TestWriteSystemdUnitRejectsControlCharacters(t *testing.T) {
	for _, bad := range []string{"\n", "\r", "\x00", "\x1f"} {
		path := filepath.Join(t.TempDir(), "frostgate.service")
		if err := WriteSystemdUnit(path, "/bin/sing-box"+bad, "/tmp/frostgate"); err == nil {
			t.Fatalf("control character %q was accepted", bad)
		}
	}
}
