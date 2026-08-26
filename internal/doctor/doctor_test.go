package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ilyasmohaimel/frostgate/internal/setup"
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

func TestRunForProfileDoesNotDiscoverAnotherProfilesBenchmarkListener(t *testing.T) {
	root := t.TempDir()
	defaultDir := filepath.Join(root, "default")
	customDir := filepath.Join(root, "custom")
	if err := os.MkdirAll(defaultDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(customDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(defaultDir, "sing-box.json"), []byte(`{"inbounds":[{"type":"mixed","tag":"frostgate-benchmark","listen":"127.0.0.1","listen_port":2080}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(customDir, "config.yaml")
	configData := "singbox:\n  controller: http://127.0.0.1:9090\nendpoints:\n  - name: route\n    type: singbox\n    outbound: route\n"
	if err := os.WriteFile(configPath, []byte(configData), 0o600); err != nil {
		t.Fatal(err)
	}
	checks := RunForProfile(configPath, customDir, false)
	for _, check := range checks {
		if check.Name == "Benchmark proxy" && check.Status == "PASS" {
			t.Fatalf("custom profile borrowed benchmark listener: %#v", check)
		}
	}
}

func TestDirectOnlySkipsSingBoxWithEmptyPATH(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`endpoints:
  - name: direct
    type: http
    url: https://example.com/
`), 0o600); err != nil {
		t.Fatal(err)
	}
	emptyPATH := filepath.Join(dir, "empty-path")
	if err := os.Mkdir(emptyPATH, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", emptyPATH)
	checks := RunForProfile(path, dir, false)
	want := map[string]bool{"sing-box": false, "Sidecar service": false, "Controller": false}
	for _, check := range checks {
		if _, ok := want[check.Name]; ok {
			if check.Status != "SKIP" || !strings.Contains(check.Detail, "no proxy routes") {
				t.Fatalf("%s = %+v, want direct-only SKIP", check.Name, check)
			}
			want[check.Name] = true
		}
		if check.Status == "FAIL" {
			t.Fatalf("healthy direct-only doctor has unexpected failure: %+v", check)
		}
	}
	for name, found := range want {
		if !found {
			t.Fatalf("missing direct-only %s check: %+v", name, checks)
		}
	}
}

func TestDirectOnlySkipsStaleSingBoxTestURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`singbox:
  test_url: :not-a-url
  controller: not-a-controller
  secret_file: /does/not/exist
endpoints:
  - name: direct
    type: http
    url: https://example.com/
`), 0o600); err != nil {
		t.Fatal(err)
	}
	checks := RunForProfile(path, dir, false)
	for _, check := range checks {
		if check.Status == "FAIL" {
			t.Fatalf("direct-only doctor evaluated stale sing-box state: %+v", check)
		}
		if check.Name == "Test URL" && check.Status != "SKIP" {
			t.Fatalf("sing-box test URL = %+v, want SKIP", check)
		}
	}
}

func TestDoctorDNSUsesBoundedResolverContext(t *testing.T) {
	oldLookup, oldTimeout := doctorLookupHost, doctorLookupTimeout
	doctorLookupTimeout = 20 * time.Millisecond
	doctorLookupHost = func(ctx context.Context, _ string) ([]string, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	t.Cleanup(func() {
		doctorLookupHost = oldLookup
		doctorLookupTimeout = oldTimeout
	})
	start := time.Now()
	if err := checkDNS(); err == nil || time.Since(start) > time.Second {
		t.Fatalf("bounded doctor DNS error=%v elapsed=%s", err, time.Since(start))
	}
}

func TestDirectOnlyDoctorSurfacesOwnedActiveSidecar(t *testing.T) {
	home := t.TempDir()
	xdg := filepath.Join(home, "xdg")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dir := filepath.Join(xdg, "frostgate")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	unitPath := filepath.Join(xdg, "systemd", "user", "frostgate-singbox.service")
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unitPath, []byte(setup.BuildSystemdUnit("/bin/sing-box", dir)), 0o644); err != nil {
		t.Fatal(err)
	}
	oldStatus := systemdStatus
	systemdStatus = func(string) string { return "active" }
	t.Cleanup(func() { systemdStatus = oldStatus })
	check := directOnlySidecarCheck(filepath.Join(dir, "config.yaml"), dir)
	if check == nil || check.Status != "FAIL" || !strings.Contains(check.Detail, "stale") {
		t.Fatalf("stale owned sidecar check = %+v", check)
	}
}
