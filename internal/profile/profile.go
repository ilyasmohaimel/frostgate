// Package profile derives non-secret identities for managed Frostgate profiles.
package profile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

const DefaultID = "default"

type Identity struct {
	ID             string
	Service        string
	ControllerAddr string
	BenchmarkAddr  string
}

// StateDir is the private managed-state directory for an explicit config.
// State is deliberately keyed by the canonical config file, rather than its
// parent directory, so sibling configs never share a sidecar, secret, or
// rollback target. The default profile retains its historical XDG directory
// and does not use this function.
func StateDir(configPath string) string {
	canonical := CanonicalPath(configPath)
	return filepath.Join(filepath.Dir(canonical), ".frostgate-"+ForPath(canonical, false).ID)
}

// CanonicalPath returns a stable profile path. Existing symlinks resolve to
// their target so aliases cannot create duplicate managed services.
func CanonicalPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	// The config file may not exist yet (for example, on the first `add
	// --config` invocation). Resolve the longest existing parent so a
	// symlinked directory cannot change the profile identity after creation.
	// If that parent symlink is dangling, EvalSymlinks cannot resolve it, so
	// resolve its link text explicitly and retain the not-yet-existing suffix.
	return canonicalMissingPath(abs, make(map[string]bool))
}

func canonicalMissingPath(path string, seen map[string]bool) string {
	path = filepath.Clean(path)
	if seen[path] {
		return path
	}
	seen[path] = true

	if info, err := filepath.EvalSymlinks(path); err == nil {
		return info
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		target, err := filepath.EvalSymlinks(path)
		if err == nil {
			return target
		}
		link, err := os.Readlink(path)
		if err != nil {
			return path
		}
		if !filepath.IsAbs(link) {
			link = filepath.Join(filepath.Dir(path), link)
		}
		return canonicalMissingPath(link, seen)
	}

	missing := make([]string, 0, 2)
	for current := path; ; {
		parent := filepath.Dir(current)
		if parent == current {
			return path
		}
		missing = append(missing, filepath.Base(current))
		current = parent
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved
		}
		if info, err := os.Lstat(current); err == nil && info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(current)
			if err != nil {
				return path
			}
			if !filepath.IsAbs(link) {
				link = filepath.Join(filepath.Dir(current), link)
			}
			resolved := canonicalMissingPath(link, seen)
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved
		}
	}
}

// ForPath returns stable, profile-specific loopback bindings. The default
// profile retains Frostgate's historical unit and ports for compatibility.
func ForPath(configPath string, isDefault bool) Identity {
	if isDefault {
		return Identity{
			ID:             DefaultID,
			Service:        "frostgate-singbox.service",
			ControllerAddr: "127.0.0.1:9090",
			BenchmarkAddr:  "127.0.0.1:2080",
		}
	}
	abs := CanonicalPath(configPath)
	sum := sha256.Sum256([]byte(abs))
	// Keep enough of the canonical-file digest in the identity that distinct
	// explicit profiles do not share a service or state directory. The former
	// eight-byte identity was adequate only while each parent directory could
	// own one profile.
	id := hex.EncodeToString(sum[:16])
	// Linux reserves 127.0.0.0/8 for loopback. A per-profile address keeps
	// simultaneously running profiles separate without relying on a small,
	// collision-prone port range. Avoid the default profile's 127.0.0.1.
	host := fmt.Sprintf("127.%d.%d.%d", int(sum[16])%254+1, int(sum[17])%254+1, int(sum[18])%254+1)
	controllerPort := 1024 + int(uint16(sum[19])<<8|uint16(sum[20]))%50000
	benchmarkPort := 1024 + int(uint16(sum[21])<<8|uint16(sum[22]))%50000
	if benchmarkPort == controllerPort {
		benchmarkPort = 51024 + (benchmarkPort % 14512)
	}
	return Identity{
		ID:             id,
		Service:        fmt.Sprintf("frostgate-singbox-%s.service", id),
		ControllerAddr: net.JoinHostPort(host, fmt.Sprintf("%d", controllerPort)),
		BenchmarkAddr:  net.JoinHostPort(host, fmt.Sprintf("%d", benchmarkPort)),
	}
}

// LegacyIdentity returns the custom-profile service and bindings used before
// v0.4.9. It is intentionally retained only to identify an on-disk legacy
// sidecar during a conservative migration; new state must always use ForPath.
func LegacyIdentity(configPath string) Identity {
	abs := CanonicalPath(configPath)
	sum := sha256.Sum256([]byte(abs))
	id := hex.EncodeToString(sum[:8])
	host := fmt.Sprintf("127.%d.%d.%d", int(sum[8])%254+1, int(sum[9])%254+1, int(sum[10])%254+1)
	return Identity{
		ID:             id,
		Service:        fmt.Sprintf("frostgate-singbox-%s.service", id),
		ControllerAddr: net.JoinHostPort(host, "9090"),
		BenchmarkAddr:  net.JoinHostPort(host, "2080"),
	}
}

// CheckAvailable verifies both profile-owned loopback bindings can be claimed
// before a new or migrated sidecar is written. It does not reserve the ports;
// systemd owns that final handoff.
func (i Identity) CheckAvailable() error {
	for _, addr := range []string{i.ControllerAddr, i.BenchmarkAddr} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("profile listener %s is unavailable: %w", addr, err)
		}
		if err := ln.Close(); err != nil {
			return err
		}
	}
	return nil
}
