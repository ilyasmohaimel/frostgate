package main

import (
	"os"
	"path/filepath"

	"github.com/ilyasmohaimel/frostgate/internal/config"
	"github.com/ilyasmohaimel/frostgate/internal/setup"
)

// commandPaths resolves a Frostgate config and its associated managed sidecar
// directory. An explicit config is a self-contained profile: its sing-box
// state and controller secret live beside that config, never in the default
// XDG profile.
func commandPaths(explicitConfig string) (configPath, managedDir string) {
	if explicitConfig != "" {
		configPath = config.ResolveConfigPath(explicitConfig)
		if abs, err := filepath.Abs(configPath); err == nil {
			configPath = abs
		}
		return configPath, filepath.Dir(configPath)
	}
	home := setup.DefaultHomeDir()
	if h := os.Getenv("FROSTGATE_TEST_HOME"); h != "" {
		home = h
	}
	managedDir = setup.ConfigDir(home)
	configPath = config.ResolveConfigPath("")
	if _, err := os.Stat(filepath.Join(managedDir, "config.yaml")); err == nil {
		configPath = filepath.Join(managedDir, "config.yaml")
	}
	return configPath, managedDir
}
