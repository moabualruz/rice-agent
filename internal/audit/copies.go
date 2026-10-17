package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/moabualruz/agent-dry/internal/manifest"
)

// skipDirs are never walked for copies: VCS metadata, dependency caches, build output and test
// fixtures (a fixture copy is not a deployed copy).
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, ".next": true, ".turbo": true, ".cache": true,
	".venv": true, "venv": true, "__pycache__": true, ".gradle": true, "target": true,
	"testdata": true,
}

// maxWalkEntries bounds the copy search across all roots.
const maxWalkEntries = 2_000_000

// copyNames are the basenames that identify copies of a tool: its entry, the file it was promoted
// from, and the entry with .sh for Bash tools (the usual name of an unpromoted script).
func copyNames(t *manifest.Tool) []string {
	set := map[string]bool{}
	entry := filepath.Base(t.Entry)
	set[entry] = true
	if t.Origin.PromotedFrom != "" {
		set[filepath.Base(t.Origin.PromotedFrom)] = true
	}
	if t.Language == "bash" && filepath.Ext(entry) == "" {
		set[entry+".sh"] = true
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// copyStat is the per-tool copy census.
type copyStat struct {
	Tool      string `json:"tool"`
	Copies    int    `json:"copies"`
	Variants  int    `json:"variants"`
	Canonical int    `json:"identical_to_canonical"`
	Baseline  string `json:"baseline,omitempty"`
}

type copyCensus struct {
	Source    string     `json:"source"` // config repos, or the ~/workspace/*/scripts fallback
	Roots     int        `json:"roots"`
	Truncated bool       `json:"truncated,omitempty"`
	Tools     []copyStat `json:"tools"`
}

// censusCopies walks roots once and groups files named like each tool by sha256. The tool's own
// canonical entry is excluded from the copies and used to count identical vendored copies.
func censusCopies(tools []*manifest.Tool, roots []string, source string) copyCensus {
	byName := map[string][]int{}
	for i, t := range tools {
		for _, n := range copyNames(t) {
			byName[n] = append(byName[n], i)
		}
	}
	canonical := make([]string, len(tools))
	canonPath := make([]string, len(tools))
	for i, t := range tools {
		canonPath[i] = t.EntryPath()
		canonical[i] = fileHash(canonPath[i])
	}
	hashes := make([]map[string]int, len(tools))
	for i := range hashes {
		hashes[i] = map[string]int{}
	}
	seenFile := map[string]bool{}
	visited := 0
	truncated := false
	// consider counts one regular file named like a tool, once per absolute path.
	consider := func(abs string, idx []int) {
		if seenFile[abs] {
			return
		}
		seenFile[abs] = true
		h := fileHash(abs)
		if h == "" {
			return
		}
		for _, i := range idx {
			if abs == canonPath[i] {
				continue
			}
			hashes[i][h]++
		}
	}
	for _, root := range roots {
		if truncated {
			break
		}
		// In a Git work tree only tracked files count, so untracked agent working copies
		// (.scratch state, harness worktrees) and ignored trees are not reported as copies.
		if files, ok := trackedFiles(root); ok {
			for _, abs := range files {
				visited++
				if visited > maxWalkEntries {
					truncated = true
					break
				}
				idx, named := byName[filepath.Base(abs)]
				if !named || inSkipDir(root, abs) {
					continue
				}
				if fi, err := os.Lstat(abs); err != nil || !fi.Mode().IsRegular() {
					continue
				}
				consider(abs, idx)
			}
			continue
		}
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			visited++
			if visited > maxWalkEntries {
				truncated = true
				return fs.SkipAll
			}
			if d.IsDir() {
				if p != root && skipDirs[d.Name()] {
					return fs.SkipDir
				}
				return nil
			}
			idx, ok := byName[d.Name()]
			if !ok || !d.Type().IsRegular() {
				return nil
			}
			abs, err := filepath.Abs(p)
			if err != nil {
				return nil
			}
			consider(abs, idx)
			return nil
		})
	}
	out := copyCensus{Source: source, Roots: len(roots), Truncated: truncated}
	for i, t := range tools {
		cs := copyStat{Tool: t.Name, Variants: len(hashes[i]), Baseline: copyBaselines[t.Name]}
		for h, n := range hashes[i] {
			cs.Copies += n
			if h == canonical[i] {
				cs.Canonical += n
			}
		}
		out.Tools = append(out.Tools, cs)
	}
	return out
}

func fileHash(p string) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// copyRoots returns the configured repository roots, or every ~/workspace/*/scripts directory
// when none are configured.
func copyRoots(repos []string, home string) ([]string, string) {
	if len(repos) > 0 {
		out := make([]string, 0, len(repos))
		for _, r := range repos {
			if r == "~" {
				r = home
			} else if strings.HasPrefix(r, "~/") {
				r = filepath.Join(home, r[2:])
			}
			out = append(out, r)
		}
		return out, "config repos"
	}
	matches, _ := filepath.Glob(filepath.Join(home, "workspace", "*", "scripts"))
	var out []string
	for _, m := range matches {
		if info, err := os.Stat(m); err == nil && info.IsDir() {
			out = append(out, m)
		}
	}
	return out, "fallback ~/workspace/*/scripts"
}

// trackedFiles lists the absolute paths Git tracks under root. ok is false when root is not inside a
// Git work tree, and the caller walks the directory instead.
func trackedFiles(root string) ([]string, bool) {
	out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		return nil, false
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, false
	}
	var files []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			files = append(files, filepath.Join(abs, f))
		}
	}
	return files, true
}

// inSkipDir reports whether a path below root passes through a directory in skipDirs.
func inSkipDir(root, abs string) bool {
	rel, err := filepath.Rel(root, filepath.Dir(abs))
	if err != nil {
		return false
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if skipDirs[part] {
			return true
		}
	}
	return false
}
