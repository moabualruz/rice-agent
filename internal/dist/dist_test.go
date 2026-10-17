package dist

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moabualruz/agent-dry/internal/cli"
)

const fixtures = "../../testdata/dist"

// run executes a registered command with the given environment variables only.
func run(t *testing.T, vars map[string]string, args ...string) (int, string, string) {
	t.Helper()
	c, ok := cli.Lookup(args[0])
	if !ok {
		t.Fatalf("command %q not registered", args[0])
	}
	var out, errb bytes.Buffer
	env := &cli.Env{Args: args[1:], Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb,
		Getenv: func(k string) string { return vars[k] }}
	code := c.Run(env)
	return code, out.String(), errb.String()
}

// copyTree copies src into dst preserving file modes and relative symlinks.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			return os.MkdirAll(target, 0o755)
		case info.Mode()&fs.ModeSymlink != 0:
			dest, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(dest, target)
		default:
			in, err := os.Open(p)
			if err != nil {
				return err
			}
			defer in.Close()
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, in); err != nil {
				out.Close()
				return err
			}
			return out.Close()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}

// tempDir is t.TempDir with symlinks resolved (macOS temp lives under /private).
func tempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// fixtureRoot copies the fixture agent-dry root into a temporary directory.
func fixtureRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(tempDir(t), "root")
	copyTree(t, filepath.Join(fixtures, "root"), root)
	return root
}

func writeFile(t *testing.T, path, content string, mode fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// fixtureModule turns a fixture root into a buildable source checkout whose cmd/agent-dry is a
// tiny stand-in: `version` prints the stamped version; anything else records its arguments in
// $HOME/install-args and exits 7, so remote tests can see the exit status travel back.
func fixtureModule(t *testing.T) string {
	t.Helper()
	root := fixtureRoot(t)
	writeFile(t, filepath.Join(root, "go.mod"), "module github.com/moabualruz/agent-dry\n\ngo 1.21\n", 0o644)
	writeFile(t, filepath.Join(root, "internal", "commands", "version.go"),
		"package commands\n\n// Version is stamped by the bundle ldflag.\nvar Version = \"dev\"\n", 0o644)
	writeFile(t, filepath.Join(root, "cmd", "agent-dry", "main.go"), `package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/moabualruz/agent-dry/internal/commands"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(commands.Version)
		return
	}
	line := strings.Join(os.Args[1:], " ") + "\n"
	if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), "install-args"), []byte(line), 0o644); err != nil {
		os.Exit(3)
	}
	fmt.Println("fixture agent-dry", commands.Version, line)
	os.Exit(7)
}
`, 0o644)
	return root
}

func requireCommands(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			t.Skipf("%s not available", n)
		}
	}
}

func TestCommandsRegisteredWithHelp(t *testing.T) {
	for _, name := range []string{"install", "doctor", "vendor", "bundle", "section"} {
		code, out, errb := run(t, map[string]string{}, name, "--help")
		if code != 0 || !strings.HasPrefix(out, "Usage: agent-dry "+name) {
			t.Errorf("%s --help: code %d, out %q, err %q", name, code, out, errb)
		}
		if hasDash(out) {
			t.Errorf("%s --help contains an em or en dash", name)
		}
		if code, _, _ := run(t, map[string]string{}, name, "--no-such-flag"); code != cli.ExitUsage {
			t.Errorf("%s --no-such-flag: code %d, want 2", name, code)
		}
	}
}

func TestParseArgsInterspersed(t *testing.T) {
	code, _, errb := run(t, map[string]string{}, "vendor", "--into")
	if code != cli.ExitUsage || !strings.Contains(errb, "flag needs an argument") {
		t.Fatalf("missing flag value: code %d err %q", code, errb)
	}
}

func execOutput(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}
