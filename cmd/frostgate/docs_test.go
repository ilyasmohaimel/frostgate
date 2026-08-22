package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadmeReferencesReproducibleDemo(t *testing.T) {
	root := filepath.Join("..", "..")
	readme, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), "assets/frostgate-demo.gif") {
		t.Fatal("README does not reference the demo")
	}
	for _, path := range []string{"assets/frostgate-demo.gif", "docs/frostgate-demo.tape", "docs/frostgate-demo-env.sh"} {
		info, err := os.Stat(filepath.Join(root, path))
		if err != nil || info.Size() == 0 {
			t.Fatalf("demo asset missing or empty: %s (%v)", path, err)
		}
	}
}
