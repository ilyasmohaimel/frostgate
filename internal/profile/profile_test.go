package profile

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestForPathStableAndDistinct(t *testing.T) {
	a := ForPath("/tmp/frostgate/a/config.yaml", false)
	if again := ForPath("/tmp/frostgate/a/config.yaml", false); again != a {
		t.Fatalf("identity not stable: %#v != %#v", again, a)
	}
	b := ForPath("/tmp/frostgate/b/config.yaml", false)
	if a.Service == b.Service || a.ControllerAddr == b.ControllerAddr || a.BenchmarkAddr == b.BenchmarkAddr {
		t.Fatalf("profiles collided: %#v %#v", a, b)
	}
	d := ForPath("/anything/config.yaml", true)
	if d.Service != "frostgate-singbox.service" || d.ControllerAddr != "127.0.0.1:9090" || d.BenchmarkAddr != "127.0.0.1:2080" {
		t.Fatalf("default profile changed: %#v", d)
	}
}

func TestForPathResolvesExistingSymlinkAliases(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(config, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias.yaml")
	if err := os.Symlink(config, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got, want := ForPath(alias, false), ForPath(config, false); got != want {
		t.Fatalf("symlink aliases have different identities: %#v != %#v", got, want)
	}
}

func TestForPathResolvesSymlinkedParentBeforeConfigExists(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	aliasDir := filepath.Join(root, "alias")
	if err := os.Symlink(realDir, aliasDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	aliasConfig := filepath.Join(aliasDir, "config.yaml")
	realConfig := filepath.Join(realDir, "config.yaml")
	before := ForPath(aliasConfig, false)
	beforeState := StateDir(aliasConfig)
	want := ForPath(realConfig, false)
	wantState := StateDir(realConfig)
	if before != want || beforeState != wantState {
		t.Fatalf("missing config through symlinked parent was not canonicalized: before=%#v state=%q want=%#v state=%q", before, beforeState, want, wantState)
	}

	if err := os.WriteFile(realConfig, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	after := ForPath(aliasConfig, false)
	afterState := StateDir(aliasConfig)
	if after != before || afterState != beforeState {
		t.Fatalf("profile identity changed after config creation: before=%#v state=%q after=%#v state=%q", before, beforeState, after, afterState)
	}
}

func TestForPathResolvesDanglingSymlinkedParentBeforeConfigExists(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	aliasDir := filepath.Join(root, "alias")
	if err := os.Symlink(realDir, aliasDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	aliasConfig := filepath.Join(aliasDir, "config.yaml")
	realConfig := filepath.Join(realDir, "config.yaml")
	before := ForPath(aliasConfig, false)
	beforeState := StateDir(aliasConfig)
	want := ForPath(realConfig, false)
	wantState := StateDir(realConfig)
	if before != want || beforeState != wantState {
		t.Fatalf("dangling symlinked parent was not canonicalized: before=%#v state=%q want=%#v state=%q", before, beforeState, want, wantState)
	}

	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(realConfig, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	after := ForPath(aliasConfig, false)
	afterState := StateDir(aliasConfig)
	if after != before || afterState != beforeState {
		t.Fatalf("profile identity changed after dangling symlink target creation: before=%#v state=%q after=%#v state=%q", before, beforeState, after, afterState)
	}
}

func TestCheckAvailableRejectsProfileListenerCollision(t *testing.T) {
	identity := ForPath(filepath.Join(t.TempDir(), "config.yaml"), false)
	listener, err := net.Listen("tcp", identity.ControllerAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := identity.CheckAvailable(); err == nil {
		t.Fatal("active profile listener was accepted")
	}
}
