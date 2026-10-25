package dist

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	base := []string{"-C", dir, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid",
		"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false", "-c", "core.hooksPath=/dev/null"}
	cmd := exec.Command("git", append(base, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// readTarball returns name -> (mode, content) for every regular file and the set of names.
func readTarball(t *testing.T, path string) (map[string][]byte, map[string]int64) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	files, modes := map[string][]byte{}, map[string]int64{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		modes[h.Name] = h.Mode
		if h.Typeflag == tar.TypeReg {
			b, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			files[h.Name] = b
		}
	}
	return files, modes
}

func addGoToolFixture(t *testing.T, root, name string) {
	t.Helper()
	dir := filepath.Join(root, "tools", name)
	platforms := []string{platform()}
	if platform() != "linux-amd64" {
		platforms = append(platforms, "linux-amd64")
	}
	platformJSON, err := json.Marshal(platforms)
	if err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`{"schema":"tool/v1","name":%q,"summary":"Use when testing Go tool distribution","intents":["test Go tool bundle","test Go install","test Go compiler"],"kind":"cli","scope":"global","entry":%q,"language":"go","side_effects":"read","exit_codes":{"0":"success","2":"usage error"},"platforms":%s,"test":"go test .","status":"trial","origin":{"date":"2026-10-25","builder":"test","task":"fixture"}}`, name, name, platformJSON)
	writeFile(t, filepath.Join(dir, "tool.json"), manifest, 0o644)
	writeFile(t, filepath.Join(dir, name), "#!/bin/sh\nexit 0\n", 0o755)
	writeFile(t, filepath.Join(dir, "main.go"), "package main\nfunc main() {}\n", 0o644)
}

func TestBundleLinuxAmd64(t *testing.T) {
	requireCommands(t, "go", "git")
	root := fixtureModule(t)
	addGoToolFixture(t, root, "dfx-go")
	gitRun(t, root, "init", "-q")
	gitRun(t, root, "add", "-A")
	gitRun(t, root, "commit", "-q", "-m", "fixture")
	gitRun(t, root, "tag", "v0.0.0-fixture")
	out := filepath.Join(tempDir(t), "out", "agent-dry.tar.gz")
	vars := map[string]string{"HOME": tempDir(t), "AGENT_DRY_ROOT": root, "PATH": os.Getenv("PATH")}

	code, stdout, errb := run(t, vars, "bundle", "--goos", "linux", "--goarch", "amd64", "--out", out)
	if code != 0 {
		t.Fatalf("bundle: %d %s", code, errb)
	}
	var b Bundle
	if err := json.Unmarshal([]byte(stdout), &b); err != nil {
		t.Fatal(err)
	}
	sum, _ := sha256File(out)
	if b.Path != out || b.SHA256 != sum || b.Version != "v0.0.0-fixture" || b.GOOS != "linux" || b.GOARCH != "amd64" {
		t.Fatalf("bundle result: %+v (sha %s)", b, sum)
	}
	files, modes := readTarball(t, out)
	if string(files["VERSION"]) != "v0.0.0-fixture\n" {
		t.Fatalf("VERSION: %q", files["VERSION"])
	}
	bin := files["bin/agent-dry"]
	if !bytes.HasPrefix(bin, []byte("\x7fELF")) || modes["bin/agent-dry"] != 0o755 {
		t.Fatalf("bin/agent-dry is not a linux executable (mode %o)", modes["bin/agent-dry"])
	}
	if !bytes.Contains(bin, []byte("v0.0.0-fixture")) {
		t.Fatal("version ldflag not stamped into the binary")
	}
	for _, name := range []string{"tools/dfx-hello/dfx-hello", "tools/dfx-hello/tool.json", "registry/wanted.json",
		"policy/dry-policy.md", "docs/guide.md", "schemas/fixture.json"} {
		if _, ok := files[name]; !ok {
			t.Errorf("bundle lacks %s", name)
		}
	}
	if modes["tools/dfx-hello/dfx-hello"] != 0o755 || modes["registry/wanted.json"] != 0o644 {
		t.Errorf("modes: entry %o manifest %o", modes["tools/dfx-hello/dfx-hello"], modes["registry/wanted.json"])
	}
	goTool := files["tools/dfx-go/bin/linux-amd64/dfx-go"]
	if !bytes.HasPrefix(goTool, []byte("\x7fELF")) || modes["tools/dfx-go/bin/linux-amd64/dfx-go"] != 0o755 {
		t.Errorf("Go tool binary missing or invalid, mode %o", modes["tools/dfx-go/bin/linux-amd64/dfx-go"])
	}
	for name := range files {
		if strings.HasPrefix(name, "cmd/") || strings.HasPrefix(name, "internal/") || name == "go.mod" || strings.HasPrefix(name, ".git") {
			t.Errorf("bundle leaks source file %s", name)
		}
	}
}

func TestBundleHostBinaryReportsVersion(t *testing.T) {
	requireCommands(t, "go")
	root := fixtureModule(t)
	out := filepath.Join(tempDir(t), "host.tar.gz")
	vars := map[string]string{"HOME": tempDir(t), "AGENT_DRY_ROOT": root, "PATH": os.Getenv("PATH")}
	goos, goarch, _ := strings.Cut(platform(), "-")
	if code, _, errb := run(t, vars, "bundle", "--goos", goos, "--goarch", goarch, "--out", out); code != 0 {
		t.Fatalf("bundle: %s", errb)
	}
	files, _ := readTarball(t, out)
	exe := filepath.Join(tempDir(t), "agent-dry")
	writeFile(t, exe, string(files["bin/agent-dry"]), 0o755)
	got, err := exec.Command(exe, "version").Output()
	// Outside a Git checkout the version is the running build's, sanitized for a directory name.
	if err != nil || strings.TrimSpace(string(got)) != sanitizeVersion(rootVersionForTest(root)) {
		t.Fatalf("version: %q %v", got, err)
	}
}

func rootVersionForTest(root string) string {
	v, _ := rootVersion(root, os.Getenv("PATH"))
	return v
}

func TestBundleNeedsSourceCheckout(t *testing.T) {
	root := fixtureRoot(t)
	vars := map[string]string{"HOME": tempDir(t), "AGENT_DRY_ROOT": root, "PATH": os.Getenv("PATH")}
	if code, _, errb := run(t, vars, "bundle", "--out", filepath.Join(tempDir(t), "x.tar.gz")); code != 3 || !strings.Contains(errb, "not a source checkout") {
		t.Fatalf("no source: %d %s", code, errb)
	}
	if code, _, _ := run(t, vars, "bundle", "--goos", "../x"); code != 2 {
		t.Fatalf("bad goos: %d", code)
	}
}

func TestBundleBuildFailureIsNegative(t *testing.T) {
	requireCommands(t, "go")
	root := fixtureModule(t)
	writeFile(t, filepath.Join(root, "cmd", "agent-dry", "broken.go"), "package main\n\nfunc broken() { undefined() }\n", 0o644)
	vars := map[string]string{"HOME": tempDir(t), "AGENT_DRY_ROOT": root, "PATH": os.Getenv("PATH")}
	out := filepath.Join(tempDir(t), "x.tar.gz")
	if code, _, errb := run(t, vars, "bundle", "--out", out); code != 1 || !strings.Contains(errb, "go build for linux-amd64 failed") || exists(out) {
		t.Fatalf("build failure: %d %s", code, errb)
	}
}
