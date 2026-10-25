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
)

func TestInstallAndUninstallRespectStateLock(t *testing.T) {
	for _, action := range []string{"install", "uninstall"} {
		t.Run(action, func(t *testing.T) {
			f := newInstallFixture(t)
			if code, _, errb := run(t, f.vars, "install"); code != 0 {
				t.Fatalf("setup: %d %s", code, errb)
			}
			before := readFile(t, filepath.Join(f.state, ReceiptFile))
			lock, err := os.OpenFile(filepath.Join(f.state, "install.lock"), os.O_CREATE|os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
				t.Fatal(err)
			}
			defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			args := []string{"install"}
			if action == "uninstall" {
				args = append(args, "--uninstall")
			}
			if code, _, _ := run(t, f.vars, args...); code == 0 {
				t.Fatal("mutation ignored active install lock")
			}
			if got := readFile(t, filepath.Join(f.state, ReceiptFile)); got != before {
				t.Fatal("locked receipt changed")
			}
		})
	}
}

func TestInstallAndUninstallRespectSharedPrefixLock(t *testing.T) {
	for _, action := range []string{"install", "uninstall"} {
		t.Run(action, func(t *testing.T) {
			f := newInstallFixture(t)
			if code, _, errb := run(t, f.vars, "install"); code != 0 {
				t.Fatalf("setup: %d %s", code, errb)
			}
			before := readFile(t, filepath.Join(f.state, ReceiptFile))
			unlock, err := lockInstallFile(filepath.Join(f.prefix, ".agent-dry-install.lock"))
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			args := []string{"install"}
			if action == "uninstall" {
				args = append(args, "--uninstall")
			}
			if code, _, _ := run(t, f.vars, args...); code == 0 {
				t.Fatal("mutation ignored shared prefix lock")
			}
			if got := readFile(t, filepath.Join(f.state, ReceiptFile)); got != before {
				t.Fatal("locked receipt changed")
			}
		})
	}
}

func TestInstallRollsBackBinariesLinksAndReceiptOnWriteFailure(t *testing.T) {
	f := newInstallFixture(t)
	name := "dfx-go-rollback"
	addGoToolFixture(t, f.root, name)
	packaged := filepath.Join(f.root, "tools", name, "bin", platform(), name)
	writeFile(t, packaged, "#!/bin/sh\necho previous\n", 0o755)
	code, _, stderr := run(t, f.vars, "install")
	if code != 0 {
		t.Fatalf("initial install: %d %s", code, stderr)
	}
	target := filepath.Join(f.state, "go-tools", name, platform(), name)
	beforeBinary := readFile(t, target)
	beforeReceipt := readFile(t, filepath.Join(f.state, ReceiptFile))
	previousLink, err := os.Readlink(filepath.Join(f.prefix, name))
	if err != nil {
		t.Fatal(err)
	}
	// A stale command is removed during application and must also be rolled back.
	if err := os.RemoveAll(filepath.Join(f.root, "tools", "dfx-hello")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, packaged, "#!/bin/sh\necho replacement\n", 0o755)
	saved := installReceiptWrite
	t.Cleanup(func() { installReceiptWrite = saved })
	installReceiptWrite = func(string, []byte, fs.FileMode) error { return errors.New("injected receipt write failure") }
	code, _, stderr = run(t, f.vars, "install")
	if code == 0 {
		t.Fatal("receipt failure was ignored")
	}
	if got := readFile(t, target); got != beforeBinary {
		t.Fatal("stable binary was not rolled back")
	}
	if got := readFile(t, filepath.Join(f.state, ReceiptFile)); got != beforeReceipt {
		t.Fatal("receipt changed")
	}
	if got, err := os.Readlink(filepath.Join(f.prefix, name)); err != nil || got != previousLink {
		t.Fatal("Go command link changed")
	}
	if _, err := os.Readlink(filepath.Join(f.prefix, "dfx-hello")); err != nil {
		t.Fatal("stale link was not rolled back")
	}
}

func TestGoToolInstallRejectsSymlinkedBinaryParents(t *testing.T) {
	for _, kind := range []string{"packaged", "installed"} {
		t.Run(kind, func(t *testing.T) {
			f := newInstallFixture(t)
			name := "dfx-go-symlink"
			addGoToolFixture(t, f.root, name)
			packaged := filepath.Join(f.root, "tools", name, "bin", platform(), name)
			writeFile(t, packaged, "#!/bin/sh\necho packaged\n", 0o755)
			external := t.TempDir()
			if kind == "packaged" {
				writeFile(t, filepath.Join(external, name), "#!/bin/sh\necho external\n", 0o755)
				if err := os.Remove(packaged); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Dir(packaged)); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(external, filepath.Dir(packaged)); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.MkdirAll(f.state, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(external, filepath.Join(f.state, "go-tools")); err != nil {
					t.Fatal(err)
				}
			}
			if code, _, _ := run(t, f.vars, "install"); code == 0 {
				t.Fatal("symlinked binary parent accepted")
			}
			if exists(filepath.Join(f.state, ReceiptFile)) {
				t.Fatal("receipt changed on unsafe install")
			}
		})
	}
}

func TestCommitGoToolsVerifiesAllStagedBinariesBeforeReplacement(t *testing.T) {
	dir := t.TempDir()
	var tools []goToolBinary
	for _, name := range []string{"one", "two"} {
		staged := filepath.Join(dir, name+"-staged")
		target := filepath.Join(dir, name)
		writeFile(t, staged, "new binary", 0o755)
		writeFile(t, target, "previous binary", 0o755)
		sum, err := sha256File(staged)
		if err != nil {
			t.Fatal(err)
		}
		tools = append(tools, goToolBinary{Name: name, Path: staged, Target: target, SHA256: sum})
	}
	writeFile(t, tools[1].Path, "tampered", 0o755)
	if err := commitGoTools(tools); err == nil {
		t.Fatal("tampered staging file accepted")
	}
	for _, tool := range tools {
		if got := readFile(t, tool.Target); got != "previous binary" {
			t.Fatal("binary replaced before all integrity checks finished")
		}
	}
}

func TestRound5InvalidReceiptDoesNotStageGoTools(t *testing.T) {
	f := newInstallFixture(t)
	name := "dfx-go-invalid-receipt"
	addGoToolFixture(t, f.root, name)
	writeFile(t, filepath.Join(f.root, "tools", name, "bin", platform(), name), "#!/bin/sh\nexit 0\n", 0o755)
	writeFile(t, filepath.Join(f.state, ReceiptFile), "invalid receipt", 0o644)
	before := snapshotInstallTree(t, f.home)
	if code, _, _ := run(t, f.vars, "install"); code == 0 {
		t.Fatal("invalid receipt accepted")
	}
	if after := snapshotInstallTree(t, f.home); !reflect.DeepEqual(before, after) {
		t.Fatal("receipt preflight staged tools or lock paths")
	}
}

func TestRound5UninstallRejectsReceiptPrefixSymlinkEscape(t *testing.T) {
	f := newInstallFixture(t)
	if code, _, stderr := run(t, f.vars, "install"); code != 0 {
		t.Fatal(stderr)
	}
	moved := filepath.Join(f.home, "moved-prefix")
	if err := os.Rename(f.prefix, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, f.prefix); err != nil {
		t.Fatal(err)
	}
	before := snapshotInstallTree(t, f.home)
	if code, _, _ := run(t, f.vars, "install", "--uninstall"); code == 0 {
		t.Fatal("receipt prefix symlink accepted")
	}
	if after := snapshotInstallTree(t, f.home); !reflect.DeepEqual(before, after) {
		t.Fatal("rejected receipt escaped into another directory")
	}
}

func TestRound5InitialInstallFailureRestoresAbsentDirectories(t *testing.T) {
	f := newInstallFixture(t)
	name := "dfx-go-rollback-dirs"
	addGoToolFixture(t, f.root, name)
	writeFile(t, filepath.Join(f.root, "tools", name, "bin", platform(), name), "#!/bin/sh\nexit 0\n", 0o755)
	before := snapshotInstallTree(t, f.home)
	saved := installReceiptWrite
	installReceiptWrite = func(string, []byte, fs.FileMode) error { return errors.New("injected receipt failure") }
	t.Cleanup(func() { installReceiptWrite = saved })
	if code, _, _ := run(t, f.vars, "install"); code == 0 {
		t.Fatal("receipt failure ignored")
	}
	if after := snapshotInstallTree(t, f.home); !reflect.DeepEqual(before, after) {
		t.Fatalf("initial failure left created directories: before=%v after=%v", before, after)
	}
}

func snapshotInstallTree(t *testing.T, root string) map[string]string {
	t.Helper()
	got := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		value := info.Mode().String()
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value += string(data)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			value += target
		}
		got[rel] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestRound5InstallCollisionLeavesAbsentStateAndPrefixUnchanged(t *testing.T) {
	f := newInstallFixture(t)
	shadow := filepath.Join(f.home, "shadow")
	writeFile(t, filepath.Join(shadow, "dfx-hello"), "#!/bin/sh\n", 0o755)
	f.vars["PATH"] = f.prefix + ":" + shadow + ":/usr/bin:/bin"
	if exists(f.state) || exists(f.prefix) {
		t.Fatal("fixture unexpectedly has state or prefix directories")
	}
	before := snapshotInstallTree(t, f.home)
	if code, _, stderr := run(t, f.vars, "install"); code != 1 || !strings.Contains(stderr, "collision") {
		t.Fatalf("install collision: code=%d stderr=%s", code, stderr)
	}
	if after := snapshotInstallTree(t, f.home); !reflect.DeepEqual(before, after) {
		t.Fatalf("blocked install changed tree: before=%v after=%v", before, after)
	}
}

func TestRound5InstallRejectsSymlinkedLockPathsWithoutChanges(t *testing.T) {
	for _, kind := range []string{"state", "prefix"} {
		t.Run(kind, func(t *testing.T) {
			f := newInstallFixture(t)
			if err := os.MkdirAll(f.state, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(f.prefix, 0o755); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(f.home, "lock-target")
			writeFile(t, target, "preserve", 0o600)
			lockPath := filepath.Join(f.state, "install.lock")
			if kind == "prefix" {
				lockPath = filepath.Join(f.prefix, ".agent-dry-install.lock")
			}
			if err := os.Symlink(target, lockPath); err != nil {
				t.Fatal(err)
			}
			before := snapshotInstallTree(t, f.home)
			if code, _, _ := run(t, f.vars, "install"); code == 0 {
				t.Fatal("install accepted a symlinked lock path")
			}
			if after := snapshotInstallTree(t, f.home); !reflect.DeepEqual(before, after) {
				t.Fatalf("blocked install changed tree: before=%v after=%v", before, after)
			}
		})
	}
}

func TestRound5UninstallChecksReplacedReceiptOnlyAfterPrefixLock(t *testing.T) {
	f := newInstallFixture(t)
	if code, _, stderr := run(t, f.vars, "install"); code != 0 {
		t.Fatalf("setup install: code=%d stderr=%s", code, stderr)
	}
	unlock, err := lockInstallFile(filepath.Join(f.prefix, ".agent-dry-install.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	rpath := filepath.Join(f.state, ReceiptFile)
	replacement := []byte("replacement while prefix lock held")
	if err := os.WriteFile(rpath, replacement, 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := run(t, f.vars, "install", "--uninstall")
	if code == 0 || !strings.Contains(stderr, "busy") || strings.Contains(stderr, "replacement while prefix lock held") {
		t.Fatalf("uninstall read the replaced receipt before the prefix lock: code=%d stderr=%s", code, stderr)
	}
	if got := readFile(t, rpath); got != string(replacement) {
		t.Fatalf("receipt changed while prefix lock was held: %q", got)
	}
}

func TestRound5UninstallRollsBackLinksWhenReceiptWriteFails(t *testing.T) {
	f := newInstallFixture(t)
	if code, _, stderr := run(t, f.vars, "install"); code != 0 {
		t.Fatalf("setup install: code=%d stderr=%s", code, stderr)
	}
	before := snapshotInstallTree(t, f.home)
	savedWrite := installReceiptWrite
	savedRemove := installLinkRemove
	t.Cleanup(func() {
		installReceiptWrite = savedWrite
		installLinkRemove = savedRemove
	})
	installReceiptWrite = func(string, []byte, fs.FileMode) error { return errors.New("injected receipt write failure") }
	installLinkRemove = func(path string) error {
		if filepath.Base(path) == "agent-dry" {
			return errors.New("injected link removal failure")
		}
		return os.Remove(path)
	}
	if code, _, stderr := run(t, f.vars, "install", "--uninstall"); code == 0 || !strings.Contains(stderr, "injected receipt write failure") {
		t.Fatalf("receipt write failure ignored: code=%d stderr=%s", code, stderr)
	}
	if after := snapshotInstallTree(t, f.home); !reflect.DeepEqual(before, after) {
		t.Fatalf("failed uninstall did not restore exact tree: before=%v after=%v", before, after)
	}
}

func TestRound5RelativeStateGoToolLinksSurviveWorkingDirectoryChanges(t *testing.T) {
	f := newInstallFixture(t)
	name := "dfx-go-relative-state"
	addGoToolFixture(t, f.root, name)
	packaged := filepath.Join(f.root, "tools", name, "bin", platform(), name)
	writeFile(t, packaged, "#!/bin/sh\nexit 0\n", 0o755)
	f.vars["AGENT_DRY_STATE"] = "relative-state"
	originalWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(f.home); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(originalWD) })
	if code, _, stderr := run(t, f.vars, "install"); code != 0 {
		t.Fatalf("install with relative state: code=%d stderr=%s", code, stderr)
	}
	receiptPath := filepath.Join(f.home, "relative-state", ReceiptFile)
	receipt, err := loadReceipt(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	var goLink *Link
	for i := range receipt.Links {
		if receipt.Links[i].Name == name {
			goLink = &receipt.Links[i]
			break
		}
	}
	if goLink == nil || !filepath.IsAbs(goLink.Target) {
		t.Fatalf("Go tool link target is not absolute: %+v", goLink)
	}
	otherWD := t.TempDir()
	if err := os.Chdir(otherWD); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(goLink.Path); err != nil || got != goLink.Target {
		t.Fatalf("Go tool link target after cwd change: got=%q want=%q err=%v", got, goLink.Target, err)
	}
	if _, err := os.Stat(goLink.Path); err != nil {
		t.Fatalf("Go tool link does not resolve after cwd change: %v", err)
	}
	if err := os.Chdir(f.home); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := run(t, f.vars, "install", "--uninstall"); code != 0 {
		t.Fatalf("uninstall with relative state: code=%d stderr=%s", code, stderr)
	}
}
