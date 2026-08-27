// Package lockfile provides cancellable inter-process advisory locks.
package lockfile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const pollInterval = 25 * time.Millisecond

type Lock struct {
	file *os.File
}

// Acquire obtains an exclusive lock for key. The key is hashed so lock names
// never disclose paths, profile names, or credentials.
func Acquire(ctx context.Context, key string) (*Lock, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := lockRoot()
	if err != nil {
		return nil, err
	}
	if err := ensurePrivateDir(root); err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(key))
	path := filepath.Join(root, "lock-"+hex.EncodeToString(digest[:])+".lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("secure lock: %w", err)
	}
	locked := false
	defer func() {
		if !locked {
			_ = file.Close()
		}
	}()
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			locked = true
			return &Lock{file: file}, nil
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			return nil, fmt.Errorf("acquire lock: %w", err)
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

func lockRoot() (string, error) {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		var err error
		base, err = os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("locate private lock directory: %w", err)
		}
	}
	return filepath.Join(base, "frostgate", "locks"), nil
}

func ensurePrivateDir(path string) error {
	for _, dir := range []string{filepath.Dir(path), path} {
		info, err := os.Lstat(dir)
		if os.IsNotExist(err) {
			if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
				return fmt.Errorf("create private lock directory: %w", err)
			}
			info, err = os.Lstat(dir)
		}
		if err != nil {
			return fmt.Errorf("inspect private lock directory: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("private lock directory is not a directory")
		}
		if info.Mode().Perm()&0o077 != 0 {
			if err := os.Chmod(dir, 0o700); err != nil {
				return fmt.Errorf("secure private lock directory: %w", err)
			}
		}
	}
	return nil
}
