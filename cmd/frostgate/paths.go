package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/ilyasmohaimel/frostgate/internal/config"
	"github.com/ilyasmohaimel/frostgate/internal/history"
	"github.com/ilyasmohaimel/frostgate/internal/profile"
	"github.com/ilyasmohaimel/frostgate/internal/setup"
)

// commandPaths resolves a Frostgate config and its associated managed sidecar
// directory. Explicit configs use a deterministic private sibling state
// directory derived from the canonical config file. This is intentionally not
// filepath.Dir(configPath): two configurations can live beside one another.
func commandPaths(explicitConfig string) (configPath, managedDir string) {
	if explicitConfig != "" {
		configPath = config.ResolveConfigPath(explicitConfig)
		configPath = profile.CanonicalPath(configPath)
		implicitConfig, defaultManagedDir := commandPaths("")
		if profile.CanonicalPath(implicitConfig) == configPath {
			return configPath, defaultManagedDir
		}
		return configPath, profile.StateDir(configPath)
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

func commandPathsChecked(explicitConfig string) (configPath, managedDir string, err error) {
	configPath, managedDir = commandPaths(explicitConfig)
	if err := validateManagedStatePath(configPath, managedDir); err != nil {
		return "", "", err
	}
	return configPath, managedDir, nil
}

func validateManagedStatePath(configPath, managedDir string) error {
	implicitConfig, defaultManagedDir := commandPaths("")
	if profile.CanonicalPath(configPath) == profile.CanonicalPath(implicitConfig) &&
		filepath.Clean(managedDir) == filepath.Clean(defaultManagedDir) {
		return nil
	}

	if filepath.Clean(managedDir) != filepath.Clean(profile.StateDir(configPath)) {
		return fmt.Errorf("managed state path is not owned by the selected profile")
	}
	if symlink, err := pathHasSymlinkComponent(managedDir); err != nil {
		return fmt.Errorf("cannot inspect managed state path: %w", err)
	} else if symlink {
		return fmt.Errorf("refusing symlinked managed state path")
	}
	if info, err := os.Lstat(managedDir); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("managed state path is not a directory")
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("cannot inspect managed state path: %w", err)
	}
	return nil
}

func pathHasSymlinkComponent(path string) (bool, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false, err
	}
	for current := filepath.Clean(abs); ; {
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				parent := filepath.Dir(current)
				if parent == current {
					return false, nil
				}
				current = parent
				continue
			}
			return false, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return true, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false, nil
		}
		current = parent
	}
}

// profileForPaths derives default-ness from the selected managed directory.
// Implicit lookup keeps its historical sidecar. Explicit configs, including
// alternates under the default config directory, have a private state dir and
// therefore always get a config-file-specific identity.
func profileForPaths(configPath, managedDir string) profile.Identity {
	implicitConfig, defaultDir := commandPaths("")
	isDefault := profile.CanonicalPath(configPath) == profile.CanonicalPath(implicitConfig) &&
		filepath.Clean(managedDir) == filepath.Clean(defaultDir)
	return profile.ForPath(configPath, isDefault)
}

func historyProfileForPaths(configPath, managedDir string) string {
	identity := profileForPaths(configPath, managedDir)
	return history.ProfileID(configPath, identity.ID == profile.DefaultID)
}

func historyProfileForConfig(configPath string) string {
	implicitConfig, _ := commandPaths("")
	if profile.CanonicalPath(configPath) == profile.CanonicalPath(implicitConfig) {
		return history.ProfileID(configPath, true)
	}
	return history.ProfileID(configPath, false)
}
