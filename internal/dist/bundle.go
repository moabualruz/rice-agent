package dist

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/moabualruz/agent-dry/internal/cli"
	"github.com/moabualruz/agent-dry/internal/commands"
	"github.com/moabualruz/agent-dry/internal/paths"
)

const bundleUsage = `Usage: agent-dry bundle [--goos linux] [--goarch amd64] [--out <file>]

Cross-compile cmd/agent-dry with CGO_ENABLED=0 and the version ldflag (git describe of the root),
then pack bin/agent-dry, tools/, skills/ (if present), registry/, policy/, docs/, schemas/ and a
VERSION file into a tar.gz. Prints the path, sha256, size and version as JSON.

Options:
  --goos <os>      target operating system (default linux)
  --goarch <arch>  target architecture (default amd64)
  --out <file>     output file (default <root>/dist/agent-dry-<version>-<goos>-<goarch>.tar.gz)

Exit codes: 0 bundle written, 1 build failed, 2 usage, 3 environment (no source root, no go).

Example: agent-dry bundle --goos linux --goarch amd64 --out /tmp/agent-dry.tar.gz
`

// VersionLDFlag is the linker flag prefix that stamps the build version.
const VersionLDFlag = "-X github.com/moabualruz/agent-dry/internal/commands.Version="

// Bundle describes a written release tarball.
type Bundle struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Bytes   int64  `json:"bytes"`
	Version string `json:"version"`
	GOOS    string `json:"goos"`
	GOARCH  string `json:"goarch"`
}

// bundleDirs are packed in this order when present; registry is required.
var bundleDirs = []string{"tools", "skills", "registry", "policy", "docs", "schemas"}

var versionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

func runBundle(env *cli.Env) int {
	fs := flag.NewFlagSet("bundle", flag.ContinueOnError)
	goos := fs.String("goos", "linux", "")
	goarch := fs.String("goarch", "amd64", "")
	out := fs.String("out", "", "")
	pos, code, ok := parseArgs(env, fs, bundleUsage)
	if !ok {
		return code
	}
	if len(pos) > 0 {
		return cli.Usagef(env, "bundle: unexpected argument %q", pos[0])
	}
	if !regexp.MustCompile(`^[a-z0-9]+$`).MatchString(*goos) || !regexp.MustCompile(`^[a-z0-9]+$`).MatchString(*goarch) {
		return cli.Usagef(env, "bundle: invalid --goos or --goarch")
	}
	root, err := paths.Root(env.Getenv)
	if err != nil {
		return cli.Envf(env, "bundle: %v", err)
	}
	dst := *out
	if dst != "" {
		if dst, err = absPath(dst, paths.Home(env.Getenv)); err != nil {
			return cli.Usagef(env, "bundle: --out: %v", err)
		}
	}
	b, code := buildBundle(env, root, *goos, *goarch, dst)
	if b == nil {
		return code
	}
	if err := writeJSON(env.Stdout, b); err != nil {
		return cli.Envf(env, "bundle: %v", err)
	}
	return cli.ExitOK
}

// describe returns git describe for root, or "" when root is not itself a Git worktree root (an
// installed bundle may sit inside an unrelated repository, such as a versioned home directory).
func describe(root, pathEnv string) string {
	git, ok := lookPath("git", pathEnv)
	if !ok {
		return ""
	}
	gitOut := func(args ...string) (string, bool) {
		cmd := exec.Command(git, append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err == nil
	}
	top, ok := gitOut("rev-parse", "--show-toplevel")
	if !ok || realOrSelf(top) != realOrSelf(root) {
		return ""
	}
	v, ok := gitOut("describe", "--tags", "--always", "--dirty")
	if !ok {
		return ""
	}
	return v
}

// rootVersion names the content of root: git describe, the VERSION file of an installed bundle,
// or the running build version.
func rootVersion(root, pathEnv string) (string, bool) {
	if v := describe(root, pathEnv); v != "" {
		return v, true
	}
	if raw, err := os.ReadFile(filepath.Join(root, "VERSION")); err == nil {
		if v := strings.TrimSpace(string(raw)); versionPattern.MatchString(v) {
			return v, false
		}
	}
	return commands.Version, false
}

func sanitizeVersion(v string) string {
	b := []byte(v)
	for i, c := range b {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '+' || c == '-'
		if !ok {
			b[i] = '-'
		}
	}
	s := strings.TrimLeft(string(b), ".-_+")
	if s == "" {
		return "dev"
	}
	return s
}

// goBuildEnv is the process environment for a reproducible cross-compile: GOFLAGS is dropped so a
// host-level setting cannot change the build, and the target is set explicitly.
func goBuildEnv(goos, goarch string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "GOFLAGS", "GOOS", "GOARCH", "CGO_ENABLED":
			continue
		}
		env = append(env, kv)
	}
	return append(env, "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
}

// buildBundle compiles and packs a bundle. It returns nil and an exit code on failure.
func buildBundle(env *cli.Env, root, goos, goarch, out string) (*Bundle, int) {
	for _, need := range []string{"go.mod", filepath.Join("cmd", "agent-dry")} {
		if _, err := os.Stat(filepath.Join(root, need)); err != nil {
			return nil, cli.Envf(env, "bundle: %s is not a source checkout (missing %s)", root, need)
		}
	}
	if _, err := os.Stat(filepath.Join(root, paths.Marker)); err != nil {
		return nil, cli.Envf(env, "bundle: missing %s", paths.Marker)
	}
	goBin, ok := lookPath("go", env.Getenv("PATH"))
	if !ok {
		return nil, cli.Envf(env, "bundle: go not found on PATH")
	}
	v, _ := rootVersion(root, env.Getenv("PATH"))
	version := sanitizeVersion(v)
	if out == "" {
		out = filepath.Join(root, "dist", fmt.Sprintf("agent-dry-%s-%s-%s.tar.gz", version, goos, goarch))
	}
	work, err := os.MkdirTemp("", "agent-dry-bundle-")
	if err != nil {
		return nil, cli.Envf(env, "bundle: %v", err)
	}
	defer os.RemoveAll(work)
	bin := filepath.Join(work, "agent-dry")
	cmd := exec.Command(goBin, "build", "-trimpath", "-ldflags", "-s -w "+VersionLDFlag+version, "-o", bin, "./cmd/agent-dry")
	cmd.Dir = root
	cmd.Env = goBuildEnv(goos, goarch)
	if outb, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(env.Stderr, "agent-dry: bundle: go build for %s-%s failed: %v\n%s", goos, goarch, err, tailLines(string(outb), 40))
		return nil, cli.ExitNegative
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return nil, cli.Envf(env, "bundle: %v", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(out), "."+filepath.Base(out)+".agent-dry-*")
	if err != nil {
		return nil, cli.Envf(env, "bundle: %v", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := writeTarball(tmp, root, bin, version); err != nil {
		tmp.Close()
		return nil, cli.Envf(env, "bundle: %v", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, cli.Envf(env, "bundle: %v", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return nil, cli.Envf(env, "bundle: %v", err)
	}
	if err := os.Rename(tmpName, out); err != nil {
		return nil, cli.Envf(env, "bundle: %v", err)
	}
	sum, err := sha256File(out)
	if err != nil {
		return nil, cli.Envf(env, "bundle: %v", err)
	}
	info, err := os.Stat(out)
	if err != nil {
		return nil, cli.Envf(env, "bundle: %v", err)
	}
	return &Bundle{Path: out, SHA256: sum, Bytes: info.Size(), Version: version, GOOS: goos, GOARCH: goarch}, cli.ExitOK
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n") + "\n"
}

// writeTarball packs the bundle layout into w as gzip-compressed tar.
func writeTarball(w io.Writer, root, bin, version string) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	now := time.Now().Truncate(time.Second)
	dir := func(name string) error {
		return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: 0o755, ModTime: now})
	}
	file := func(name string, mode int64, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode, Size: int64(len(data)), ModTime: now}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	if err := file("VERSION", 0o644, []byte(version+"\n")); err != nil {
		return err
	}
	if err := dir("bin"); err != nil {
		return err
	}
	exe, err := os.ReadFile(bin)
	if err != nil {
		return err
	}
	if err := file("bin/agent-dry", 0o755, exe); err != nil {
		return err
	}
	for _, d := range bundleDirs {
		src := filepath.Join(root, d)
		info, err := os.Stat(src)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", src)
		}
		if err := addTree(tw, root, src); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// addTree adds src (inside root) with relative names, skipping .git and .DS_Store.
func addTree(tw *tar.Writer, root, src string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" || d.Name() == ".DS_Store" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		mt := info.ModTime().Truncate(time.Second)
		switch {
		case info.IsDir():
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: 0o755, ModTime: mt})
		case info.Mode()&fs.ModeSymlink != 0:
			dest, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if filepath.IsAbs(dest) || !within(root, filepath.Join(filepath.Dir(p), dest)) {
				return fmt.Errorf("%s: symlink leaves the bundle", p)
			}
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: name, Linkname: dest, Mode: 0o777, ModTime: mt})
		case info.Mode().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			mode := int64(0o644)
			if info.Mode()&0o111 != 0 {
				mode = 0o755
			}
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode, Size: int64(len(data)), ModTime: mt}); err != nil {
				return err
			}
			_, err = io.Copy(tw, bytes.NewReader(data))
			return err
		default:
			return nil
		}
	})
}
