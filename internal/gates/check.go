package gates

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/moabualruz/agent-dry/internal/cli"
	"github.com/moabualruz/agent-dry/internal/manifest"
	"github.com/moabualruz/agent-dry/internal/paths"
)

// Gate time limits; tests shorten them.
var (
	helpTimeout   = 10 * time.Second
	testTimeout   = 180 * time.Second
	staticTimeout = 120 * time.Second
)

const checkUsage = `Usage: agent-dry check [<tool>...|--all] [--skip-unavailable]

Run the admission gates on agent-dry tools, cheapest first. A failed gate stops that tool's
later gates, which are reported as SKIPPED.
  G1 manifest    tool/v1 schema, name matches its directory, no collision on PATH
  G2 duplicate   search of agent-dry's own tools and skills over name, summary and intents;
                 a match needs extends or distinct_from naming it
  G3 interface   exit codes 0 and 2 documented; --help exits 0 within 10s, starts with "Usage:"
                 and names the dry_run flag
  G4 static      no Python, no em or en dash, shellcheck (Bash) or go vet (Go), gitleaks when installed
  G5 behavior    the manifest test command exits 0 within 180s
  G6 provenance  origin filled; warns when the entry calls ssh, curl, gh or wget with side_effects read

A <tool> is a name under <root>/tools or a path to a tool directory. --all also fails on any
Python file tracked in the repository.

Flags:
  --all                check every tool under <root>/tools
  --skip-unavailable   report a missing checker (shellcheck, go) as SKIPPED instead of exit 3;
                       the run still exits 1, so a skipped gate never counts as proof

Exit codes: 0 every gate passed, 1 a gate failed or was skipped, 2 usage error,
3 environment (no agent-dry root, missing checker, unreadable registry).

Example:
  agent-dry check --all
`

type gateStatus string

const (
	statusPass gateStatus = "PASS"
	statusFail gateStatus = "FAIL"
	statusSkip gateStatus = "SKIPPED"
	statusWarn gateStatus = "WARN"
)

// gateResult is the outcome of one gate for one tool.
type gateResult struct {
	status      gateStatus
	reason      string
	env         bool // environment failure: the run exits 3
	unavailable bool // skipped because a checker is missing: the run exits 1
}

func pass() gateResult { return gateResult{status: statusPass} }
func failf(f string, a ...any) gateResult {
	return gateResult{status: statusFail, reason: oneLine(fmt.Sprintf(f, a...))}
}
func envFailf(f string, a ...any) gateResult { r := failf(f, a...); r.env = true; return r }
func warnf(f string, a ...any) gateResult {
	return gateResult{status: statusWarn, reason: oneLine(fmt.Sprintf(f, a...))}
}
func skippedf(f string, a ...any) gateResult {
	return gateResult{status: statusSkip, reason: oneLine(fmt.Sprintf(f, a...))}
}
func oneLine(s string) string    { return strings.Join(strings.Fields(s), " ") }
func (r gateResult) stops() bool { return r.status == statusFail }
func (r gateResult) withNote(n string) gateResult {
	if n != "" && r.reason == "" {
		r.reason = n
	}
	return r
}

type gate struct {
	id, name string
	run      func(c *checker, t *manifest.Tool) gateResult
}

// later are the gates after G1, in order; each needs the manifest G1 loaded.
var later = []gate{
	{"G2", "duplicate", (*checker).duplicate},
	{"G3", "interface", (*checker).iface},
	{"G4", "static", (*checker).static},
	{"G5", "behavior", (*checker).behavior},
	{"G6", "provenance", (*checker).provenance},
}

type checker struct {
	env             *cli.Env
	root, toolsDir  string
	skipUnavailable bool

	ix       searcher
	ixErr    error
	ixLoaded bool

	repoPy       []string
	repoPyLoaded bool

	tools, toolsPassed, toolsFailed, toolsIncomplete int
	gates                                            map[gateStatus]int
	envFailure, failure                              bool
}

func runCheck(env *cli.Env) int {
	var all, skip bool
	fset := flag.NewFlagSet("check", flag.ContinueOnError)
	fset.SetOutput(io.Discard)
	fset.BoolVar(&all, "all", false, "")
	fset.BoolVar(&skip, "skip-unavailable", false, "")
	args, err := parseInterleaved(fset, env.Args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(env.Stdout, checkUsage)
		return cli.ExitOK
	}
	if err != nil {
		return cli.Usagef(env, "check: %v; run agent-dry check --help", err)
	}
	if all == (len(args) > 0) {
		return cli.Usagef(env, "check: name tools or pass --all, not both or neither; run agent-dry check --help")
	}
	root, err := paths.Root(env.Getenv)
	if err != nil {
		return cli.Envf(env, "%v", err)
	}
	c := &checker{env: env, root: root, toolsDir: filepath.Join(root, "tools"), skipUnavailable: skip, gates: map[gateStatus]int{}}

	type target struct{ label, dir string }
	var targets []target
	if all {
		entries, err := os.ReadDir(c.toolsDir)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return cli.Envf(env, "read %s: %v", c.toolsDir, err)
		}
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				targets = append(targets, target{e.Name(), filepath.Join(c.toolsDir, e.Name())})
			}
		}
	}
	for _, a := range args {
		if strings.ContainsRune(a, filepath.Separator) || a == "." || a == ".." {
			abs, err := filepath.Abs(a)
			if err != nil {
				return cli.Usagef(env, "check: %v", err)
			}
			targets = append(targets, target{filepath.Base(abs), abs})
			continue
		}
		targets = append(targets, target{a, filepath.Join(c.toolsDir, a)})
	}

	for _, tg := range targets {
		c.checkTool(tg.label, tg.dir)
	}
	if all {
		c.emit("(repository)", "G4", "static", c.repositoryPython())
	}

	fmt.Fprintf(env.Stdout, "check: %d tools: %d passed, %d failed, %d incomplete; gates: %d passed, %d warned, %d failed, %d skipped\n",
		c.tools, c.toolsPassed, c.toolsFailed, c.toolsIncomplete,
		c.gates[statusPass], c.gates[statusWarn], c.gates[statusFail], c.gates[statusSkip])
	switch {
	case c.envFailure:
		return cli.ExitEnv
	case c.failure:
		return cli.ExitNegative
	}
	return cli.ExitOK
}

func (c *checker) emit(label, id, name string, r gateResult) {
	c.gates[r.status]++
	if r.env {
		c.envFailure = true
	}
	if r.status == statusFail || r.unavailable {
		c.failure = true
	}
	line := fmt.Sprintf("%-7s %-14s %s", r.status, id+" "+name, label)
	if r.reason != "" {
		line += ": " + r.reason
	}
	fmt.Fprintln(c.env.Stdout, line)
}

// checkTool runs G1 to G6 on one tool directory and prints one line per gate.
func (c *checker) checkTool(label, dir string) {
	c.tools++
	failed, incomplete := false, false
	t, r := c.manifest(dir)
	c.emit(label, "G1", "manifest", r)
	stoppedBy := ""
	if r.stops() {
		failed, stoppedBy = true, "G1"
	}
	for _, g := range later {
		if stoppedBy != "" {
			c.emit(label, g.id, g.name, skippedf("not run: %s failed", stoppedBy))
			continue
		}
		r := g.run(c, t)
		c.emit(label, g.id, g.name, r)
		if r.unavailable {
			incomplete = true
		}
		if r.stops() {
			failed, stoppedBy = true, g.id
		}
	}
	switch {
	case failed:
		c.toolsFailed++
	case incomplete:
		c.toolsIncomplete++
	default:
		c.toolsPassed++
	}
}

// G1: the manifest loads and validates, and the name does not collide on PATH.
func (c *checker) manifest(dir string) (*manifest.Tool, gateResult) {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil, failf("no tool directory %s", dir)
	}
	t, err := manifest.Load(dir)
	if err != nil {
		return nil, failf("%v", err)
	}
	if hit := pathCollision(t.Name, c.toolsDir); hit != "" {
		return nil, failf("name %q collides with %s on PATH; rename the tool", t.Name, hit)
	}
	return t, pass()
}

func (c *checker) index() (searcher, error) {
	if !c.ixLoaded {
		c.ixLoaded = true
		c.ix, c.ixErr = loadIndex(c.env.Getenv)
	}
	return c.ix, c.ixErr
}

// G2: no other registry entry matches unless extends or distinct_from names it.
func (c *checker) duplicate(t *manifest.Tool) gateResult {
	ix, err := c.index()
	if err != nil {
		return envFailf("load registry: %v", err)
	}
	covered := map[string]bool{}
	if t.Extends != "" {
		covered[t.Extends] = true
	}
	for _, d := range t.DistinctFrom {
		covered[d.Tool] = true
	}
	// A declaration made by the other tool covers the pair too, so one side declaring is enough and a
	// growing library cannot flip a pair that was already reviewed.
	for name := range c.reverseDeclared(t.Name) {
		covered[name] = true
	}
	query := strings.Join(append([]string{t.Name, t.Summary}, t.Intents...), " ")
	hits := duplicates(ix, query, t.Name, covered, true)
	if len(hits) == 0 {
		return pass()
	}
	var names []string
	for i, h := range hits {
		if i == 3 {
			names = append(names, fmt.Sprintf("and %d more", len(hits)-3))
			break
		}
		names = append(names, fmt.Sprintf("%s (%s, score %.2f)", h.Name, h.Kind, h.Score))
	}
	return failf("matches %s; extend that tool, or declare extends or distinct_from naming it", strings.Join(names, ", "))
}

// G3: exit codes 0 and 2 are documented and --help follows the contract. A promoted tool may keep
// its established usage code (tool contract), so it needs 0 plus at least one other documented code.
func (c *checker) iface(t *manifest.Tool) gateResult {
	if _, ok := t.ExitCodes["0"]; !ok {
		return failf("exit_codes must document 0 (success)")
	}
	if _, ok := t.ExitCodes["2"]; !ok && t.Origin.PromotedFrom == "" {
		return failf("exit_codes must document 2 (usage error); only a promoted tool may keep another usage code")
	}
	entry := t.EntryPath()
	fi, err := os.Stat(entry)
	if err != nil {
		return failf("entry %s: %v", t.Entry, err)
	}
	if !fi.Mode().IsRegular() || fi.Mode()&0o111 == 0 {
		return failf("entry %s is not an executable file", t.Entry)
	}
	r := runProc(t.Dir, helpTimeout, false, entry, "--help")
	switch {
	case r.startErr != nil:
		return failf("run %s --help: %v", t.Entry, r.startErr)
	case r.timedOut:
		return failf("%s --help did not finish within %s", t.Entry, helpTimeout)
	case r.code != 0:
		return failf("%s --help exited %d: %s", t.Entry, r.code, lastLine(r.stderr))
	}
	if !strings.HasPrefix(strings.TrimLeft(r.stdout, " \t\r\n"), "Usage:") {
		return failf("%s --help output does not start with \"Usage:\"", t.Entry)
	}
	if t.DryRun != "" && !strings.Contains(r.stdout, t.DryRun) {
		return failf("%s --help does not document the dry_run flag %s", t.Entry, t.DryRun)
	}
	return pass()
}

// G4: no Python, no em or en dash, the language's static check, then gitleaks when installed.
func (c *checker) static(t *manifest.Tool) gateResult {
	if py := walkPython(t.Dir); len(py) > 0 {
		return failf("Python file in the tool: %s", strings.Join(relAll(t.Dir, py), ", "))
	}
	if py := c.trackedPython(); len(py) > 0 {
		return failf("the repository tracks Python files: %s", listSome(py))
	}
	if bad := walkDashes(t.Dir); len(bad) > 0 {
		return failf("em or en dash in %s; use - , ; : or parentheses", strings.Join(relAll(t.Dir, bad), ", "))
	}

	var missing []string
	switch t.Language {
	case "bash":
	case "go":
		if _, err := exec.LookPath("go"); err != nil {
			missing = append(missing, "go")
		} else if r := runProc(t.Dir, staticTimeout, true, "go", "vet", "."); r.code != 0 {
			return failf("go vet: %s", firstFinding(r.stderr, r.startErr, r.timedOut))
		}
	default:
		return failf("no static check for language %q in agent-dry; build the tool in bash or go", t.Language)
	}
	if scripts := shellScripts(t); len(scripts) > 0 {
		if _, err := exec.LookPath("shellcheck"); err != nil {
			missing = append(missing, "shellcheck")
		} else if r := runProc(t.Dir, staticTimeout, false, "shellcheck", append([]string{"-x", "-f", "gcc"}, scripts...)...); r.code != 0 {
			return failf("shellcheck: %s", firstFinding(r.stdout+r.stderr, r.startErr, r.timedOut))
		}
	}
	if len(missing) > 0 && !c.skipUnavailable {
		return envFailf("%s not installed; install it, or pass --skip-unavailable to run the remaining gates", strings.Join(missing, " and "))
	}

	note := ""
	if _, err := exec.LookPath("gitleaks"); err != nil {
		note = "gitleaks not installed; secret scan not run"
	} else if r := runProc(t.Dir, staticTimeout, true, "gitleaks", "dir", "--no-banner", "--redact", "--no-color", t.Dir); r.code != 0 {
		if r.timedOut || r.startErr != nil {
			return failf("gitleaks did not complete: %s", firstFinding("", r.startErr, r.timedOut))
		}
		return failf("gitleaks reported findings (exit %d); inspect with: gitleaks dir --redact %s", r.code, t.Dir)
	}
	if len(missing) > 0 {
		r := skippedf("%s not installed; the other static checks passed", strings.Join(missing, " and "))
		r.unavailable = true
		return r
	}
	return pass().withNote(note)
}

// G5: the manifest test command passes in the tool directory.
func (c *checker) behavior(t *manifest.Tool) gateResult {
	r := runProc(t.Dir, testTimeout, true, "bash", "-c", t.Test)
	switch {
	case r.startErr != nil:
		return envFailf("run test %q: %v", t.Test, r.startErr)
	case r.timedOut:
		return failf("test %q did not finish within %s", t.Test, testTimeout)
	case r.code != 0:
		return failf("test %q exited %d: %s", t.Test, r.code, lastLine(r.stdout))
	}
	return pass()
}

var (
	shellNetwork = regexp.MustCompile("(^|[\\s;|&(`])(ssh|curl|gh|wget)([\\s;|&)`]|$)")
	goNetwork    = regexp.MustCompile(`"(ssh|curl|gh|wget)"`)
)

// G6: origin is filled; network commands under side_effects read are flagged.
func (c *checker) provenance(t *manifest.Tool) gateResult {
	for _, f := range []struct{ key, val string }{{"date", t.Origin.Date}, {"builder", t.Origin.Builder}, {"task", t.Origin.Task}} {
		if strings.TrimSpace(f.val) == "" {
			return failf("origin.%s is blank", f.key)
		}
	}
	if _, err := time.Parse("2006-01-02", t.Origin.Date); err != nil {
		return failf("origin.date %q is not a calendar date", t.Origin.Date)
	}
	if t.SideEffects != "read" {
		return pass()
	}
	if cmds := networkCalls(t); len(cmds) > 0 {
		return warnf("entry calls %s while side_effects is read; declare remote-read or higher if it reaches the network", strings.Join(cmds, ", "))
	}
	return pass()
}

// networkCalls lists the network commands the entry (or a Go tool's non-test sources) invokes.
func networkCalls(t *manifest.Tool) []string {
	found := map[string]bool{}
	scan := func(path string, re *regexp.Regexp, comment string) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, comment) || strings.HasPrefix(line, "#!") {
				continue
			}
			for _, m := range re.FindAllStringSubmatch(line, -1) {
				found[m[len(m)-2]] = true
			}
		}
	}
	if t.Language == "go" {
		matches, _ := filepath.Glob(filepath.Join(t.Dir, "*.go"))
		for _, m := range matches {
			if !strings.HasSuffix(m, "_test.go") {
				scan(m, goNetwork, "//")
			}
		}
	}
	if isShellScript(t.EntryPath()) {
		scan(t.EntryPath(), shellNetwork, "#")
	}
	out := make([]string, 0, len(found))
	for k := range found {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// repositoryPython is the repository-wide no-Python gate that --all adds.
func (c *checker) repositoryPython() gateResult {
	if py := c.trackedPython(); len(py) > 0 {
		return failf("Python files: %s", listSome(py))
	}
	return pass().withNote("no Python files")
}

// trackedPython lists Python files tracked in the repository holding root, or present under
// root when it is not a Git work tree (an installed bundle).
func (c *checker) trackedPython() []string {
	if c.repoPyLoaded {
		return c.repoPy
	}
	c.repoPyLoaded = true
	files, err := gitLsFiles(c.root)
	if err != nil {
		c.repoPy = relAll(c.root, walkPython(c.root))
		return c.repoPy
	}
	for _, f := range files {
		if isPython(filepath.Join(c.root, f)) {
			c.repoPy = append(c.repoPy, f)
		}
	}
	return c.repoPy
}

func gitLsFiles(dir string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), staticTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "ls-files", "-z")
	cmd.Env = childEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// isPython reports a .py, .pyw or .pyi file, or a file whose shebang names python.
func isPython(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".py", ".pyw", ".pyi":
		return true
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 128)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	if !bytes.HasPrefix(head, []byte("#!")) {
		return false
	}
	if i := bytes.IndexByte(head, '\n'); i >= 0 {
		head = head[:i]
	}
	return bytes.Contains(head, []byte("python"))
}

func walkFiles(dir string, fn func(path string)) {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() {
			fn(p)
		}
		return nil
	})
}

func walkPython(dir string) []string {
	var out []string
	walkFiles(dir, func(p string) {
		if isPython(p) {
			out = append(out, p)
		}
	})
	return out
}

// walkDashes lists text files under dir containing an em or en dash; binary files (a NUL byte in
// the first 8000 bytes, as grep -I decides) are skipped.
func walkDashes(dir string) []string {
	em, en := []byte(emDash), []byte(enDash)
	var out []string
	walkFiles(dir, func(p string) {
		raw, err := os.ReadFile(p)
		if err != nil {
			return
		}
		if bytes.IndexByte(raw[:min(len(raw), 8000)], 0) >= 0 {
			return
		}
		if bytes.Contains(raw, em) || bytes.Contains(raw, en) {
			out = append(out, p)
		}
	})
	return out
}

// shellScripts lists the Bash files shellcheck covers, relative to the tool directory: the
// entry when it is a shell script, and every *.sh file at the top of the tool directory.
func shellScripts(t *manifest.Tool) []string {
	var out []string
	if isShellScript(t.EntryPath()) {
		out = append(out, t.Entry)
	}
	matches, _ := filepath.Glob(filepath.Join(t.Dir, "*.sh"))
	for _, m := range matches {
		if rel, err := filepath.Rel(t.Dir, m); err == nil && rel != t.Entry {
			out = append(out, rel)
		}
	}
	return out
}

func isShellScript(path string) bool {
	if strings.HasSuffix(path, ".sh") {
		return true
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 64)
	n, _ := io.ReadFull(f, head)
	line := string(head[:n])
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return strings.HasPrefix(line, "#!") && (strings.Contains(line, "bash") || strings.HasSuffix(strings.TrimSpace(line), "/sh") || strings.HasSuffix(strings.TrimSpace(line), " sh"))
}

// firstFinding is the first line of checker output that is not a package header.
func firstFinding(out string, startErr error, timedOut bool) string {
	switch {
	case startErr != nil:
		return startErr.Error()
	case timedOut:
		return fmt.Sprintf("did not finish within %s", staticTimeout)
	}
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			if len(l) > 200 {
				l = l[:197] + "..."
			}
			return l
		}
	}
	return "failed without output"
}

func relAll(base string, ps []string) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		if r, err := filepath.Rel(base, p); err == nil {
			p = r
		}
		out = append(out, p)
	}
	return out
}

func listSome(ps []string) string {
	if len(ps) > 5 {
		return strings.Join(ps[:5], ", ") + fmt.Sprintf(" and %d more", len(ps)-5)
	}
	return strings.Join(ps, ", ")
}

// reverseDeclared returns the tools whose manifests name tool in extends or distinct_from.
// Unreadable manifests are skipped here; G1 reports them.
func (c *checker) reverseDeclared(tool string) map[string]bool {
	out := map[string]bool{}
	others, _ := manifest.LoadAll(c.toolsDir)
	for _, o := range others {
		if o.Name == tool {
			continue
		}
		if o.Extends == tool {
			out[o.Name] = true
		}
		for _, d := range o.DistinctFrom {
			if d.Tool == tool {
				out[o.Name] = true
			}
		}
	}
	return out
}
