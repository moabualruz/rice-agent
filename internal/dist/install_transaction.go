package dist

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/moabualruz/rice-agent/internal/fslock"
)

// Preserve previous inodes until binaries, command links and the receipt commit.
// All backups share a directory with their target, so rollback uses atomic rename.
type installBackup struct {
	path   string
	backup string
}

type installTransaction struct {
	files     []installBackup
	dirs      []string
	committed bool
}

func validateInstallTargets(paths []string) error {
	for _, path := range paths {
		if _, err := fslock.ExistingAnchor(filepath.Dir(path)); err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("install target is not a regular file or symlink: %s", path)
		}
	}
	return nil
}

func (tx *installTransaction) mkdir(path string) error {
	var missing []string
	for p := path; ; p = filepath.Dir(p) {
		info, err := os.Stat(p)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("install parent is not a directory")
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		missing = append(missing, p)
	}
	for i := len(missing) - 1; i >= 0; i-- {
		tx.dirs = append(tx.dirs, missing[i])
	}
	return os.MkdirAll(path, 0o755)
}

func prepareInstall(paths []string) (*installTransaction, error) {
	if err := validateInstallTargets(paths); err != nil {
		return nil, err
	}
	tx := &installTransaction{}
	seen := map[string]bool{}
	for _, path := range paths {
		if seen[path] {
			continue
		}
		seen[path] = true
		if err := tx.mkdir(filepath.Dir(path)); err != nil {
			return nil, errors.Join(err, tx.finish())
		}
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			tx.files = append(tx.files, installBackup{path: path})
			continue
		}
		if err != nil {
			return nil, errors.Join(err, tx.finish())
		}
		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return nil, errors.Join(fmt.Errorf("install target is not a regular file or symlink: %s", path), tx.finish())
		}
		tmp, err := os.CreateTemp(filepath.Dir(path), ".agent-dry-backup-*")
		if err != nil {
			return nil, errors.Join(err, tx.finish())
		}
		backup := tmp.Name()
		closeErr := tmp.Close()
		removeErr := os.Remove(backup)
		if err := errors.Join(closeErr, removeErr); err != nil {
			return nil, errors.Join(err, tx.finish())
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, readErr := os.Readlink(path)
			if readErr != nil {
				return nil, errors.Join(readErr, tx.finish())
			}
			err = os.Symlink(target, backup)
		} else {
			err = os.Link(path, backup)
		}
		if err != nil {
			return nil, errors.Join(err, tx.finish())
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
			} else if err := os.Remove(file.backup); err != nil && !errors.Is(err, os.ErrNotExist) {
				// Renaming two hard links to the same inode is a no-op.
				failures = append(failures, err)
			}
		} else if err := os.Remove(file.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, fmt.Errorf("rollback %s: %w", file.path, err))
		}
	}
	for i := len(tx.dirs) - 1; i >= 0; i-- {
		if err := os.Remove(tx.dirs[i]); err != nil && !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
