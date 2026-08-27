package setup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type setupSnapshot struct {
	path   string
	data   []byte
	mode   os.FileMode
	exists bool
}

type setupServiceState struct {
	present bool
	owned   bool
	enabled bool
	active  bool
}

// setupTransaction protects the historical default setup flow. It snapshots
// every Frostgate-owned file and the matching user unit before a reconfigure
// or reset, allowing any candidate/activation failure to restore the old
// usable setup automatically. Backups made by the caller remain on disk.
type setupTransaction struct {
	dir     string
	files   []setupSnapshot
	unit    setupSnapshot
	service string
	runner  SystemdRunner
	state   setupServiceState
	// candidateServiceInstalled is set only after the newly written unit has
	// been reloaded, enabled, restarted, and verified active.
	candidateServiceInstalled bool
	// candidateManaged distinguishes a Frostgate-owned local sidecar from an
	// external/manual controller configuration.
	candidateManaged         bool
	preserveControllerSecret bool
}

var systemdRunnerFactory = func() SystemdRunner { return execSystemdRunner{} }

func beginSetupTransaction(dir string) (*setupTransaction, error) {
	t := &setupTransaction{
		dir:     dir,
		service: "frostgate-singbox.service",
		runner:  systemdRunnerFactory(),
	}
	for _, name := range []string{"config.yaml", "sing-box.json", "controller-secret", "cache.db", "sing-box.log"} {
		snapshot, err := snapshotSetupPath(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("snapshot existing Frostgate file: %w", err)
		}
		t.files = append(t.files, snapshot)
	}
	unitPath := filepath.Join(filepath.Dir(dir), "systemd", "user", t.service)
	unit, err := snapshotSetupPath(unitPath)
	if err != nil {
		return nil, fmt.Errorf("snapshot Frostgate systemd unit: %w", err)
	}
	t.unit = unit
	if !unit.exists {
		return t, nil
	}
	t.state.present = true
	t.state.owned = unitOwnedByFrostgate(string(unit.data), filepath.Join(dir, "sing-box.json"))
	if !t.state.owned {
		return t, nil
	}
	if t.runner == nil {
		return nil, fmt.Errorf("systemd runner unavailable")
	}
	enabled, err := systemdEnabled(t.runner, t.service)
	if err != nil {
		return nil, err
	}
	active, err := systemdActive(t.runner, t.service)
	if err != nil {
		return nil, err
	}
	t.state.enabled, t.state.active = enabled, active
	return t, nil
}

func snapshotSetupPath(path string) (setupSnapshot, error) {
	snapshot := setupSnapshot{path: path, mode: 0o600}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return snapshot, nil
	}
	if err != nil {
		return snapshot, err
	}
	if !info.Mode().IsRegular() {
		return snapshot, fmt.Errorf("refusing non-regular path %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return snapshot, err
	}
	snapshot.exists, snapshot.data, snapshot.mode = true, data, info.Mode().Perm()
	return snapshot, nil
}

func unitOwnedByFrostgate(unit, configPath string) bool {
	if !strings.Contains(unit, "Description=Frostgate sing-box sidecar") {
		return false
	}
	want := "-c " + systemdQuote(configPath)
	for _, line := range strings.Split(unit, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "ExecStart=") && strings.Contains(line, " run ") && strings.Contains(line, want) {
			return true
		}
	}
	return false
}

// IsOwnedSystemdUnit is the shared ownership predicate for callers that must
// inspect a Frostgate unit without touching unrelated services.
func IsOwnedSystemdUnit(unit, configPath string) bool {
	return unitOwnedByFrostgate(unit, configPath)
}

func systemdEnabled(runner SystemdRunner, service string) (bool, error) {
	out, err := runner.Output("systemctl", "--user", "is-enabled", service)
	state := strings.TrimSpace(string(out))
	switch state {
	case "enabled", "enabled-runtime", "linked", "linked-runtime", "alias":
		return true, nil
	case "disabled", "masked", "not-found":
		return false, nil
	default:
		if err != nil {
			return false, fmt.Errorf("inspect Frostgate service enablement: %w", err)
		}
		return false, fmt.Errorf("inspect Frostgate service enablement: unexpected %q", state)
	}
}

func systemdActive(runner SystemdRunner, service string) (bool, error) {
	out, err := runner.Output("systemctl", "--user", "is-active", service)
	state := strings.TrimSpace(string(out))
	switch state {
	case "active", "activating":
		return true, nil
	case "inactive", "failed", "deactivating", "maintenance", "unknown", "not-found":
		return false, nil
	default:
		if err != nil {
			return false, fmt.Errorf("inspect Frostgate service activity: %w", err)
		}
		return false, fmt.Errorf("inspect Frostgate service activity: unexpected %q", state)
	}
}

func (t *setupTransaction) serviceInstallationAllowed() error {
	if t == nil || !t.unit.exists || t.state.owned {
		return nil
	}
	return fmt.Errorf("existing systemd unit does not prove Frostgate ownership")
}

func (t *setupTransaction) finish(candidateHasProxy bool) error {
	if t == nil {
		return nil
	}
	if !candidateHasProxy || !t.candidateManaged {
		return t.removeOwnedSidecar(true)
	}
	// A user who declines a service during reconfigure selected the documented
	// manual-run model. Do not leave the old managed process serving an
	// obsolete sing-box config in the background.
	if t.state.owned && !t.candidateServiceInstalled {
		return t.removeOwnedSidecar(false)
	}
	return nil
}

func (t *setupTransaction) removeOwnedSidecar(removeState bool) error {
	if t.unit.exists && !t.state.owned {
		return fmt.Errorf("existing systemd unit does not prove Frostgate ownership; refusing sidecar cleanup")
	}
	if t.unit.exists && t.state.owned {
		if err := t.runner.Run("systemctl", "--user", "disable", "--now", t.service); err != nil {
			return fmt.Errorf("stop Frostgate sidecar: %w", err)
		}
		if err := os.Remove(t.unit.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove Frostgate systemd unit: %w", err)
		}
		if err := t.runner.Run("systemctl", "--user", "daemon-reload"); err != nil {
			return fmt.Errorf("reload systemd after removing Frostgate sidecar: %w", err)
		}
		active, err := systemdActive(t.runner, t.service)
		if err != nil {
			return err
		}
		if active {
			return fmt.Errorf("Frostgate sidecar remains active after direct-only conversion")
		}
	}
	if removeState {
		for _, snapshot := range t.files {
			base := filepath.Base(snapshot.path)
			if base == "config.yaml" || (base == "controller-secret" && t.preserveControllerSecret) {
				continue
			}
			if err := os.Remove(snapshot.path); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove obsolete Frostgate sidecar file: %w", err)
			}
		}
	}
	return nil
}

func (t *setupTransaction) rollback(cause error) error {
	if t == nil {
		return cause
	}
	var recovery error
	// Stop a possibly activated Frostgate candidate before restoring its old
	// unit/files. A pre-existing unit that we do not own must never be touched.
	candidateUnit, unitErr := t.currentUnitIsCandidate()
	if unitErr != nil {
		recovery = errors.Join(recovery, unitErr)
	}
	if t.runner != nil && candidateUnit {
		if _, err := os.Stat(t.unit.path); err == nil {
			if err := t.runner.Run("systemctl", "--user", "disable", "--now", t.service); err != nil {
				recovery = errors.Join(recovery, fmt.Errorf("quiesce candidate sidecar: %w", err))
			}
		} else if !os.IsNotExist(err) {
			recovery = errors.Join(recovery, fmt.Errorf("inspect candidate sidecar: %w", err))
		}
	}
	for _, snapshot := range t.files {
		if err := restoreSetupSnapshot(snapshot); err != nil {
			recovery = errors.Join(recovery, fmt.Errorf("restore Frostgate file: %w", err))
		}
	}
	if err := restoreSetupSnapshot(t.unit); err != nil {
		recovery = errors.Join(recovery, fmt.Errorf("restore Frostgate systemd unit: %w", err))
	}
	if t.runner != nil && (t.state.owned || candidateUnit) {
		if err := t.runner.Run("systemctl", "--user", "daemon-reload"); err != nil {
			recovery = errors.Join(recovery, fmt.Errorf("reload restored systemd unit: %w", err))
		}
		if t.state.owned {
			if err := restoreServiceState(t.runner, t.service, t.state); err != nil {
				recovery = errors.Join(recovery, err)
			}
		}
	}
	if recovery != nil {
		return errors.Join(cause, recovery)
	}
	return cause
}

// currentUnitIsCandidate reports whether the current unit is one Frostgate
// may safely stop. The only valid candidates are a replacement for a unit we
// already owned or a newly created unit where none existed before.
func (t *setupTransaction) currentUnitIsCandidate() (bool, error) {
	current, err := snapshotSetupPath(t.unit.path)
	if os.IsNotExist(err) || !current.exists {
		return false, err
	}
	if err != nil {
		return false, fmt.Errorf("inspect candidate sidecar: %w", err)
	}
	if !unitOwnedByFrostgate(string(current.data), filepath.Join(t.dir, "sing-box.json")) {
		return false, nil
	}
	return t.state.owned || !t.unit.exists, nil
}

func restoreSetupSnapshot(snapshot setupSnapshot) error {
	if snapshot.exists {
		if err := os.MkdirAll(filepath.Dir(snapshot.path), 0o700); err != nil {
			return err
		}
		return writeFileAtomic(snapshot.path, snapshot.data, snapshot.mode)
	}
	if err := os.Remove(snapshot.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func restoreServiceState(runner SystemdRunner, service string, state setupServiceState) error {
	if state.enabled {
		if state.active {
			if err := runner.Run("systemctl", "--user", "enable", "--now", service); err != nil {
				return fmt.Errorf("restore Frostgate service enabled/active state: %w", err)
			}
			return nil
		}
		if err := runner.Run("systemctl", "--user", "enable", service); err != nil {
			return fmt.Errorf("restore Frostgate service enablement: %w", err)
		}
		return nil
	}
	if state.active {
		if err := runner.Run("systemctl", "--user", "start", service); err != nil {
			return fmt.Errorf("restore Frostgate service active state: %w", err)
		}
	}
	if err := runner.Run("systemctl", "--user", "disable", service); err != nil {
		return fmt.Errorf("restore Frostgate service disabled state: %w", err)
	}
	return nil
}
