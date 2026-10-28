package gates

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moabualruz/rice-agent/internal/cli"
	"github.com/moabualruz/rice-agent/internal/paths"
	"github.com/moabualruz/rice-agent/internal/registry"
)

// TestMain pins the Go build cache before tests move HOME, so Go scaffolds build warm.
func TestMain(m *testing.M) {
	for _, k := range []string{"GOCACHE", "GOPATH"} {
		if os.Getenv(k) != "" {
			continue
		}
		cmd := exec.Command("go", "env", k)
		cmd.Env = childEnv()
		if out, err := cmd.Output(); err == nil {
			os.Setenv(k, strings.TrimSpace(string(out)))
		}
	}
	os.Exit(m.Run())
}

var fixtureRoot = mustAbs(filepath.Join("..", "..", "testdata", "gates", "root"))

func mustAbs(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		panic(err)
	}
	return abs
}

// fakeIndex returns fixed results, or an error from Load.
type fakeIndex struct{ results []registry.Result }

func (f fakeIndex) Search(string, int) []registry.Result { return f.results }

// useIndex replaces the registry for one test.
func useIndex(t *testing.T, err error, results ...registry.Result) {
	t.Helper()
	saved := loadIndex
	t.Cleanup(func() { loadIndex = saved })
	loadIndex = func(func(string) string) (searcher, error) {
		if err != nil {
			return nil, err
		}
		return fakeIndex{results}, nil
	}
}

func hit(name, kind string) registry.Result {
	return registry.Result{Name: name, Summary: "Use when testing.", Kind: kind, Scope: "global", Entry: name, Source: "agent-dry", Score: registry.Threshold + 3}
}

// isolate points rice-agent at root and moves HOME, state and config into temporary directories.
func isolate(t *testing.T, root string) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("AGENT_DRY_ROOT", root)
	t.Setenv("HOME", filepath.Join(tmp, "home"))
	t.Setenv("AGENT_DRY_STATE", filepath.Join(tmp, "state"))
	t.Setenv("AGENT_DRY_CONFIG", filepath.Join(tmp, "config.json"))
	t.Setenv("AGENT_DRY_BUILDER", "test-model")
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, "cache"))
}

// emptyRoot is a fresh rice-agent root with only the marker file.
func emptyRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, paths.Marker), `{"schema":"wanted/v1","families":[]}`+"\n", 0o644)
	return root
}

// rootWith is a fresh rice-agent root holding copies of the named fixture tools.
func rootWith(t *testing.T, tools ...string) string {
	t.Helper()
	root := emptyRoot(t)
	for _, name := range tools {
		copyDir(t, filepath.Join(fixtureRoot, "tools", name), filepath.Join(root, "tools", name))
	}
	return root
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, p, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// run invokes a registered command in process.
func run(t *testing.T, name string, args ...string) (int, string, string) {
	t.Helper()
	c, ok := cli.Lookup(name)
	if !ok {
		t.Fatalf("command %q is not registered", name)
	}
	var out, errOut bytes.Buffer
	env := &cli.Env{Args: args, Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut, Getenv: os.Getenv}
	code := c.Run(env)
	return code, out.String(), errOut.String()
}

// gateLine is one parsed check output line.
type gateLine struct{ status, reason string }

// parseCheck maps "label/G<n>" to the gate's line.
func parseCheck(t *testing.T, out string) map[string]gateLine {
	t.Helper()
	got := map[string]gateLine{}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) < 4 || !strings.HasPrefix(f[1], "G") {
			continue
		}
		label, reason, _ := strings.Cut(strings.Join(f[3:], " "), ": ")
		got[label+"/"+f[1]] = gateLine{status: f[0], reason: reason}
	}
	return got
}

// statuses is the six gate statuses of one tool, space separated.
func statuses(lines map[string]gateLine, label string) string {
	var s []string
	for _, g := range []string{"G1", "G2", "G3", "G4", "G5", "G6"} {
		s = append(s, lines[label+"/"+g].status)
	}
	return strings.Join(s, " ")
}

func requireCommands(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			t.Skipf("%s not installed", n)
		}
	}
}

// pathOnly sets PATH to a directory linking just the named commands.
func pathOnly(t *testing.T, names ...string) {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		p, err := exec.LookPath(n)
		if err != nil {
			t.Skipf("%s not installed", n)
		}
		if err := os.Symlink(p, filepath.Join(dir, n)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "test@example.invalid"}, {"config", "user.name", "test"}} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = childEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}

func gitAdd(t *testing.T, dir string, files ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "add", "--"}, files...)...)
	cmd.Env = childEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return !errors.Is(err, os.ErrNotExist)
}
