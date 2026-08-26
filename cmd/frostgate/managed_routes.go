package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ilyasmohaimel/frostgate/internal/config"
	"github.com/ilyasmohaimel/frostgate/internal/managedconfig"
	"github.com/ilyasmohaimel/frostgate/internal/profile"
	"github.com/ilyasmohaimel/frostgate/internal/route"
	"github.com/ilyasmohaimel/frostgate/internal/setup"
)

var (
	findSingBox      = exec.LookPath
	writeSystemdUnit = setup.WriteSystemdUnit
	newSidecarRunner = managedconfig.DefaultSidecarRunnerForService
	runUserSystemctl = func(args ...string) error {
		return exec.Command("systemctl", append([]string{"--user"}, args...)...).Run()
	}
	runUserSystemctlOutput = func(args ...string) ([]byte, error) {
		return exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	}
)

const migrationOwnerFile = ".frostgate-migration-owner"

type managedRouteResult struct {
	Tag           string
	AlreadyExists bool
	ConfigPath    string
}

// prepareManagedState adopts an unambiguous pre-v0.4.9 custom sidecar into
// the config-file-specific state directory. Previous releases used the parent
// directory as the profile key. Copying rather than moving preserves the old
// state until the new profile has completed its own transaction; ambiguous
// sibling configurations are rejected instead of guessing ownership.
func prepareManagedState(configPath, managedDir string) error {
	_, finalize, err := prepareManagedStateWithRollback(configPath, managedDir)
	if err != nil {
		return err
	}
	return finalize()
}

// prepareManagedStateWithRollback returns rollback and finalize actions only
// when it created a new profile state directory by adopting legacy state.
// Callers use them around the first mutation so a later validation or
// lifecycle failure cannot leave a half-migrated profile behind, and a
// successful activation does not leave an in-progress marker behind.
func prepareManagedStateWithRollback(configPath, managedDir string) (func() error, func() error, error) {
	noRollback := func() error { return nil }
	noFinalize := func() error { return nil }
	if managedDir != profile.StateDir(configPath) {
		return noRollback, noFinalize, nil // historical default profile
	}
	if info, err := os.Lstat(managedDir); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, nil, fmt.Errorf("profile state %q is not a private directory", managedDir)
		}
		inProgress, markerErr := migrationMarkerPresent(managedDir)
		if markerErr != nil {
			return nil, nil, markerErr
		}
		if inProgress {
			return nil, nil, fmt.Errorf("profile state %q has an in-progress migration; refusing concurrent profile operation", managedDir)
		}
		return noRollback, noFinalize, nil
	} else if !os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("inspect profile state: %w", err)
	}
	legacyDir := filepath.Dir(configPath)
	legacyConfig := filepath.Join(legacyDir, "sing-box.json")
	if _, err := os.Stat(legacyConfig); err != nil {
		if os.IsNotExist(err) {
			return noRollback, noFinalize, nil
		}
		return nil, nil, fmt.Errorf("inspect legacy managed config: %w", err)
	}
	entries, err := os.ReadDir(legacyDir)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect legacy profile directory: %w", err)
	}
	canonicalConfig := profile.CanonicalPath(configPath)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		candidate := profile.CanonicalPath(filepath.Join(legacyDir, entry.Name()))
		if candidate != canonicalConfig {
			return nil, nil, fmt.Errorf("legacy managed state is ambiguous with sibling config %q; move it to %q only after verifying ownership", entry.Name(), managedDir)
		}
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, nil, fmt.Errorf("validate config before legacy state migration: %w", err)
	}
	legacy, err := managedconfig.Load(legacyDir)
	if err != nil {
		return nil, nil, fmt.Errorf("load legacy managed config: %w", err)
	}
	legacyIdentity := profile.LegacyIdentity(configPath)
	if !legacyStateMatchesConfig(legacy, cfg, legacyIdentity) {
		return nil, nil, fmt.Errorf("legacy managed state ownership cannot be proven; refusing to adopt it into %q", managedDir)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(managedDir), ".frostgate-migrate-*")
	if err != nil {
		return nil, nil, fmt.Errorf("create migration state: %w", err)
	}
	failMigration := func(cause error) (func() error, error) {
		if cleanupErr := os.RemoveAll(tmp); cleanupErr != nil {
			return nil, errors.Join(cause, fmt.Errorf("remove incomplete migration state: %w", cleanupErr))
		}
		return nil, cause
	}
	if err := os.Chmod(tmp, 0o700); err != nil {
		_, err := failMigration(fmt.Errorf("secure migration state: %w", err))
		return nil, nil, err
	}
	owner, err := createMigrationOwner(tmp)
	if err != nil {
		_, err := failMigration(fmt.Errorf("mark migrated state ownership: %w", err))
		return nil, nil, err
	}
	for _, name := range []string{"sing-box.json", "controller-secret", "cache.db", "sing-box.log"} {
		if err := copyLegacyStateFile(filepath.Join(legacyDir, name), filepath.Join(tmp, name)); err != nil {
			_, err := failMigration(err)
			return nil, nil, err
		}
	}
	if err := os.Rename(tmp, managedDir); err != nil {
		_, err := failMigration(fmt.Errorf("activate migrated profile state: %w", err))
		return nil, nil, err
	}
	legacyService, err := quiesceLegacyService(legacyDir, legacyIdentity)
	if err != nil {
		if legacyService != nil {
			if restoreErr := restoreLegacyService(*legacyService); restoreErr != nil {
				err = errors.Join(err, restoreErr)
			}
		}
		_, cleanupErr := failMigrationWithState(managedDir, owner, err)
		return nil, nil, cleanupErr
	}
	return func() error {
			if legacyService != nil {
				if err := restoreLegacyService(*legacyService); err != nil {
					return err
				}
			}
			newService := setup.ServiceUnitPath(profile.ForPath(configPath, false).Service)
			if _, err := os.Stat(newService); err == nil {
				return fmt.Errorf("refuse to remove migrated state while profile service %q remains installed", newService)
			} else if !os.IsNotExist(err) {
				return fmt.Errorf("inspect profile service before migration rollback: %w", err)
			}
			if err := removeOwnedMigrationState(managedDir, owner); err != nil {
				return fmt.Errorf("rollback migrated profile state: %w", err)
			}
			return nil
		}, func() error {
			return finalizeMigrationState(managedDir, owner)
		}, nil
}

func createMigrationOwner(dir string) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	owner := hex.EncodeToString(raw)
	if err := os.WriteFile(filepath.Join(dir, migrationOwnerFile), []byte(owner+"\n"), 0o600); err != nil {
		return "", err
	}
	return owner, nil
}

func migrationMarkerPresent(dir string) (bool, error) {
	marker := filepath.Join(dir, migrationOwnerFile)
	info, err := os.Lstat(marker)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect migration ownership marker: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, fmt.Errorf("migration ownership marker %q is not a regular file", marker)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		return false, fmt.Errorf("read migration ownership marker: %w", err)
	}
	if strings.TrimSpace(string(data)) == "" {
		return false, fmt.Errorf("migration ownership marker %q is empty", marker)
	}
	return true, nil
}

func migrationOwnerMatches(dir, owner string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect migrated profile state: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("migrated profile state %q is not a private directory", dir)
	}
	marker := filepath.Join(dir, migrationOwnerFile)
	info, err = os.Lstat(marker)
	if err != nil {
		return fmt.Errorf("inspect migration ownership marker: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("migration ownership marker %q is not a regular file", marker)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		return fmt.Errorf("read migration ownership marker: %w", err)
	}
	if strings.TrimSpace(string(data)) != owner {
		return fmt.Errorf("migration ownership marker does not match this transaction")
	}
	return nil
}

func finalizeMigrationState(dir, owner string) error {
	if err := migrationOwnerMatches(dir, owner); err != nil {
		return fmt.Errorf("finalize migrated profile state: %w", err)
	}
	if err := os.Remove(filepath.Join(dir, migrationOwnerFile)); err != nil {
		return fmt.Errorf("remove migration ownership marker: %w", err)
	}
	return nil
}

func removeOwnedMigrationState(dir, owner string) error {
	if err := migrationOwnerMatches(dir, owner); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return nil
}

func failMigrationWithState(managedDir, owner string, cause error) (func() error, error) {
	if cleanupErr := removeOwnedMigrationState(managedDir, owner); cleanupErr != nil {
		return nil, errors.Join(cause, fmt.Errorf("remove incomplete migration state: %w", cleanupErr))
	}
	return nil, cause
}

type legacyServiceState struct {
	Name              string
	Enabled           bool
	EnablementManaged bool
	Active            bool
}

func quiesceLegacyService(legacyDir string, identity profile.Identity) (*legacyServiceState, error) {
	path := setup.ServiceUnitPath(identity.Service)
	unit, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read legacy service unit: %w", err)
	}
	legacyConfig := filepath.Join(legacyDir, "sing-box.json")
	if !unitExecStartMatches(string(unit), legacyConfig) {
		return nil, fmt.Errorf("legacy service unit %q does not prove ownership of %q", identity.Service, legacyConfig)
	}
	enabled, enablementManaged, err := legacySystemdEnabled(identity.Service)
	if err != nil {
		return nil, err
	}
	active, err := legacySystemdActive(identity.Service)
	if err != nil {
		return nil, err
	}
	if err := runUserSystemctl("disable", "--now", identity.Service); err != nil {
		return &legacyServiceState{Name: identity.Service, Enabled: enabled, EnablementManaged: enablementManaged, Active: active}, fmt.Errorf("quiesce legacy service %s: %w", identity.Service, err)
	}
	return &legacyServiceState{Name: identity.Service, Enabled: enabled, EnablementManaged: enablementManaged, Active: active}, nil
}

func legacySystemdEnabled(service string) (bool, bool, error) {
	out, err := runUserSystemctlOutput("is-enabled", service)
	state := strings.TrimSpace(string(out))
	switch state {
	case "enabled", "enabled-runtime", "linked", "linked-runtime", "alias":
		return true, true, nil
	case "disabled", "masked", "not-found":
		return false, state == "disabled", nil
	case "static", "indirect", "generated", "transient":
		return false, false, nil
	default:
		if err != nil {
			return false, false, fmt.Errorf("inspect legacy service %s enabled state: %w", service, err)
		}
		return false, false, fmt.Errorf("inspect legacy service %s enabled state: unexpected %q", service, state)
	}
}

func legacySystemdActive(service string) (bool, error) {
	out, err := runUserSystemctlOutput("is-active", service)
	state := strings.TrimSpace(string(out))
	switch state {
	case "active", "activating":
		return true, nil
	case "inactive", "failed", "deactivating", "maintenance", "unknown":
		return false, nil
	default:
		if err != nil {
			return false, fmt.Errorf("inspect legacy service %s active state: %w", service, err)
		}
		return false, fmt.Errorf("inspect legacy service %s active state: unexpected %q", service, state)
	}
}

func restoreLegacyService(state legacyServiceState) error {
	if !state.EnablementManaged {
		if state.Active {
			if err := runUserSystemctl("start", state.Name); err != nil {
				return fmt.Errorf("restore legacy service %s: %w", state.Name, err)
			}
		}
		return nil
	}
	if state.Enabled {
		if state.Active {
			if err := runUserSystemctl("enable", "--now", state.Name); err != nil {
				return fmt.Errorf("restore legacy service %s: %w", state.Name, err)
			}
			return nil
		}
		if err := runUserSystemctl("enable", state.Name); err != nil {
			return fmt.Errorf("restore legacy service enablement %s: %w", state.Name, err)
		}
		return nil
	}
	if state.Active {
		if err := runUserSystemctl("start", state.Name); err != nil {
			return fmt.Errorf("restore legacy service %s: %w", state.Name, err)
		}
	}
	if err := runUserSystemctl("disable", state.Name); err != nil {
		return fmt.Errorf("restore legacy service disablement %s: %w", state.Name, err)
	}
	return nil
}

func unitExecStartMatches(unit, configPath string) bool {
	wantConfig := `-c ` + systemdQuoted(configPath)
	for _, line := range strings.Split(unit, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ExecStart=") {
			continue
		}
		command := strings.TrimPrefix(line, "ExecStart=")
		if strings.Contains(command, " run ") && strings.Contains(command, wantConfig) {
			return true
		}
	}
	return false
}

func systemdQuoted(value string) string {
	value = strings.ReplaceAll(value, "%", "%%")
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return `"` + value + `"`
}

func legacyStateMatchesConfig(mg *managedconfig.Managed, cfg *config.Config, legacy profile.Identity) bool {
	expected := map[string]bool{}
	for _, endpoint := range cfg.Endpoints {
		if endpoint.Type == config.ProbeSingBox {
			expected[endpoint.Outbound] = true
		}
	}
	if len(expected) == 0 || !mg.HasManagedSidecar() || len(expected) != len(mg.VLESS)+len(mg.Hysteria) {
		return false
	}
	for _, route := range mg.VLESS {
		if route == nil || !expected[route.Tag] {
			return false
		}
	}
	for _, route := range mg.Hysteria {
		if route == nil || !expected[route.Tag] {
			return false
		}
	}
	controller, err := url.Parse(cfg.SingBox.Controller)
	if err != nil || controller.Host != legacy.ControllerAddr || (cfg.SingBox.BenchmarkProxy != "" && cfg.SingBox.BenchmarkProxy != legacy.BenchmarkAddr) || cfg.SingBox.Secret == "" || cfg.SingBox.Secret != mg.Secret {
		return false
	}
	return mg.UsesProfileBindings(cfg.SingBox.Controller, legacy.BenchmarkAddr)
}

func copyLegacyStateFile(src, dst string) error {
	in, err := os.Open(src)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read legacy state %s: %w", filepath.Base(src), err)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		closeErr := in.Close()
		return errors.Join(fmt.Errorf("create migrated state %s: %w", filepath.Base(dst), err), closeErr)
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	closeInErr := in.Close()
	if copyErr != nil {
		return errors.Join(fmt.Errorf("copy legacy state %s: %w", filepath.Base(src), copyErr), closeInErr)
	}
	if closeErr != nil {
		return errors.Join(fmt.Errorf("close migrated state %s: %w", filepath.Base(dst), closeErr), closeInErr)
	}
	if closeInErr != nil {
		return fmt.Errorf("close legacy state %s: %w", filepath.Base(src), closeInErr)
	}
	return nil
}

// addManagedRoute is the single route-add path used by the CLI and setup. It
// parses with route.ParseShareLink, updates the managed sing-box model, and
// installs sing-box.json and config.yaml in one transaction.
func addManagedRoute(dir, configPath, link, tag string, runner *managedconfig.SidecarRunner) (managedRouteResult, error) {
	if err := route.ValidateName(tag); err != nil {
		return managedRouteResult{}, fmt.Errorf("invalid route name %q: %w", tag, err)
	}
	r, err := route.ParseShareLink(link, tag)
	if err != nil {
		return managedRouteResult{}, err
	}
	mg, err := managedconfig.Load(dir)
	if err != nil {
		return managedRouteResult{}, fmt.Errorf("load managed config: %w", err)
	}
	for _, v := range mg.VLESS {
		if v.Tag == tag || route.FingerprintVLESS(v) == r.Fingerprint {
			return managedRouteResult{Tag: v.Tag, AlreadyExists: true, ConfigPath: configPath}, nil
		}
	}
	for _, h := range mg.Hysteria {
		if h.Tag == tag || route.FingerprintHysteria(h) == r.Fingerprint {
			return managedRouteResult{Tag: h.Tag, AlreadyExists: true, ConfigPath: configPath}, nil
		}
	}
	if r.VLESS != nil {
		mg.AddVLESS(r.VLESS)
	} else {
		mg.AddHysteria(r.Hysteria)
	}

	if err := commitManagedRoutes(mg, configPath, []string{tag}, runner); err != nil {
		return managedRouteResult{}, err
	}
	return managedRouteResult{Tag: tag, ConfigPath: configPath}, nil
}

func commitManagedRoutes(mg *managedconfig.Managed, configPath string, tags []string, runner *managedconfig.SidecarRunner) error {
	identity := profileForPaths(configPath, mg.Dir)
	newSidecar := !mg.HasManagedSidecar()
	needsProfileBindings := newSidecar || (identity.ID != profile.DefaultID && !mg.UsesProfileBindings(identity.ControllerAddr, identity.BenchmarkAddr))
	if identity.ID != profile.DefaultID && needsProfileBindings {
		if err := identity.CheckAvailable(); err != nil {
			return err
		}
	}
	configData, controller, configuredSecret, err := routeConfigCandidate(configPath, mg.Dir, tags, identity, newSidecar)
	if err != nil {
		return err
	}
	if mg.Secret == "" {
		mg.Secret = configuredSecret
		if mg.Secret == "" {
			mg.Secret, err = setup.GenerateSecret()
			if err != nil {
				return err
			}
		}
	}
	// A custom profile that owns a sidecar gets unique loopback bindings. The
	// default profile keeps its long-standing ports and unit name.
	if newSidecar || identity.ID != profile.DefaultID {
		mg.ConfigureProfile(controller, identity.BenchmarkAddr)
	}
	var restoreUnit func() error
	var unitCreated bool
	if runner == nil {
		if newSidecar || identity.ID != profile.DefaultID {
			sbPath, lookErr := findSingBox("sing-box")
			if lookErr != nil {
				return fmt.Errorf("sing-box not available for managed sidecar bootstrap: %w", lookErr)
			}
			var unitErr error
			restoreUnit, unitCreated, unitErr = writeManagedUnit(setup.ServiceUnitPath(identity.Service), sbPath, mg.Dir)
			if unitErr != nil {
				return unitErr
			}
		}
		runner = newSidecarRunner(controller, mg.Secret, identity.Service)
	}
	if err := mg.SaveTransactionWithConfig(runner, configPath, configData); err != nil {
		if restoreUnit != nil {
			var recovery error
			quiesced := true
			// A newly-created unit may have been started by SaveTransaction's
			// rollback. Quiesce it before removing the registration. Existing
			// custom units are healthy pre-existing services and must stay up.
			if unitCreated {
				if runner != nil && runner.Stop != nil {
					if stopErr := runner.Stop(); stopErr != nil {
						recovery = errors.Join(recovery, fmt.Errorf("stop transient sidecar: %w", stopErr))
						quiesced = false
					}
				}
				if runner != nil && runner.Disable != nil {
					if disableErr := runner.Disable(); disableErr != nil {
						recovery = errors.Join(recovery, fmt.Errorf("disable transient sidecar: %w", disableErr))
						quiesced = false
					}
				}
			}
			if quiesced {
				if restoreErr := restoreUnit(); restoreErr != nil {
					recovery = errors.Join(recovery, fmt.Errorf("restore service unit: %w", restoreErr))
				}
				if runner != nil && runner.DaemonReload != nil {
					if reloadErr := runner.DaemonReload(); reloadErr != nil {
						recovery = errors.Join(recovery, fmt.Errorf("reload restored service unit: %w", reloadErr))
					}
				}
			}
			if recovery != nil {
				return errors.Join(err, recovery)
			}
		}
		return err
	}
	return nil
}

// writeManagedUnit snapshots a profile unit so failed first-route bootstraps
// do not leave a sidecar registered for a configuration that was rolled back.
func writeManagedUnit(path, binaryPath, configDir string) (func() error, bool, error) {
	unitMode := os.FileMode(0o644)
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, false, fmt.Errorf("snapshot service unit: refusing symlinked unit %s", path)
		}
		if !info.Mode().IsRegular() {
			return nil, false, fmt.Errorf("snapshot service unit: %s is not a regular file", path)
		}
		unitMode = info.Mode().Perm()
	} else if !os.IsNotExist(err) {
		return nil, false, fmt.Errorf("snapshot service unit: %w", err)
	}
	before, err := os.ReadFile(path)
	existed := err == nil
	if err != nil && !os.IsNotExist(err) {
		return nil, false, fmt.Errorf("snapshot service unit: %w", err)
	}
	if err := writeSystemdUnit(path, binaryPath, configDir); err != nil {
		return nil, false, err
	}
	return func() error {
		if existed {
			if err := restoreManagedUnit(path, before, unitMode); err != nil {
				return err
			}
			return nil
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}, !existed, nil
}

func restoreManagedUnit(path string, data []byte, mode os.FileMode) (err error) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to restore symlinked systemd unit %s", path)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".frostgate-unit-restore-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	clean := true
	defer func() {
		if clean {
			if removeErr := os.Remove(tmpPath); removeErr != nil && !os.IsNotExist(removeErr) {
				err = errors.Join(err, fmt.Errorf("remove temporary systemd unit restore: %w", removeErr))
			}
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	clean = false
	return nil
}

func routeConfigCandidate(path, dir string, tags []string, identity profile.Identity, bootstrap bool) ([]byte, string, string, error) {
	controller := "http://" + identity.ControllerAddr
	secretFile := filepath.Join(dir, "controller-secret")
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if err := config.ValidateEndpointCount(len(tags) + 1); err != nil {
			return nil, "", "", err
		}
		data, marshalErr := setup.MarshalFrostgateConfig(tags, nil, config.DefaultTestURL, controller, secretFile)
		if marshalErr != nil {
			return nil, "", "", marshalErr
		}
		data, marshalErr = setManagedSingBoxSettings(data, controller, secretFile, identity.BenchmarkAddr)
		if marshalErr != nil {
			return nil, "", "", marshalErr
		}
		return data, controller, "", nil
	}
	if err != nil {
		return nil, "", "", fmt.Errorf("read Frostgate config: %w", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, "", "", err
	}
	if err := config.ValidateEndpointCount(len(cfg.Endpoints) + len(tags)); err != nil {
		return nil, "", "", err
	}
	if bootstrap {
		for _, endpoint := range cfg.Endpoints {
			if endpoint.Type == config.ProbeSingBox {
				return nil, "", "", fmt.Errorf("cannot bootstrap a managed sidecar over an existing external controller")
			}
		}
	}
	data, err := appendSingBoxEndpoints(raw, tags)
	if err != nil {
		return nil, "", "", err
	}
	if bootstrap || identity.ID != profile.DefaultID {
		data, err = setManagedSingBoxSettings(data, controller, secretFile, identity.BenchmarkAddr)
		if err != nil {
			return nil, "", "", err
		}
		return data, controller, cfg.SingBox.Secret, nil
	}
	return data, cfg.SingBox.Controller, cfg.SingBox.Secret, nil
}

func setManagedSingBoxSettings(raw []byte, controller, secretFile, benchmarkAddr string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("parse Frostgate config for managed sidecar: %w", err)
	}
	root := doc.Content[0]
	singbox := mappingValue(root, "singbox")
	if singbox == nil {
		singbox = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "singbox"}, singbox)
	}
	if singbox.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("Frostgate config singbox must be a mapping")
	}
	setMappingString(singbox, "controller", controller)
	setMappingString(singbox, "secret_file", secretFile)
	setMappingString(singbox, "benchmark_proxy", benchmarkAddr)
	return yaml.Marshal(&doc)
}

func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

func setMappingString(mapping *yaml.Node, key, value string) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1].Kind = yaml.ScalarNode
			mapping.Content[i+1].Tag = "!!str"
			mapping.Content[i+1].Value = value
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
	)
}

func appendSingBoxEndpoints(raw []byte, tags []string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse Frostgate config: %w", err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("Frostgate config must be a mapping")
	}
	root := doc.Content[0]
	var endpoints *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "endpoints" {
			endpoints = root.Content[i+1]
			break
		}
	}
	if endpoints == nil {
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "endpoints"},
			&yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"},
		)
		endpoints = root.Content[len(root.Content)-1]
	}
	if endpoints.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("Frostgate config endpoints must be a list")
	}
	if err := config.ValidateEndpointCount(len(endpoints.Content) + len(tags)); err != nil {
		return nil, err
	}
	maxPriority := 0
	for _, endpoint := range endpoints.Content {
		if endpoint.Kind != yaml.MappingNode || endpointField(endpoint, "type") != string(config.ProbeSingBox) {
			continue
		}
		if priority, err := strconv.Atoi(endpointField(endpoint, "priority")); err == nil && priority > maxPriority {
			maxPriority = priority
		}
	}
	nextPriority := ((maxPriority / 10) + 1) * 10
	for _, tag := range tags {
		for _, endpoint := range endpoints.Content {
			if endpoint.Kind != yaml.MappingNode {
				continue
			}
			for i := 0; i+1 < len(endpoint.Content); i += 2 {
				if endpoint.Content[i].Value == "name" && endpoint.Content[i+1].Value == tag {
					return nil, fmt.Errorf("endpoint %q already exists", tag)
				}
			}
		}
		endpointData, err := yaml.Marshal(config.Endpoint{Name: tag, Type: config.ProbeSingBox, Outbound: tag, Priority: nextPriority})
		if err != nil {
			return nil, err
		}
		var endpoint yaml.Node
		if err := yaml.Unmarshal(endpointData, &endpoint); err != nil || len(endpoint.Content) != 1 {
			return nil, fmt.Errorf("encode endpoint: %w", err)
		}
		endpoints.Content = append(endpoints.Content, endpoint.Content[0])
		nextPriority += 10
	}
	data, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, fmt.Errorf("encode Frostgate config: %w", err)
	}
	return data, nil
}

func endpointField(endpoint *yaml.Node, key string) string {
	for i := 0; i+1 < len(endpoint.Content); i += 2 {
		if endpoint.Content[i].Value == key {
			return endpoint.Content[i+1].Value
		}
	}
	return ""
}
