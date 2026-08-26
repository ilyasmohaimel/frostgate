package setup

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ilyasmohaimel/frostgate/internal/config"
	"gopkg.in/yaml.v3"
)

func TestDirectOnlyFlowUsesStatusPolicy(t *testing.T) {
	t.Setenv("PATH", "")
	dir := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	if err := directOnlyFlow(Options{Stdin: bufio.NewReader(strings.NewReader(server.URL + "\n"))}, dir); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoints[0].ExpectedStatus != 0 {
		t.Fatalf("custom direct endpoint has exact status %d", cfg.Endpoints[0].ExpectedStatus)
	}
}

func TestRunValidationPropagatesConfigFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("endpoints: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runValidation(dir); err == nil {
		t.Fatal("invalid setup config was accepted")
	}
}

func TestRunValidationSkipsStaleSingBoxForDirectOnly(t *testing.T) {
	singBoxDir := t.TempDir()
	singBox := filepath.Join(singBoxDir, "sing-box")
	if err := os.WriteFile(singBox, []byte("#!/bin/sh\nexit 42\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", singBoxDir)
	dir := t.TempDir()
	content, err := yaml.Marshal(BuildDirectOnlyConfig("https://example.com/"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sing-box.json"), []byte("malformed stale sidecar"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runValidation(dir); err != nil {
		t.Fatalf("direct-only validation failed on stale sidecar: %v", err)
	}
}

func TestExistingControllerFlowUsesDefaultGenerate204Status(t *testing.T) {
	t.Setenv("PATH", "")
	dir := t.TempDir()
	input := "\n\nproxy-route\n"
	if err := existingControllerFlow(Options{Stdin: bufio.NewReader(strings.NewReader(input))}, dir); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoints[0].ExpectedStatus != http.StatusNoContent {
		t.Fatalf("default direct endpoint expected status = %d", cfg.Endpoints[0].ExpectedStatus)
	}
}

func TestImportFlowCustomDirectUsesGenericStatus(t *testing.T) {
	t.Setenv("PATH", "")
	dir := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	input := "\n\nn\n" + server.URL + "\n"
	if err := importFlow(Options{Stdin: bufio.NewReader(strings.NewReader(input))}, dir, "", nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoints[0].ExpectedStatus != 0 {
		t.Fatalf("custom direct endpoint has exact status %d", cfg.Endpoints[0].ExpectedStatus)
	}
}

func TestBackupExistingNeverOverwritesSameTimestamp(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("new-config"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixed := time.Date(2026, time.August, 25, 12, 0, 0, 0, time.UTC)
	oldNow := backupNow
	backupNow = func() time.Time { return fixed }
	t.Cleanup(func() { backupNow = oldNow })
	first := filepath.Join(dir, "backup-"+fixed.Format("20060102T150405"))
	if err := os.Mkdir(first, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first, "config.yaml"), []byte("original-backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := backupExisting(dir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(first, "config.yaml"))
	if err != nil || string(got) != "original-backup" {
		t.Fatalf("existing backup was overwritten: %q err=%v", got, err)
	}
	newPath := filepath.Join(dir, "backup-"+fixed.Format("20060102T150405")+"-1", "config.yaml")
	got, err = os.ReadFile(newPath)
	if err != nil || string(got) != "new-config" {
		t.Fatalf("new backup missing: %q err=%v", got, err)
	}
}
