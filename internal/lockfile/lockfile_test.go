package lockfile

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHelperProcess(t *testing.T) {
	if os.Getenv("FROSTGATE_LOCK_HELPER") != "1" {
		return
	}
	event := os.Getenv("FROSTGATE_LOCK_EVENT")
	key := os.Getenv("FROSTGATE_LOCK_KEY")
	_ = os.WriteFile(event+".attempted", []byte("1"), 0o600)
	lock, err := Acquire(context.Background(), key)
	if err != nil {
		_ = os.WriteFile(event+".failed", []byte(err.Error()), 0o600)
		os.Exit(2)
	}
	_ = os.WriteFile(event+".acquired", []byte("1"), 0o600)
	for {
		if _, err := os.Stat(event + ".release"); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = lock.Close()
	os.Exit(0)
}

func TestAcquireCoordinatesAcrossProcesses(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	key := "profile:/tmp/profile/config.yaml"
	parent, err := Acquire(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	event := filepath.Join(t.TempDir(), "events")
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess", "--")
	cmd.Env = append(os.Environ(),
		"FROSTGATE_LOCK_HELPER=1",
		"FROSTGATE_LOCK_EVENT="+event,
		"FROSTGATE_LOCK_KEY="+key,
	)
	if err := cmd.Start(); err != nil {
		parent.Close()
		t.Fatal(err)
	}
	waitForFile(t, event+".attempted")
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(event + ".acquired"); err == nil {
		t.Fatal("child acquired an advisory lock while parent held it")
	}
	if err := os.WriteFile(event+".release", []byte("1"), 0o600); err != nil {
		parent.Close()
		t.Fatal(err)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("lock helper: %v", err)
	}
	if _, err := os.Stat(event + ".acquired"); err != nil {
		t.Fatalf("child did not acquire after release: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runtimeDir, "frostgate", "locks")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(runtimeDir, "frostgate", "locks"))
	if err != nil || len(entries) != 1 || strings.Contains(entries[0].Name(), "profile") {
		t.Fatalf("lock directory exposed key: entries=%v err=%v", entries, err)
	}
	info, err := os.Stat(filepath.Join(runtimeDir, "frostgate", "locks", entries[0].Name()))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("lock permissions = %o, want 600 (err=%v)", info.Mode().Perm(), err)
	}
}

func TestAcquireRejectsHostileLockDirectorySymlink(t *testing.T) {
	runtimeDir := t.TempDir()
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(runtimeDir, "frostgate")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	if _, err := Acquire(context.Background(), "secret-profile-key"); err == nil {
		t.Fatal("lock acquisition followed a hostile lock directory symlink")
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("hostile symlink target was modified: %v", entries)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}
