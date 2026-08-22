package setup

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ilyasmohaimel/frostgate/internal/config"
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
	if err := importFlow(Options{Stdin: bufio.NewReader(strings.NewReader(input))}, dir, ""); err != nil {
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
