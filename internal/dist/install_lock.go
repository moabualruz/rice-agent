package dist

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Hold the same lock through validation, commit and rollback. Otherwise a failed
// install can roll back another install's successful binary, links or receipt.
func lockInstallation(stateDir string) (func(), error) {
	return lockInstallFile(filepath.Join(stateDir, "install.lock"))
}

func lockInstallFile(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("install lock is not a regular file")
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("installation is busy: %w", err)
	}
	return func() { _ = f.Close() }, nil
}
