package main

import (
	"os"
	"path/filepath"

	"github.com/ilyasmohaimel/frostgate/internal/config"
	"github.com/ilyasmohaimel/frostgate/internal/history"
	"github.com/ilyasmohaimel/frostgate/internal/profile"
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

// profileForPaths derives identity from the managed directory, not merely the
// config filename. This keeps an implicit ./frostgate.yaml on the historical
// default sidecar while an explicit profile remains isolated beside its file.
func profileForPaths(configPath, managedDir string) profile.Identity {
	_, defaultDir := commandPaths("")
	canonicalDir := profile.CanonicalPath(managedDir)
	canonicalDefault := profile.CanonicalPath(defaultDir)
	return profile.ForPath(configPath, canonicalDir != "" && canonicalDir == canonicalDefault)
}

func historyProfileForPaths(configPath, managedDir string) string {
	identity := profileForPaths(configPath, managedDir)
	return history.ProfileID(configPath, identity.ID == profile.DefaultID)
}

func historyProfileForConfig(configPath string) string {
	return historyProfileForPaths(configPath, filepath.Dir(configPath))
}
