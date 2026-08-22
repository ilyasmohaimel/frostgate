// Package profile derives non-secret identities for managed Frostgate profiles.
package profile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"path/filepath"
)

const DefaultID = "default"

type Identity struct {
	ID             string
	Service        string
	ControllerAddr string
	BenchmarkAddr  string
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
	return abs
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
	id := hex.EncodeToString(sum[:8])
	// Linux reserves 127.0.0.0/8 for loopback. A per-profile address keeps
	// simultaneously running profiles separate without relying on a small,
	// collision-prone port range. Avoid the default profile's 127.0.0.1.
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
