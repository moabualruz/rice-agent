// Package fslock locks existing directory identities without creating lock files.
package fslock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// ExistingAnchor returns the nearest existing directory at or above path.
func ExistingAnchor(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	for {
		info, err := os.Stat(path)
		if err == nil {
			if !info.IsDir() {
				return "", errors.New("lock anchor is not a directory")
			}
			return filepath.EvalSymlinks(path)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", err
		}
		path = parent
	}
}

type request struct {
	path      string
	exclusive bool
}

func requests(paths []string) ([]request, []string, error) {
	modes := map[string]bool{}
	anchors := make([]string, len(paths))
	for i, path := range paths {
		anchor, err := ExistingAnchor(path)
		if err != nil {
			return nil, nil, err
		}
		anchors[i] = anchor
		modes[anchor] = true
		for parent := filepath.Dir(anchor); parent != anchor; parent = filepath.Dir(parent) {
			if _, ok := modes[parent]; !ok {
				modes[parent] = false
			}
			if filepath.Dir(parent) == parent {
				break
			}
		}
	}
	out := make([]request, 0, len(modes))
	for path, exclusive := range modes {
		out = append(out, request{path, exclusive})
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].path) != len(out[j].path) {
			return len(out[i].path) < len(out[j].path)
		}
		return out[i].path < out[j].path
	})
	return out, anchors, nil
}

// Lock holds each anchor exclusively and its ancestors shared. A writer creating
// a missing root therefore blocks a second writer entering that new root. All
// callers use the same ancestor order, and retry if an anchor changed while
// waiting. Disjoint existing roots do not serialize on a global exclusive lock.
func Lock(paths []string, timeout time.Duration) (func(), error) {
	deadline := time.Now().Add(timeout)
	for {
		plan, anchors, err := requests(paths)
		if err != nil {
			return nil, err
		}
		var files []*os.File
		release := func() {
			for i := len(files) - 1; i >= 0; i-- {
				_ = files[i].Close()
			}
		}
		retry := false
		for _, item := range plan {
			f, err := os.Open(item.path)
			if err != nil {
				release()
				if errors.Is(err, os.ErrNotExist) {
					retry = true
					break
				}
				return nil, err
			}
			mode := syscall.LOCK_SH
			if item.exclusive {
				mode = syscall.LOCK_EX
			}
			err = syscall.Flock(int(f.Fd()), mode|syscall.LOCK_NB)
			if err != nil {
				f.Close()
				release()
				if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
					retry = true
					break
				}
				return nil, err
			}
			files = append(files, f)
			opened, err := f.Stat()
			current, statErr := os.Stat(item.path)
			if err != nil || statErr != nil || !os.SameFile(opened, current) {
				release()
				retry = true
				break
			}
		}
		if !retry {
			for i, path := range paths {
				anchor, err := ExistingAnchor(path)
				if err != nil || anchor != anchors[i] {
					retry = true
					break
				}
			}
			if !retry {
				return release, nil
			}
			release()
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("filesystem mutation is busy")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
