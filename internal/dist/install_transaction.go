package dist

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Preserve previous inodes until binaries, command links and the receipt commit.
// All backups share a directory with their target, so rollback uses atomic rename.
type installBackup struct {
	path   string
	backup string
}

type installTransaction struct {
	files     []installBackup
	committed bool
}

func prepareInstall(paths []string) (*installTransaction, error) {
	tx := &installTransaction{}
	seen := map[string]bool{}
	for _, path := range paths {
		if seen[path] {
			continue
		}
		seen[path] = true
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			tx.discard()
			return nil, err
		}
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			tx.files = append(tx.files, installBackup{path: path})
			continue
		}
		if err != nil {
			tx.discard()
			return nil, err
		}
		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			tx.discard()
			return nil, fmt.Errorf("install target is not a regular file or symlink: %s", path)
		}
		tmp, err := os.CreateTemp(filepath.Dir(path), ".agent-dry-backup-*")
		if err != nil {
			tx.discard()
			return nil, err
		}
		backup := tmp.Name()
		closeErr := tmp.Close()
		removeErr := os.Remove(backup)
		if err := errors.Join(closeErr, removeErr); err != nil {
			tx.discard()
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, readErr := os.Readlink(path)
			if readErr != nil {
				tx.discard()
				return nil, readErr
			}
			err = os.Symlink(target, backup)
		} else {
			err = os.Link(path, backup)
		}
		if err != nil {
			tx.discard()
			return nil, err
		}
		tx.files = append(tx.files, installBackup{path: path, backup: backup})
	}
	return tx, nil
}

func (tx *installTransaction) discard() {
	for _, file := range tx.files {
		if file.backup != "" {
			_ = os.Remove(file.backup)
		}
	}
}

func (tx *installTransaction) finish() error {
	if tx.committed {
		tx.discard()
		return nil
	}
	var failures []error
	for i := len(tx.files) - 1; i >= 0; i-- {
		file := tx.files[i]
		if file.backup != "" {
			if err := os.Rename(file.backup, file.path); err != nil {
				failures = append(failures, fmt.Errorf("rollback %s: %w", file.path, err))
			}
		} else if err := os.Remove(file.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, fmt.Errorf("rollback %s: %w", file.path, err))
		}
	}
	return errors.Join(failures...)
}
