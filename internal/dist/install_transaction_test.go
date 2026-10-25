package dist

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

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
