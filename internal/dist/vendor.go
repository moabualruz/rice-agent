package dist

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/moabualruz/agent-dry/internal/cli"
	"github.com/moabualruz/agent-dry/internal/manifest"
	"github.com/moabualruz/agent-dry/internal/paths"
)

const vendorUsage = `Usage: agent-dry vendor <tool> --into <repo> [--as <relpath>] [--dry-run]
       agent-dry vendor --check <repo>

Copy a tool's entry from the agent-dry root into a repository (default tools/<tool>/<entry>),
preserving its mode, and record it in <repo>/.agent-dry/vendor.json and
<repo>/.agent-dry/SHA256SUMS (accepted by sha256sum -c and shasum -a 256 -c from the repo root).
Never stages or commits; prints the changed files relative to the repo.

--check verifies integrity (every file matches SHA256SUMS and vendor.json) and drift (every file
matches the tool's current content in the root; drift prints "outdated" with both hashes).

Options:
  --into <repo>     repository to copy into
  --as <relpath>    destination inside the repository, for example scripts/check-no-dashes.sh
  --dry-run         print the files that would change and change nothing
  --check <repo>    verify vendored copies in <repo>

Exit codes: 0 vendored or clean, 1 drift, integrity failure or unknown tool, 2 usage,
3 environment (no root, unreadable manifest or repository).

Example: agent-dry vendor check-no-dashes --into . --as scripts/check-no-dashes.sh
`

// Vendor manifest constants.
const (
	VendorSchema = "vendor/v1"
	VendorSource = "moabualruz/agent-dry"
	VendorDir    = ".agent-dry"
	VendorJSON   = "vendor.json"
	VendorSums   = "SHA256SUMS"
)

// VendorManifest is <repo>/.agent-dry/vendor.json.
type VendorManifest struct {
	Schema string       `json:"schema"`
	Source string       `json:"source"`
	Ref    string       `json:"ref"`
	Files  []VendorFile `json:"files"`
}

// VendorFile is one vendored copy; Path is relative to the repository root with forward slashes.
type VendorFile struct {
	Path   string `json:"path"`
	Tool   string `json:"tool"`
	SHA256 string `json:"sha256"`
}

// VendorFinding is one vendored-copy check result.
type VendorFinding struct {
	Path    string `json:"path"`
	Tool    string `json:"tool,omitempty"`
	Status  string `json:"status"` // ok, integrity, outdated
	Detail  string `json:"detail,omitempty"`
	Want    string `json:"vendored_sha256,omitempty"`
	Current string `json:"current_sha256,omitempty"`
}

// VendorReport is the result of CheckVendored.
type VendorReport struct {
	Repo     string          `json:"repo"`
	Ref      string          `json:"ref"`
	Findings []VendorFinding `json:"findings"`
}

// Clean reports whether every finding is ok.
func (r *VendorReport) Clean() bool {
	for _, f := range r.Findings {
		if f.Status != "ok" {
			return false
		}
	}
	return true
}

var toolName = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

func runVendor(env *cli.Env) int {
	fs := flag.NewFlagSet("vendor", flag.ContinueOnError)
	into := fs.String("into", "", "")
	as := fs.String("as", "", "")
	dryRun := fs.Bool("dry-run", false, "")
	check := fs.String("check", "", "")
	pos, code, ok := parseArgs(env, fs, vendorUsage)
	if !ok {
		return code
	}
	home := paths.Home(env.Getenv)
	if *check != "" {
		if len(pos) > 0 || *into != "" || *as != "" {
			return cli.Usagef(env, "vendor: --check takes only a repository")
		}
		repo, err := absPath(*check, home)
		if err != nil {
			return cli.Usagef(env, "vendor: --check: %v", err)
		}
		return vendorCheck(env, repo)
	}
	if len(pos) != 1 || *into == "" {
		return cli.Usagef(env, "vendor: need <tool> and --into <repo>; run agent-dry vendor --help")
	}
	repo, err := absPath(*into, home)
	if err != nil {
		return cli.Usagef(env, "vendor: --into: %v", err)
	}
	return vendorCopy(env, pos[0], repo, *as, *dryRun)
}

// cleanRel validates a repository-relative path and returns it with forward slashes.
func cleanRel(rel string) (string, error) {
	s := filepath.ToSlash(rel)
	if s == "" || path.IsAbs(s) || path.Clean(s) != s || s == "." {
		return "", fmt.Errorf("%q must be a clean relative path", rel)
	}
	for _, part := range strings.Split(s, "/") {
		if part == ".." || part == ".git" {
			return "", fmt.Errorf("%q must stay inside the repository and outside .git", rel)
		}
	}
	if s == VendorDir || strings.HasPrefix(s, VendorDir+"/") {
		return "", fmt.Errorf("%q is inside %s", rel, VendorDir)
	}
	return s, nil
}

// noSymlinkBetween refuses a destination whose existing components under repo are symlinks.
func noSymlinkBetween(repo, dst string) error {
	for cur := dst; cur != repo && within(repo, cur); cur = filepath.Dir(cur) {
		info, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink; refusing to write through it", cur)
		}
		if cur == dst && !info.Mode().IsRegular() {
			return fmt.Errorf("%s exists and is not a regular file", cur)
		}
	}
	return nil
}

func readVendorManifest(repo string) (*VendorManifest, error) {
	raw, err := os.ReadFile(filepath.Join(repo, VendorDir, VendorJSON))
	if err != nil {
		return nil, err
	}
	var m VendorManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(VendorDir, VendorJSON), err)
	}
	if m.Schema != VendorSchema {
		return nil, fmt.Errorf("%s: schema %q, want %q", filepath.Join(VendorDir, VendorJSON), m.Schema, VendorSchema)
	}
	return &m, nil
}

func sumsContent(files []VendorFile) []byte {
	var b bytes.Buffer
	for _, f := range files {
		fmt.Fprintf(&b, "%s  %s\n", f.SHA256, f.Path)
	}
	return b.Bytes()
}

// vendorRef names the root content: git describe, or bundle-<version> for an installed bundle.
func vendorRef(root, pathEnv string) string {
	v, git := rootVersion(root, pathEnv)
	if git {
		return v
	}
	return "bundle-" + v
}

func vendorCopy(env *cli.Env, name, repo, as string, dryRun bool) int {
	if !toolName.MatchString(name) {
		return cli.Usagef(env, "vendor: invalid tool name %q", name)
	}
	root, err := distRoot(env.Getenv)
	if err != nil {
		return cli.Envf(env, "vendor: %v", err)
	}
	dir := filepath.Join(root, "tools", name)
	if _, err := os.Stat(filepath.Join(dir, manifest.FileName)); errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(env.Stderr, "agent-dry: vendor: no tool %q in %s\n", name, root)
		return cli.ExitNegative
	}
	t, err := manifest.Load(dir)
	if err != nil {
		return cli.Envf(env, "vendor: %v", err)
	}
	if !t.Listed() {
		fmt.Fprintf(env.Stderr, "agent-dry: vendor: %s is %s; vendor only trial or active tools\n", name, t.Status)
		return cli.ExitNegative
	}
	if info, err := os.Stat(repo); err != nil || !info.IsDir() {
		return cli.Usagef(env, "vendor: --into %s is not a directory", repo)
	}
	repo = realOrSelf(repo)
	if as == "" {
		as = path.Join("tools", name, filepath.ToSlash(t.Entry))
	}
	rel, err := cleanRel(as)
	if err != nil {
		return cli.Usagef(env, "vendor: --as %v", err)
	}
	dst := filepath.Join(repo, filepath.FromSlash(rel))
	if err := noSymlinkBetween(repo, dst); err != nil {
		fmt.Fprintf(env.Stderr, "agent-dry: vendor: %v\n", err)
		return cli.ExitNegative
	}
	src := t.EntryPath()
	data, err := os.ReadFile(src)
	if err != nil {
		return cli.Envf(env, "vendor: %v", err)
	}
	srcInfo, err := os.Stat(src)
	if err != nil {
		return cli.Envf(env, "vendor: %v", err)
	}
	mode := srcInfo.Mode().Perm()
	sum := sha256Bytes(data)

	m, err := readVendorManifest(repo)
	if errors.Is(err, fs.ErrNotExist) {
		m = &VendorManifest{}
	} else if err != nil {
		return cli.Envf(env, "vendor: %v", err)
	}
	m.Schema, m.Source, m.Ref = VendorSchema, VendorSource, vendorRef(root, env.Getenv("PATH"))
	merged := []VendorFile{{Path: rel, Tool: name, SHA256: sum}}
	for _, f := range m.Files {
		if f.Path != rel {
			merged = append(merged, f)
		}
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Path < merged[j].Path })
	m.Files = merged
	jsonBytes, err := marshalJSON(m)
	if err != nil {
		return cli.Envf(env, "vendor: %v", err)
	}
	sums := sumsContent(merged)

	jsonPath := filepath.Join(repo, VendorDir, VendorJSON)
	sumsPath := filepath.Join(repo, VendorDir, VendorSums)
	var changed []string
	if old, err := os.ReadFile(dst); err != nil || !bytes.Equal(old, data) {
		changed = append(changed, rel)
	} else if info, err := os.Stat(dst); err != nil || info.Mode().Perm() != mode {
		changed = append(changed, rel)
	}
	if old, err := os.ReadFile(jsonPath); err != nil || !bytes.Equal(old, jsonBytes) {
		changed = append(changed, VendorDir+"/"+VendorJSON)
	}
	if old, err := os.ReadFile(sumsPath); err != nil || !bytes.Equal(old, sums) {
		changed = append(changed, VendorDir+"/"+VendorSums)
	}
	if dryRun {
		for _, c := range changed {
			fmt.Fprintf(env.Stdout, "would write %s\n", c)
		}
		if len(changed) == 0 {
			fmt.Fprintln(env.Stdout, "vendor: up to date")
		}
		return cli.ExitOK
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return cli.Envf(env, "vendor: %v", err)
	}
	if err := noSymlinkBetween(repo, dst); err != nil {
		fmt.Fprintf(env.Stderr, "agent-dry: vendor: %v\n", err)
		return cli.ExitNegative
	}
	if err := writeFileAtomic(dst, data, mode); err != nil {
		return cli.Envf(env, "vendor: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repo, VendorDir), 0o755); err != nil {
		return cli.Envf(env, "vendor: %v", err)
	}
	if err := writeFileAtomic(jsonPath, jsonBytes, 0o644); err != nil {
		return cli.Envf(env, "vendor: %v", err)
	}
	if err := writeFileAtomic(sumsPath, sums, 0o644); err != nil {
		return cli.Envf(env, "vendor: %v", err)
	}
	if got, err := sha256File(dst); err != nil || got != sum {
		return cli.Envf(env, "vendor: verification of %s failed", rel)
	}
	for _, c := range changed {
		fmt.Fprintln(env.Stdout, c)
	}
	if len(changed) == 0 {
		fmt.Fprintln(env.Stdout, "vendor: up to date")
	}
	return cli.ExitOK
}

var sumLine = regexp.MustCompile(`^([0-9a-f]{64}) [ *](.+)$`)

// CheckVendored compares the copies recorded in repo/.agent-dry with their checksums and with the
// current tool content under root. The error is set only when nothing could be checked.
func CheckVendored(root, repo string) (*VendorReport, error) {
	m, err := readVendorManifest(repo)
	if err != nil {
		return nil, err
	}
	rep := &VendorReport{Repo: repo, Ref: m.Ref}
	fail := func(p, tool, status, detail string) {
		rep.Findings = append(rep.Findings, VendorFinding{Path: p, Tool: tool, Status: status, Detail: detail})
	}
	sums := map[string]string{}
	raw, err := os.ReadFile(filepath.Join(repo, VendorDir, VendorSums))
	if err != nil {
		fail(VendorDir+"/"+VendorSums, "", "integrity", "unreadable: "+err.Error())
	} else {
		sc := bufio.NewScanner(bytes.NewReader(raw))
		for n := 1; sc.Scan(); n++ {
			line := sc.Text()
			if line == "" {
				continue
			}
			g := sumLine.FindStringSubmatch(line)
			if g == nil {
				fail(VendorDir+"/"+VendorSums, "", "integrity", fmt.Sprintf("line %d is not sha256sum format", n))
				continue
			}
			sums[g[2]] = g[1]
		}
	}
	recorded := map[string]bool{}
	for _, f := range m.Files {
		recorded[f.Path] = true
		if _, err := cleanRel(f.Path); err != nil {
			fail(f.Path, f.Tool, "integrity", err.Error())
			continue
		}
		actual, err := sha256File(filepath.Join(repo, filepath.FromSlash(f.Path)))
		if err != nil {
			fail(f.Path, f.Tool, "integrity", "missing or unreadable")
			continue
		}
		if s, ok := sums[f.Path]; !ok || s != f.SHA256 {
			fail(f.Path, f.Tool, "integrity", "vendor.json and SHA256SUMS disagree")
			continue
		}
		if actual != f.SHA256 {
			rep.Findings = append(rep.Findings, VendorFinding{Path: f.Path, Tool: f.Tool, Status: "integrity",
				Detail: "content does not match SHA256SUMS", Want: f.SHA256, Current: actual})
			continue
		}
		t, err := manifest.Load(filepath.Join(root, "tools", f.Tool))
		if err != nil || !toolName.MatchString(f.Tool) {
			fail(f.Path, f.Tool, "outdated", "tool is not in the agent-dry root")
			continue
		}
		current, err := sha256File(t.EntryPath())
		if err != nil {
			fail(f.Path, f.Tool, "outdated", "tool entry unreadable")
			continue
		}
		if current != actual {
			rep.Findings = append(rep.Findings, VendorFinding{Path: f.Path, Tool: f.Tool, Status: "outdated",
				Detail: "differs from the tool in the agent-dry root", Want: actual, Current: current})
			continue
		}
		rep.Findings = append(rep.Findings, VendorFinding{Path: f.Path, Tool: f.Tool, Status: "ok", Want: actual, Current: current})
	}
	extra := make([]string, 0)
	for p := range sums {
		if !recorded[p] {
			extra = append(extra, p)
		}
	}
	sort.Strings(extra)
	for _, p := range extra {
		fail(p, "", "integrity", "in SHA256SUMS but not in vendor.json")
	}
	return rep, nil
}

func vendorCheck(env *cli.Env, repo string) int {
	root, err := distRoot(env.Getenv)
	if err != nil {
		return cli.Envf(env, "vendor: %v", err)
	}
	rep, err := CheckVendored(root, repo)
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(env.Stderr, "agent-dry: vendor: %s has no %s/%s\n", repo, VendorDir, VendorJSON)
		return cli.ExitNegative
	}
	if err != nil {
		fmt.Fprintf(env.Stderr, "agent-dry: vendor: %v\n", err)
		return cli.ExitNegative
	}
	problems := 0
	for _, f := range rep.Findings {
		switch f.Status {
		case "ok":
			fmt.Fprintf(env.Stdout, "ok %s (%s)\n", f.Path, f.Tool)
		case "outdated":
			problems++
			if f.Want != "" {
				fmt.Fprintf(env.Stdout, "outdated %s (%s): vendored %s current %s\n", f.Path, f.Tool, f.Want, f.Current)
			} else {
				fmt.Fprintf(env.Stdout, "outdated %s (%s): %s\n", f.Path, f.Tool, f.Detail)
			}
		default:
			problems++
			if f.Want != "" {
				fmt.Fprintf(env.Stdout, "integrity %s: %s (recorded %s actual %s)\n", f.Path, f.Detail, f.Want, f.Current)
			} else {
				fmt.Fprintf(env.Stdout, "integrity %s: %s\n", f.Path, f.Detail)
			}
		}
	}
	if problems > 0 {
		fmt.Fprintf(env.Stdout, "vendor check: %d of %d finding(s) need attention (ref %s)\n", problems, len(rep.Findings), rep.Ref)
		return cli.ExitNegative
	}
	fmt.Fprintf(env.Stdout, "vendor check: %d file(s) clean (ref %s)\n", len(rep.Findings), rep.Ref)
	return cli.ExitOK
}
