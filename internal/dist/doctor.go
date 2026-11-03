package dist

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/moabualruz/rice-agent/internal/cli"
	"github.com/moabualruz/rice-agent/internal/manifest"
	"github.com/moabualruz/rice-agent/internal/paths"
	"github.com/moabualruz/rice-agent/internal/registry"
)

const doctorUsage = `Usage: rice-agent doctor [--json]

Report this host's rice-agent health: config presence and parse, root, prefix on PATH, every
receipt link, tools the root installs that are not linked, commands that linked tools require,
index cache freshness (state index.json versus manifests), go, jq and shellcheck availability, and
zvec-grep (zg) presence and the semantic corpus index state (rice-agent index --semantic).
Warnings (missing go or shellcheck, a missing or stale index that find rebuilds, no zg or no corpus
index, which only turn semantic recall off) do not fail.

Options:
  --json   print the checks as JSON

Exit codes: 0 healthy (warnings allowed), 1 problems found, 2 usage.

Example: rice-agent doctor
`

// IndexFile is the index cache name inside the state directory.
const IndexFile = "index.json"

// Check is one doctor finding.
type Check struct {
	Status string `json:"status"` // PASS, WARN or FAIL
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

type doctorReport struct {
	Healthy  bool    `json:"healthy"`
	Problems int     `json:"problems"`
	Checks   []Check `json:"checks"`
}

func runDoctor(env *cli.Env) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	pos, code, ok := parseArgs(env, fs, doctorUsage)
	if !ok {
		return code
	}
	if len(pos) > 0 {
		return cli.Usagef(env, "doctor: unexpected argument %q", pos[0])
	}
	checks := diagnose(env.Getenv)
	rep := doctorReport{Checks: checks}
	for _, c := range checks {
		if c.Status == "FAIL" {
			rep.Problems++
		}
	}
	rep.Healthy = rep.Problems == 0
	if *asJSON {
		if err := writeJSON(env.Stdout, rep); err != nil {
			return cli.Envf(env, "doctor: %v", err)
		}
	} else {
		for _, c := range checks {
			fmt.Fprintf(env.Stdout, "%s %s: %s\n", c.Status, c.Name, c.Detail)
		}
		if rep.Healthy {
			fmt.Fprintln(env.Stdout, "doctor: healthy")
		} else {
			fmt.Fprintf(env.Stdout, "doctor: %d problem(s)\n", rep.Problems)
		}
	}
	if !rep.Healthy {
		return cli.ExitNegative
	}
	return cli.ExitOK
}

func diagnose(getenv func(string) string) []Check {
	var out []Check
	add := func(status, name, format string, a ...any) {
		out = append(out, Check{Status: status, Name: name, Detail: fmt.Sprintf(format, a...)})
	}
	pathEnv := getenv("PATH")

	cfgPath := paths.ConfigPath(getenv)
	if _, err := os.Stat(cfgPath); errors.Is(err, fs.ErrNotExist) {
		add("FAIL", "config", "%s is missing; create it with at least \"root\"", cfgPath)
	} else if cfg, err := paths.LoadConfig(getenv); err != nil {
		add("FAIL", "config", "%v", err)
	} else {
		add("PASS", "config", "%s (root %s, %d host(s), %d repo(s))", cfgPath, orNone(cfg.Root), len(cfg.Hosts), len(cfg.Repos))
	}

	root, rootErr := distRoot(getenv)
	if rootErr != nil {
		add("FAIL", "root", "%v", rootErr)
	} else {
		add("PASS", "root", "%s", root)
	}
	stateDir, stateErr := stateDirectory(getenv)
	if stateErr != nil {
		stateDir, _ = filepath.Abs(paths.StateDir(getenv))
		add("FAIL", "state", "%v", stateErr)
	}

	var tools []*manifest.Tool
	if rootErr == nil {
		var err error
		tools, err = manifest.LoadAll(filepath.Join(root, "tools"))
		if err != nil {
			add("FAIL", "manifests", "%v", err)
		} else {
			add("PASS", "manifests", "%d tool(s)", len(tools))
		}
	}

	rpath := receiptPath(getenv)
	receipt, rerr := loadReceipt(rpath)
	prefix := defaultPrefix(getenv)
	switch {
	case errors.Is(rerr, fs.ErrNotExist):
		add("FAIL", "install", "no receipt at %s; run rice-agent install", rpath)
	case rerr != nil:
		add("FAIL", "install", "%v", rerr)
	default:
		prefix = receipt.Prefix
		add("PASS", "install", "receipt %s (version %s, %d link(s))", rpath, receipt.Version, len(receipt.Links))
	}
	if onPath(prefix, pathEnv) {
		add("PASS", "path", "%s is on PATH", prefix)
	} else {
		add("FAIL", "path", "%s is not on PATH", prefix)
	}

	linked := map[string]string{} // link path -> target
	if receipt != nil {
		var bad []string
		changed := 0
		for _, l := range receipt.Links {
			if problem := linkProblem(l); problem != "" {
				bad = append(bad, l.Path+": "+problem)
				continue
			}
			linked[l.Path] = l.Target
			if sum, err := sha256File(l.Target); err == nil && l.SHA256 != "" && sum != l.SHA256 {
				changed++
			}
		}
		for _, b := range bad {
			add("FAIL", "link", "%s", b)
		}
		if len(bad) == 0 {
			add("PASS", "links", "%d link(s) valid", len(receipt.Links))
		}
		if changed > 0 {
			add("WARN", "links", "%d target(s) changed since install; re-run rice-agent install to refresh the receipt", changed)
		}
	}

	if rootErr == nil && tools != nil {
		var missing []string
		required := map[string][]string{}
		for _, t := range tools {
			if !installable(t) {
				continue
			}
			lp := filepath.Join(prefix, t.Name)
			expected := t.EntryPath()
			if t.Language == "go" {
				expected = goToolTarget(filepath.Join(stateDir, "go-tools"), t.Name, platform())
			}
			if linked[lp] != expected {
				missing = append(missing, t.Name)
				continue
			}
			for _, r := range t.Requires {
				cmd, _, _ := strings.Cut(r, " ")
				required[cmd] = append(required[cmd], t.Name)
			}
		}
		if len(missing) > 0 {
			add("FAIL", "tools", "not linked: %s; run rice-agent install", strings.Join(missing, ", "))
		}
		names := make([]string, 0, len(required))
		for n := range required {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			if _, ok := lookPath(n, pathEnv); !ok {
				add("FAIL", "requires", "%s (needed by %s) not found on PATH", n, strings.Join(required[n], ", "))
			}
		}
	}

	if rootErr == nil {
		idx := filepath.Join(stateDir, IndexFile)
		info, err := os.Stat(idx)
		if err == nil {
			var f *os.File
			if f, err = os.Open(idx); err == nil {
				f.Close()
			}
		}
		newest, newestPath := newestManifest(root)
		// find rebuilds a missing or stale cache itself; only an unreadable one breaks lookups.
		switch {
		case errors.Is(err, fs.ErrNotExist):
			add("WARN", "index", "no cache at %s yet; rice-agent find or index builds it", idx)
		case err != nil:
			add("FAIL", "index", "unreadable cache: %v", err)
		case newest.After(info.ModTime()):
			add("WARN", "index", "%s is older than %s; the next find rebuilds it (or run rice-agent index --rebuild)", idx, newestPath)
		default:
			add("PASS", "index", "%s is fresh", idx)
		}
	}

	// Semantic recall is optional: these lines are PASS or WARN, never FAIL.
	for _, s := range registry.SemanticChecks(getenv) {
		add(s.Status, s.Name, "%s", s.Detail)
	}

	for _, tool := range []string{"go", "jq", "shellcheck"} {
		if p, ok := lookPath(tool, pathEnv); ok {
			add("PASS", "toolchain", "%s at %s", tool, p)
		} else {
			add("WARN", "toolchain", "%s not found on PATH", tool)
		}
	}
	return out
}

func orNone(s string) string {
	if s == "" {
		return "unset"
	}
	return s
}

// InstalledGoToolMatches requires the native install receipt, link and unchanged artifact bytes.
func InstalledGoToolMatches(name, linkPath string, getenv func(string) string) bool {
	stateDir, err := stateDirectory(getenv)
	if err != nil {
		return false
	}
	receipt, err := loadReceipt(receiptPath(getenv))
	if err != nil || receipt.Platform != platform() {
		return false
	}
	tool, err := manifest.Load(filepath.Join(receipt.Root, "tools", name))
	if err != nil || tool.Language != "go" || tool.Scope != "global" || !tool.Listed() {
		return false
	}
	target := goToolTarget(filepath.Join(stateDir, "go-tools"), name, platform())
	resolved, err := resolvedPath(target, "")
	if err != nil || resolved != target {
		return false
	}
	for _, link := range receipt.Links {
		if link.Name != name || link.Path != linkPath || link.Target != target || link.SHA256 == "" || linkProblem(link) != "" {
			continue
		}
		sum, err := sha256File(target)
		return err == nil && sum == link.SHA256
	}
	return false
}

// linkProblem returns why a receipt link is no longer valid, or "".
func linkProblem(l Link) string {
	info, err := os.Lstat(l.Path)
	if err != nil {
		return "missing"
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		return "not a symlink"
	}
	dest, err := readLinkAbs(l.Path)
	if err != nil {
		return err.Error()
	}
	if dest != l.Target {
		return "points to " + dest + ", receipt says " + l.Target
	}
	t, err := os.Stat(dest)
	if err != nil {
		return "dangling: " + dest
	}
	if !t.Mode().IsRegular() || t.Mode()&0o111 == 0 {
		return "target is not an executable file"
	}
	return ""
}

// newestManifest returns the newest modification time over tools/*/tool.json, wanted.json and
// providers.json.
func newestManifest(root string) (time.Time, string) {
	var newest time.Time
	var newestPath string
	consider := func(p string) {
		if info, err := os.Stat(p); err == nil && info.ModTime().After(newest) {
			newest, newestPath = info.ModTime(), p
		}
	}
	consider(filepath.Join(root, paths.Marker))
	consider(filepath.Join(root, "registry", "providers.json"))
	matches, _ := filepath.Glob(filepath.Join(root, "tools", "*", manifest.FileName))
	for _, m := range matches {
		consider(m)
	}
	return newest, newestPath
}
