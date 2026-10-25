package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

type stateBefore struct {
	path     string
	mode     fs.FileMode
	data     []byte
	exists   bool
	created  fs.FileInfo
	preserve bool
}

var renameStatePath = os.Rename

// Journal only the paths owned by this mutation. Existing payload directories
// are moved back by the caller; new scaffolding is removed deepest first.
type stateTransaction struct {
	cfg       config
	before    []stateBefore
	seen      map[string]bool
	committed bool
}

func beginStateMutation(cfg config) (*stateTransaction, error) {
	return &stateTransaction{cfg: cfg, seen: map[string]bool{}}, nil
}

func (tx *stateTransaction) track(path string) error {
	if tx.seen[path] {
		return nil
	}
	if inside(tx.cfg.Root, path) {
		if err := validateExistingPath(tx.cfg.Root, path); err != nil {
			return err
		}
	} else if !inside(path, tx.cfg.Root) {
		return errors.New("transaction path is outside state root")
	}
	b := stateBefore{path: path}
	info, err := os.Lstat(path)
	if err == nil {
		b.exists = true
		b.mode = info.Mode()
		if info.Mode().IsRegular() {
			b.data, err = readStateFile(path)
		} else if !info.IsDir() {
			return errors.New("transaction path is not regular")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err != nil {
		return err
	}
	tx.before = append(tx.before, b)
	tx.seen[path] = true
	return nil
}

func (tx *stateTransaction) mkdir(path string) error {
	if err := validateExistingPath(tx.cfg.Root, path); err != nil {
		return err
	}
	var missing []string
	for p := path; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err == nil {
			if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
				return errors.New("directory path is unsafe")
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		missing = append(missing, p)
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := tx.track(missing[i]); err != nil {
			return err
		}
	}
	return mkdirState(tx.cfg.Root, path)
}

func withIndexTransaction(cfg config, stderr io.Writer, fn func(*registryIndex, *stateTransaction) int) int {
	return withIndexLock(cfg, func(idx *registryIndex) int {
		return withStateTransaction(cfg, stderr, func(tx *stateTransaction) int { return fn(idx, tx) })
	})
}

func withStateTransaction(cfg config, stderr io.Writer, fn func(*stateTransaction) int) (code int) {
	tx, _ := beginStateMutation(cfg)
	defer func() {
		if code == exitOK {
			tx.commit()
			return
		}
		if err := tx.rollback(); err != nil {
			fmt.Fprintln(stderr, "agent-state: rollback failed")
			code = exitEnv
		}
	}()
	return fn(tx)
}

func (tx *stateTransaction) saveIndex(idx registryIndex) error {
	if err := tx.track(indexPath(tx.cfg)); err != nil {
		return err
	}
	if err := tx.mkdir(registryPath(tx.cfg)); err != nil {
		return err
	}
	return saveIndex(tx.cfg, idx)
}

// Create-only writes join the journal only after publication succeeds. In
// particular, EEXIST must never make a concurrent writer's file ours to remove.
func (tx *stateTransaction) createJSON(path string, value any, mode fs.FileMode) error {
	if tx.seen[path] {
		return errors.New("create-only path is already journaled")
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := tx.mkdir(filepath.Dir(path)); err != nil {
		return err
	}
	info, err := createStateBytes(path, append(data, '\n'), mode)
	if info != nil {
		tx.before = append(tx.before, stateBefore{path: path, created: info})
		tx.seen[path] = true
	} else if errors.Is(err, os.ErrExist) {
		tx.preserveAncestors(path)
	}
	return err
}

func (tx *stateTransaction) preserveAncestors(path string) {
	for i := range tx.before {
		b := &tx.before[i]
		if !b.exists && b.created == nil && b.path != path && inside(b.path, path) {
			b.preserve = true
		}
	}
}

func (tx *stateTransaction) commit() { tx.committed = true }
func (tx *stateTransaction) rollback() error {
	if tx.committed {
		return nil
	}
	var errs []error
	sort.SliceStable(tx.before, func(i, j int) bool { return len(tx.before[i].path) > len(tx.before[j].path) })
	for _, b := range tx.before {
		if b.preserve {
			continue
		}
		var err error
		if b.created != nil {
			var current fs.FileInfo
			current, err = os.Lstat(b.path)
			if err == nil {
				if !os.SameFile(b.created, current) {
					tx.preserveAncestors(b.path)
					continue
				}
				err = os.Remove(b.path)
			}
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			}
		} else if !b.exists {
			err = os.Remove(b.path)
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			}
		} else if b.mode.IsRegular() {
			current, readErr := readStateFile(b.path)
			info, statErr := os.Stat(b.path)
			if readErr != nil || statErr != nil || info.Mode().Perm() != b.mode.Perm() || !bytes.Equal(current, b.data) {
				err = writeBytesAtomic(b.path, b.data, b.mode.Perm())
			}
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	tx.committed = true
	return errors.Join(errs...)
}
