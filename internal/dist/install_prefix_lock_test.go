package dist

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/moabualruz/rice-agent/internal/fslock"
)

func assertInstallLocksHeld(t *testing.T, paths []string) {
	t.Helper()
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		_ = f.Close()
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			t.Fatalf("mutation does not hold lock for %s: %v", path, err)
		}
	}
}

func TestPrefixMigrationAndUninstallHoldAllLocksThroughTransaction(t *testing.T) {
	for _, action := range []string{"install", "uninstall"} {
		for _, result := range []string{"commit", "rollback"} {
			t.Run(action+"/"+result, func(t *testing.T) {
				f := newInstallFixture(t)
				if code, _, stderr := run(t, f.vars, "install"); code != 0 {
					t.Fatal(stderr)
				}
				requested := filepath.Join(f.home, "new-prefix")
				if err := os.MkdirAll(requested, 0o755); err != nil {
					t.Fatal(err)
				}
				f.vars["PATH"] = requested + ":/usr/bin:/bin"
				locks := []string{f.state, f.prefix, requested}
				for _, path := range []string{filepath.Join(f.state, "install.lock"), filepath.Join(f.prefix, ".agent-dry-install.lock"), filepath.Join(requested, ".agent-dry-install.lock")} {
					writeFile(t, path, "", 0o600)
					locks = append(locks, path)
				}
				before := snapshotInstallTree(t, f.home)
				write, remove := installReceiptWrite, installLinkRemove
				t.Cleanup(func() { installReceiptWrite, installLinkRemove = write, remove })
				checked := false
				installReceiptWrite = func(path string, data []byte, mode fs.FileMode) error {
					checked = true
					assertInstallLocksHeld(t, locks)
					if result == "rollback" {
						return errors.New("injected receipt failure")
					}
					return write(path, data, mode)
				}
				installLinkRemove = func(path string) error {
					assertInstallLocksHeld(t, locks)
					// Keep one link so uninstall must commit a replacement receipt.
					if filepath.Base(path) == "agent-dry" {
						return errors.New("injected retained link")
					}
					return remove(path)
				}
				args := []string{"install", "--prefix", requested}
				if action == "uninstall" {
					args = append(args, "--uninstall")
				}
				code, _, stderr := run(t, f.vars, args...)
				if !checked || (result == "commit" && code != 0) || (result == "rollback" && (code == 0 || !strings.Contains(stderr, "injected receipt failure"))) {
					t.Fatalf("transaction result: checked=%v code=%d stderr=%s", checked, code, stderr)
				}
				if result == "rollback" {
					if after := snapshotInstallTree(t, f.home); !reflect.DeepEqual(before, after) {
						t.Fatal("failed transaction did not roll back both prefixes and receipt")
					}
				}
				unlock, err := lockInstallPaths(f.state, requested, f.prefix)
				if err != nil {
					t.Fatalf("transaction did not release its full lock set: %v", err)
				}
				unlock()
			})
		}
	}
}

func TestInstallPrefixLockOrderIsIndependentOfMigrationDirection(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	prefixes := []string{filepath.Join(root, "a"), filepath.Join(root, "z")}
	for _, path := range append([]string{state}, prefixes...) {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		name := ".agent-dry-install.lock"
		if path == state {
			name = "install.lock"
		}
		writeFile(t, filepath.Join(path, name), "", 0o600)
	}
	unlock, err := lockInstallPaths(state, prefixes[0], prefixes[1], prefixes[0])
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	done := make(chan error, 1)
	go func() {
		release, err := lockInstallPaths(state, prefixes[1], prefixes[0])
		if err == nil {
			release()
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("overlapping reversed lock set did not wait: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	unlock = nil
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("reversed migration lock order deadlocked")
	}
}

func TestPrefixMigrationAndUninstallRespectEveryPrefixLock(t *testing.T) {
	for _, action := range []string{"install", "uninstall", "install-dry-run", "uninstall-dry-run"} {
		for _, kind := range []string{"directory", "legacy"} {
			t.Run(action+"/"+kind, func(t *testing.T) {
				f := newInstallFixture(t)
				if code, _, stderr := run(t, f.vars, "install"); code != 0 {
					t.Fatal(stderr)
				}
				former := f.prefix
				requested := filepath.Join(f.home, "new-prefix")
				if err := os.MkdirAll(requested, 0o755); err != nil {
					t.Fatal(err)
				}
				f.vars["PATH"] = requested + ":/usr/bin:/bin"
				var unlock func()
				var err error
				if kind == "directory" {
					unlock, err = fslock.Lock([]string{former}, time.Second)
				} else {
					unlock, err = lockInstallFile(filepath.Join(former, ".agent-dry-install.lock"))
				}
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
				before := snapshotInstallTree(t, f.home)
				args := []string{"install", "--prefix", requested}
				if strings.HasPrefix(action, "uninstall") {
					args = append(args, "--uninstall")
				}
				if strings.HasSuffix(action, "-dry-run") {
					args = append(args, "--dry-run")
				}
				if code, _, stderr := run(t, f.vars, args...); code == 0 || !strings.Contains(stderr, "busy") {
					t.Errorf("%s ignored former-prefix %s lock: code=%d stderr=%s", action, kind, code, stderr)
				}
				if after := snapshotInstallTree(t, f.home); !reflect.DeepEqual(before, after) {
					t.Error("locked former prefix, requested prefix or receipt changed")
				}
			})
		}
	}
}
