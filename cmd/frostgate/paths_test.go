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
	if resolved != customConfig || managedDir != customDir {
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
	if _, err := os.Stat(filepath.Join(customDir, "sing-box.json")); err != nil {
		t.Fatalf("custom managed sidecar missing: %v", err)
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
