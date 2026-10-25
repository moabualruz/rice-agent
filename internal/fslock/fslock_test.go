package fslock

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMissingRootLocksCreationWithoutCreatingFiles(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "missing", "root")
	unlock, err := Lock([]string{root}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("lock changed filesystem: %v %v", entries, err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if second, err := Lock([]string{root}, 0); err == nil {
		second()
		t.Fatal("newly created root bypassed its creator's lock")
	}
	unlock()
	next, err := Lock([]string{root}, 0)
	if err != nil {
		t.Fatal(err)
	}
	next()
}

func TestDisjointExistingRootsDoNotShareExclusiveLock(t *testing.T) {
	parent := t.TempDir()
	a, b := filepath.Join(parent, "a"), filepath.Join(parent, "b")
	for _, path := range []string{a, b} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	first, err := Lock([]string{a}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer first()
	second, err := Lock([]string{b}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer second()
}

func TestOverlappingRootsAndMultipleTargetsShareOneLockOrder(t *testing.T) {
	parent := t.TempDir()
	a, b := filepath.Join(parent, "a"), filepath.Join(parent, "b")
	for _, path := range []string{a, b} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	unlock, err := Lock([]string{b, a, filepath.Join(a, "missing")}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if next, err := Lock([]string{a}, 0); err == nil {
		next()
		t.Fatal("overlapping mutation bypassed locks")
	}
}
