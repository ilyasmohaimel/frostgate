package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/ilyasmohaimel/frostgate/internal/managedconfig"
	"github.com/ilyasmohaimel/frostgate/internal/profile"
)

func TestCustomConfigUsesIsolatedManagedDirectory(t *testing.T) {
	defaultDir := filepath.Join(t.TempDir(), "default")
	customDir := filepath.Join(t.TempDir(), "profile")
	if err := os.MkdirAll(defaultDir, 0o700); err != nil {
		t.Fatal(err)
	}
	defaultSidecar := []byte(`{"outbounds":[{"type":"direct","tag":"direct"}]}`)
	if err := os.WriteFile(filepath.Join(defaultDir, "sing-box.json"), defaultSidecar, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(customDir, 0o700); err != nil {
		t.Fatal(err)
	}
	customConfig := filepath.Join(customDir, "config.yaml")
	if err := os.WriteFile(customConfig, []byte("endpoints:\n  - name: direct\n    type: http\n    url: https://www.gstatic.com/generate_204\n    expected_status: 204\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, managedDir := commandPaths(customConfig)
	if resolved != customConfig || managedDir != profile.StateDir(customConfig) {
		t.Fatalf("custom paths = %q %q", resolved, managedDir)
	}
	runner := &managedconfig.SidecarRunner{Validator: func([]byte) error { return nil }}
	_, err := addManagedRoute(managedDir, resolved, "vless://11111111-1111-4111-8111-111111111111@203.0.113.10:443?security=reality&type=tcp&sni=example.com&pbk=fixture-key", "profile-route", runner)
	if err != nil {
		t.Fatal(err)
	}
	gotDefault, err := os.ReadFile(filepath.Join(defaultDir, "sing-box.json"))
	if err != nil || !bytes.Equal(gotDefault, defaultSidecar) {
		t.Fatalf("custom config mutated default sidecar: %q err=%v", gotDefault, err)
	}
	if _, err := os.Stat(filepath.Join(managedDir, "sing-box.json")); err != nil {
		t.Fatalf("custom managed sidecar missing: %v", err)
	}
}

func TestExplicitSiblingConfigsHaveIndependentState(t *testing.T) {
	dir := t.TempDir()
	left := filepath.Join(dir, "china.yaml")
	right := filepath.Join(dir, "home.yaml")
	for _, path := range []string{left, right} {
		if err := os.WriteFile(path, []byte("endpoints: []\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	leftConfig, leftState := commandPaths(left)
	rightConfig, rightState := commandPaths(right)
	if leftConfig != left || rightConfig != right || leftState == rightState {
		t.Fatalf("sibling profiles overlap: left=%q right=%q", leftState, rightState)
	}
	leftIdentity := profileForPaths(leftConfig, leftState)
	rightIdentity := profileForPaths(rightConfig, rightState)
	if leftIdentity.ID == rightIdentity.ID || leftIdentity.Service == rightIdentity.Service || leftIdentity.ControllerAddr == rightIdentity.ControllerAddr || leftIdentity.BenchmarkAddr == rightIdentity.BenchmarkAddr {
		t.Fatalf("sibling profile identities overlap: %+v %+v", leftIdentity, rightIdentity)
	}
}

func TestExplicitAlternateInDefaultDirectoryIsNotDefault(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	t.Setenv("HOME", filepath.Join(root, "home"))
	defaultDir := filepath.Join(root, "frostgate")
	if err := os.MkdirAll(defaultDir, 0o700); err != nil {
		t.Fatal(err)
	}
	alternate := filepath.Join(defaultDir, "alternate.yaml")
	if err := os.WriteFile(alternate, []byte("endpoints: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath, state := commandPaths(alternate)
	identity := profileForPaths(configPath, state)
	if identity.ID == profile.DefaultID || state == defaultDir {
		t.Fatalf("explicit alternate became default: state=%q identity=%+v", state, identity)
	}
}

func TestExplicitCanonicalAliasesUseSameState(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("endpoints: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias.yaml")
	if err := os.Symlink(configPath, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	canonicalConfig, canonicalState := commandPaths(configPath)
	aliasConfig, aliasState := commandPaths(alias)
	if canonicalConfig != aliasConfig || canonicalState != aliasState {
		t.Fatalf("canonical aliases differ: %q %q; %q %q", canonicalConfig, canonicalState, aliasConfig, aliasState)
	}
}

func TestExplicitSymlinkedManagedStateIsRejected(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FROSTGATE_TEST_HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
	defaultDir := filepath.Join(root, "xdg", "frostgate")
	if err := os.MkdirAll(defaultDir, 0o700); err != nil {
		t.Fatal(err)
	}
	customConfig := filepath.Join(root, "custom", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(customConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(customConfig, []byte("endpoints: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, managedDir := commandPaths(customConfig)
	if err := os.Symlink(defaultDir, managedDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := validateManagedStatePath(customConfig, managedDir); err == nil {
		t.Fatal("symlinked custom managed state was accepted")
	}
	if got := profileForPaths(customConfig, managedDir); got.ID == profile.DefaultID {
		t.Fatalf("symlinked custom state was classified as default: %+v", got)
	}
}

func TestImplicitFallbackConfigKeepsDefaultProfileIdentity(t *testing.T) {
	root := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
	if err := os.WriteFile("frostgate.yaml", []byte("endpoints: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath, managedDir := commandPaths("")
	if configPath != "frostgate.yaml" {
		t.Fatalf("implicit fallback config = %q", configPath)
	}
	if got := profileForPaths(configPath, managedDir); got.ID != profile.DefaultID {
		t.Fatalf("implicit fallback must keep default profile identity: %+v", got)
	}
}

func TestAppendSingBoxEndpointsAllocatesStablePriorities(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		tags []string
		want []string
	}{
		{
			name: "empty proxy set",
			raw: []byte(`endpoints:
  - name: direct
    type: http
    url: https://example.com
    priority: 100
`),
			tags: []string{"first"},
			want: []string{"outbound: first\n      priority: 10"},
		},
		{
			name: "existing routes and gaps",
			raw: []byte(`endpoints:
  - name: direct
    type: http
    url: https://example.com
    priority: 100
  - name: old-a
    type: singbox
    outbound: old-a
    priority: 10
  - name: old-b
    type: singbox
    outbound: old-b
    priority: 35
`),
			tags: []string{"new-a", "new-b"},
			want: []string{"outbound: old-a\n      priority: 10", "outbound: old-b\n      priority: 35", "outbound: new-a\n      priority: 40", "outbound: new-b\n      priority: 50"},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			data, err := appendSingBoxEndpoints(tt.raw, tt.tags)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tt.want {
				if !bytes.Contains(data, []byte(want)) {
					t.Fatalf("priority output missing %q:\n%s", want, data)
				}
			}
		})
	}
}
