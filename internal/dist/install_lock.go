package dist

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/moabualruz/agent-dry/internal/fslock"
)

// Hold the same locks through validation, commit and rollback. Otherwise a failed
// install can roll back another install's successful binary, links or receipt.
func lockInstallation(stateDir string) (func(), error) {
	return lockInstallFile(filepath.Join(stateDir, "install.lock"))
}

func validateInstallLockPath(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	parent, err := resolvedPath(filepath.Dir(abs), "")
	if err != nil {
		return err
	}
	abs = filepath.Join(parent, filepath.Base(abs))
	var missing bool
	for current := parent; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			missing = true
		} else if err != nil {
			return err
		} else if missing {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fmt.Errorf("install lock parent is not a directory: %s", current)
			}
		} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("install lock parent is not a directory: %s", current)
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	info, err := os.Lstat(abs)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("install lock is not a regular file")
	}
	return nil
}

func openInstallLock(path string, create bool) (func(), bool, error) {
	if err := validateInstallLockPath(path); err != nil {
		return nil, false, err
	}
	flags := os.O_RDWR | syscall.O_NOFOLLOW
	if create {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, false, err
		}
		flags |= os.O_CREATE
	} else if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	} else if err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return nil, create, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, true, fmt.Errorf("install lock is not a regular file")
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, true, fmt.Errorf("installation is busy: %w", err)
	}
	return func() { _ = f.Close() }, true, nil
}

func tryLockExisting(path string) (func(), bool, error) {
	return openInstallLock(path, false)
}

func lockInstallPaths(stateDir, prefix string) (func(), error) {
	stateLock := filepath.Join(stateDir, "install.lock")
	prefixLock := filepath.Join(prefix, ".agent-dry-install.lock")
	if err := validateInstallLockPath(stateLock); err != nil {
		return nil, err
	}
	if err := validateInstallLockPath(prefixLock); err != nil {
		return nil, err
	}
	unlockDirectories, err := fslock.Lock([]string{stateDir, prefix}, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("installation is busy: %w", err)
	}
	unlocks := []func(){unlockDirectories}
	for _, path := range []string{stateLock, prefixLock} {
		unlock, exists, err := tryLockExisting(path)
		if err != nil {
			for i := len(unlocks) - 1; i >= 0; i-- {
				unlocks[i]()
			}
			return nil, err
		}
		if exists {
			unlocks = append(unlocks, unlock)
		}
	}
	return func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}, nil
}

func lockInstallFile(path string) (func(), error) {
	unlock, _, err := openInstallLock(path, true)
	return unlock, err
}
