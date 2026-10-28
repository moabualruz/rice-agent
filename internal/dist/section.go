package dist

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/moabualruz/rice-agent/internal/cli"
	"github.com/moabualruz/rice-agent/internal/paths"
)

const sectionUsage = `Usage: rice-agent section install --name <NAME> --source <file> [--destination <home>] [--project-root <dir>]... [--dry-run]
       rice-agent section check --name <NAME> --source <file> [--destination <home>] [--json]

install writes the managed block <!-- NAME_START --> ... <!-- NAME_END --> from the source into
every harness instruction file under the destination home, preserving all other content:
  ~/.config/agent-instructions/shared/KERNEL.md and shared/AGENTS.md (created when absent),
  every ~/.config/agent-instructions/compiled/*.md and *.mdc,
  ~/.claude/CLAUDE.md, ~/.codex/AGENTS.md, ~/AGENTS.md, ~/.config/opencode/AGENTS.md,
  ~/.gemini/GEMINI.md, ~/.copilot/copilot-instructions.md, ~/.hermes/SOUL.md (created when absent),
  ~/AGENTS.override.md, ~/.codex/AGENTS.override.md, ~/.grok/AGENTS.md, ~/.pi/agent/AGENTS.md,
  ~/.hermes/AGENTS.md, ~/.hermes/HERMES.md, ~/.cursor/rules/agent-instructions.mdc (only when present),
  and with --project-root, the Cursor project rule <root>/.cursor/rules/<name>.mdc.
Symlinks are resolved so each real file is written once and links survive. An existing block is
replaced in place; a missing block is inserted at the top, after any YAML front matter. A file
holding the Rice Agent or legacy agent-dry wrapper line is an import wrapper or loader that gets every
section through the kernel: install removes the block from it and never inserts one. Every file is
checked before any is written: unequal or
duplicate markers, or a path escaping the destination, refuse the whole run. Writes are atomic,
preserve mode and are read back. A JSON receipt with the source sha256 and every file's action
(inserted, updated, removed, unchanged or deferred) and sha256 goes to stdout.

check verifies every existing target holds exactly one block byte-identical to the source with
no em or en dash, and that every file with the wrapper line holds none.

Options:
  --name <NAME>          section name, upper snake case, for example DRY_POLICY
  --source <file>        block source; must start and end with the section markers
  --destination <home>   home directory to install into (default $HOME)
  --project-root <dir>   Git worktree root that gets a Cursor project rule (repeatable)
  --project-rule <file>  project rule file name (default <name in kebab case>.mdc)
  --dry-run              print the receipt of what would change and change nothing
  --json                 (check) print results as JSON

Exit codes: 0 installed or parity holds, 1 refused or parity failed, 2 usage or invalid source,
3 environment (unreadable source or destination, write or verification failure).

Example: rice-agent section install --name DRY_POLICY --source policy/dry-policy.md --dry-run
`

// alwaysTargets are created when absent; existingTargets are written only when present.
var (
	sharedTargets   = []string{"shared/KERNEL.md", "shared/AGENTS.md"}
	alwaysTargets   = []string{"AGENTS.md", ".codex/AGENTS.md", ".claude/CLAUDE.md", ".config/opencode/AGENTS.md", ".gemini/GEMINI.md", ".copilot/copilot-instructions.md", ".hermes/SOUL.md"}
	existingTargets = []string{"AGENTS.override.md", ".codex/AGENTS.override.md", ".grok/AGENTS.md", ".pi/agent/AGENTS.md", ".hermes/AGENTS.md", ".hermes/HERMES.md", ".cursor/rules/agent-instructions.mdc"}
)

// deferLine marks a file that loads every managed section from shared/KERNEL.md (an import
// wrapper or a loader such as the Codex home override), so it must not carry a copy.
const (
	deferLine       = "<!-- rice-agent: managed sections load from shared/KERNEL.md -->"
	legacyDeferLine = "<!-- agent-dry: managed sections load from shared/KERNEL.md -->"
)

// defers reports whether content holds a wrapper marker as a whole line.
func defers(content string) bool {
	for _, line := range lineBreak.Split(content, -1) {
		line = strings.TrimSpace(line)
		if line == deferLine || line == legacyDeferLine {
			return true
		}
	}
	return false
}

const mdcHeader = "---\ndescription: Shared owner instructions\nalwaysApply: true\n---\n\n"

var (
	sectionNamePattern = regexp.MustCompile(`^[A-Z][A-Z0-9]*(_[A-Z0-9]+)*$`)
	frontMatter        = regexp.MustCompile(`^---\r?\n(?s:.*?)\r?\n---\r?\n`)
	mdcStart           = regexp.MustCompile(`^---\r?\n`)
	projectFront       = regexp.MustCompile(`^---\r?\n((?s:.*?))\r?\n---(?:\r?\n|$)`)
	alwaysApplyKey     = regexp.MustCompile(`^alwaysApply\s*:`)
	alwaysApplyTrue    = regexp.MustCompile(`^alwaysApply:\s*true\s*$`)
	lineBreak          = regexp.MustCompile(`\r?\n`)
)

type section struct {
	name, start, end string
	source           string // trimmed block
	sourcePath       string
}

// sectionTarget is one real file to write.
type sectionTarget struct {
	path     string   // resolved real path
	entries  []string // entry paths (home or project relative) that resolve here
	exists   bool
	dangling bool    // an entry is a symlink whose destination is missing
	project  string  // set for a Cursor project rule
	original *string // project rule content at preflight
}

// SectionResult is one file in the receipt.
type SectionResult struct {
	Path    string   `json:"path"`
	Entries []string `json:"entries,omitempty"`
	Action  string   `json:"action"` // inserted, updated, removed, unchanged, deferred
	SHA256  string   `json:"sha256"`
}

type sectionEntry struct {
	Path   string `json:"path"`
	Target string `json:"target"`
}

type projectRuleReceipt struct {
	Project     string `json:"project"`
	Path        string `json:"path"`
	Harness     string `json:"harness"`
	Scope       string `json:"scope"`
	AlwaysApply bool   `json:"always_apply"`
}

type sectionReceipt struct {
	Name         string               `json:"name"`
	Source       string               `json:"source"`
	SourceSHA256 string               `json:"source_sha256"`
	Destination  string               `json:"destination"`
	DryRun       bool                 `json:"dry_run,omitempty"`
	Targets      []SectionResult      `json:"targets"`
	Entries      []sectionEntry       `json:"entries"`
	ProjectRules []projectRuleReceipt `json:"project_rules"`
}

func runSection(env *cli.Env) int {
	if len(env.Args) == 0 {
		return cli.Usagef(env, "section: need install or check; run rice-agent section --help")
	}
	sub := env.Args[0]
	switch sub {
	case "-h", "--help", "help":
		fmt.Fprint(env.Stdout, sectionUsage)
		return cli.ExitOK
	case "install", "check":
	default:
		return cli.Usagef(env, "section: unknown subcommand %q; run rice-agent section --help", sub)
	}
	fs := flag.NewFlagSet("section", flag.ContinueOnError)
	name := fs.String("name", "", "")
	source := fs.String("source", "", "")
	destination := fs.String("destination", "", "")
	var projects stringList
	fs.Var(&projects, "project-root", "")
	rule := fs.String("project-rule", "", "")
	dryRun := fs.Bool("dry-run", false, "")
	asJSON := fs.Bool("json", false, "")
	sub2 := *env
	sub2.Args = env.Args[1:]
	pos, code, ok := parseArgs(&sub2, fs, sectionUsage)
	if !ok {
		return code
	}
	if len(pos) > 0 {
		return cli.Usagef(env, "section: unexpected argument %q", pos[0])
	}
	if *name == "" || *source == "" {
		return cli.Usagef(env, "section %s: --name and --source are required", sub)
	}
	if sub == "check" && (len(projects) > 0 || *rule != "" || *dryRun) {
		return cli.Usagef(env, "section check: takes --name, --source, --destination and --json only")
	}
	if sub == "install" && *asJSON {
		return cli.Usagef(env, "section install: the receipt is always JSON; drop --json")
	}
	home := paths.Home(env.Getenv)
	srcPath, err := absPath(*source, home)
	if err != nil {
		return cli.Usagef(env, "section: --source: %v", err)
	}
	sec, code := loadSection(env, *name, srcPath, sub == "install")
	if sec == nil {
		return code
	}
	dest := home
	if *destination != "" {
		if dest, err = absPath(*destination, home); err != nil {
			return cli.Usagef(env, "section: --destination: %v", err)
		}
	}
	realDest, err := filepath.EvalSymlinks(dest)
	if err != nil {
		return cli.Envf(env, "section: destination: %v", err)
	}
	if info, err := os.Stat(realDest); err != nil || !info.IsDir() {
		return cli.Envf(env, "section: destination %s is not a directory", dest)
	}
	if sub == "check" {
		return sectionCheck(env, sec, realDest, *asJSON)
	}
	ruleName := *rule
	if ruleName == "" {
		ruleName = strings.ReplaceAll(strings.ToLower(sec.name), "_", "-") + ".mdc"
	}
	if strings.ContainsAny(ruleName, `/\`) || !strings.HasSuffix(ruleName, ".mdc") || strings.HasPrefix(ruleName, ".") {
		return cli.Usagef(env, "section: --project-rule must be a plain <name>.mdc file name")
	}
	return sectionInstall(env, sec, realDest, projects, ruleName, *dryRun)
}

func loadSection(env *cli.Env, name, srcPath string, forInstall bool) (*section, int) {
	if !sectionNamePattern.MatchString(name) {
		return nil, cli.Usagef(env, "section: --name %q must be upper snake case, for example DRY_POLICY", name)
	}
	raw, err := os.ReadFile(srcPath)
	if err != nil {
		return nil, cli.Envf(env, "section: source: %v", err)
	}
	s := &section{name: name, start: "<!-- " + name + "_START -->", end: "<!-- " + name + "_END -->", sourcePath: srcPath}
	s.source = strings.TrimSpace(string(raw))
	if !strings.HasPrefix(s.source, s.start) || !strings.HasSuffix(s.source, s.end) ||
		strings.Count(s.source, s.start) != 1 || strings.Count(s.source, s.end) != 1 {
		return nil, cli.Usagef(env, "section: source %s must start with %s and end with %s, once each", srcPath, s.start, s.end)
	}
	if forInstall && hasDash(s.source) {
		fmt.Fprintf(env.Stderr, "rice-agent: section: source %s contains an em or en dash; nothing changed\n", srcPath)
		return nil, cli.ExitNegative
	}
	return s, cli.ExitOK
}

// resolveTarget follows symlinks (including dangling ones) to the real path a write would land
// on. For a missing file it resolves the deepest existing ancestor.
func resolveTarget(p string) (real string, exists, dangling bool, err error) {
	for hops := 0; hops < 40; hops++ {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r, true, dangling, nil
		}
		info, lerr := os.Lstat(p)
		if lerr == nil && info.Mode()&fs.ModeSymlink != 0 {
			dest, err := os.Readlink(p)
			if err != nil {
				return "", false, false, err
			}
			if !filepath.IsAbs(dest) {
				dest = filepath.Join(filepath.Dir(p), dest)
			}
			p, dangling = filepath.Clean(dest), true
			continue
		}
		if lerr != nil && !errors.Is(lerr, fs.ErrNotExist) {
			return "", false, false, lerr
		}
		dir, tail := filepath.Dir(p), []string{filepath.Base(p)}
		for {
			if r, err := filepath.EvalSymlinks(dir); err == nil {
				return filepath.Join(append([]string{r}, tail...)...), false, dangling, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				return filepath.Clean(p), false, dangling, nil
			}
			tail = append([]string{filepath.Base(dir)}, tail...)
			dir = parent
		}
	}
	return "", false, false, fmt.Errorf("%s: too many levels of symbolic links", p)
}

// discoverTargets lists the real files the section belongs in, in the reference order. With
// create false (check), only existing files and dangling entries are returned.
func discoverTargets(home string, create bool) ([]*sectionTarget, []sectionEntry, error) {
	var order []*sectionTarget
	byPath := map[string]*sectionTarget{}
	// always: install creates the file when absent (following a dangling symlink so the link
	// survives); check includes it when it exists or dangles. Otherwise only an existing file counts.
	add := func(entry string, always bool) error {
		real, exists, dangling, err := resolveTarget(entry)
		if err != nil {
			return err
		}
		if !exists && (!always || (!create && !dangling)) {
			return nil
		}
		if exists {
			info, err := os.Stat(real)
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("instruction target %s is not a regular file", entry)
			}
		}
		t := byPath[real]
		if t == nil {
			t = &sectionTarget{path: real, exists: exists}
			byPath[real] = t
			order = append(order, t)
		}
		t.entries = append(t.entries, entry)
		t.dangling = t.dangling || dangling
		return nil
	}
	cfg := filepath.Join(home, ".config", "agent-instructions")
	for _, p := range sharedTargets {
		if err := add(filepath.Join(cfg, filepath.FromSlash(p)), true); err != nil {
			return nil, nil, err
		}
	}
	compiled := filepath.Join(cfg, "compiled")
	if entries, err := os.ReadDir(compiled); err == nil {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		sort.Strings(names)
		for _, n := range names {
			if !strings.HasSuffix(n, ".md") && !strings.HasSuffix(n, ".mdc") {
				continue
			}
			p := filepath.Join(compiled, n)
			if info, err := os.Stat(p); err != nil || !info.Mode().IsRegular() {
				continue
			}
			if err := add(p, false); err != nil {
				return nil, nil, err
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, err
	}
	var entries []sectionEntry
	for _, p := range alwaysTargets {
		full := filepath.Join(home, filepath.FromSlash(p))
		if err := add(full, true); err != nil {
			return nil, nil, err
		}
		real, _, _, _ := resolveTarget(full)
		entries = append(entries, sectionEntry{Path: full, Target: real})
	}
	for _, p := range existingTargets {
		if err := add(filepath.Join(home, filepath.FromSlash(p)), false); err != nil {
			return nil, nil, err
		}
	}
	for _, t := range order {
		if !within(home, t.path) {
			return nil, nil, fmt.Errorf("instruction target escapes destination: %s -> %s", strings.Join(t.entries, ", "), t.path)
		}
	}
	return order, entries, nil
}

// preflightProject mirrors the reference's Cursor project rule refusals.
func preflightProject(env *cli.Env, requested, ruleName, start string) (*sectionTarget, error) {
	abs, err := absPath(requested, paths.Home(env.Getenv))
	if err != nil {
		return nil, err
	}
	project, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("project root must be the actual Git worktree root: %v", err)
	}
	git, ok := lookPath("git", env.Getenv("PATH"))
	if !ok {
		return nil, errors.New("project root must be the actual Git worktree root: git not found")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, git, "-C", project, "rev-parse", "--show-toplevel")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil || realOrSelf(strings.TrimSpace(string(out))) != project {
		return nil, errors.New("project root must be the actual Git worktree root")
	}
	rule := filepath.Join(project, ".cursor", "rules", ruleName)
	for cur := rule; cur != project; cur = filepath.Dir(cur) {
		info, err := os.Lstat(cur)
		if err == nil && info.Mode()&fs.ModeSymlink != 0 {
			return nil, errors.New("project policy path is a symlink")
		}
	}
	t := &sectionTarget{path: rule, project: project, entries: []string{rule}}
	info, err := os.Lstat(rule)
	if err == nil {
		if !info.Mode().IsRegular() || info.Size() > 1<<20 {
			return nil, errors.New("project policy must be a bounded regular file")
		}
		raw, err := os.ReadFile(rule)
		if err != nil {
			return nil, err
		}
		s := string(raw)
		t.original, t.exists = &s, true
		var applyLines []string
		if g := projectFront.FindStringSubmatch(s); g != nil {
			for _, line := range lineBreak.Split(g[1], -1) {
				if alwaysApplyKey.MatchString(line) {
					applyLines = append(applyLines, line)
				}
			}
		}
		if !strings.Contains(s, start) || len(applyLines) != 1 || !alwaysApplyTrue.MatchString(applyLines[0]) {
			return nil, errors.New("existing project policy has another owner or is not always applied")
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return t, nil
}

// blockSpan returns the start and end offsets of the single well-formed block, or ok false with
// counts when the markers are absent (0, 0) or ambiguous.
func blockSpan(content string, s *section) (i, j, starts, ends int, ok bool) {
	starts, ends = strings.Count(content, s.start), strings.Count(content, s.end)
	if starts != 1 || ends != 1 {
		return 0, 0, starts, ends, false
	}
	i, j = strings.Index(content, s.start), strings.Index(content, s.end)
	if j < i {
		return 0, 0, starts, ends, false
	}
	return i, j + len(s.end), starts, ends, true
}

type plannedWrite struct {
	t             *sectionTarget
	before, after string
	action        string
	mode          fs.FileMode
}

func planSection(s *section, t *sectionTarget) (*plannedWrite, error) {
	before := ""
	mode := fs.FileMode(0o644)
	if t.exists {
		raw, err := os.ReadFile(t.path)
		if err != nil {
			return nil, err
		}
		before = string(raw)
		if info, err := os.Stat(t.path); err == nil {
			mode = info.Mode().Perm()
		}
	}
	if t.project != "" && t.original != nil && before != *t.original {
		return nil, errors.New("project policy changed after preflight")
	}
	i, j, starts, ends, ok := blockSpan(before, s)
	var after, action string
	if defers(before) {
		switch {
		case starts == 0 && ends == 0:
			return &plannedWrite{t: t, before: before, after: before, action: "deferred", mode: mode}, nil
		case !ok:
			return nil, fmt.Errorf("ambiguous managed section: %s", t.path)
		}
		// Undo the inserted layout (block and a blank line at the top or after front matter);
		// elsewhere drop the block and its own line break.
		rest := before[j:]
		if top := i == 0 || i == len(frontMatter.FindString(before)); top && strings.HasPrefix(rest, "\n\n") {
			rest = rest[2:]
		} else if strings.HasPrefix(rest, "\r\n") {
			rest = rest[2:]
		} else if strings.HasPrefix(rest, "\n") {
			rest = rest[1:]
		}
		return &plannedWrite{t: t, before: before, after: before[:i] + rest, action: "removed", mode: mode}, nil
	}
	switch {
	case ok:
		after = before[:i] + s.source + before[j:]
		action = "updated"
	case starts == 0 && ends == 0:
		prefix := frontMatter.FindString(before)
		after = prefix + s.source + "\n\n" + before[len(prefix):]
		action = "inserted"
	default:
		return nil, fmt.Errorf("ambiguous managed section: %s", t.path)
	}
	if strings.HasSuffix(t.path, ".mdc") && !mdcStart.MatchString(after) {
		after = mdcHeader + after
	}
	if after == before {
		action = "unchanged"
	}
	return &plannedWrite{t: t, before: before, after: after, action: action, mode: mode}, nil
}

func sectionInstall(env *cli.Env, s *section, home string, projects []string, ruleName string, dryRun bool) int {
	targets, entries, err := discoverTargets(home, true)
	if err != nil {
		fmt.Fprintf(env.Stderr, "rice-agent: section: %v; nothing changed\n", err)
		return cli.ExitNegative
	}
	var rules []projectRuleReceipt
	for _, requested := range projects {
		t, err := preflightProject(env, requested, ruleName, s.start)
		if err != nil {
			fmt.Fprintf(env.Stderr, "rice-agent: section: %v; nothing changed\n", err)
			return cli.ExitNegative
		}
		dup := false
		for _, o := range targets {
			dup = dup || o.path == t.path
		}
		if !dup {
			targets = append(targets, t)
			rules = append(rules, projectRuleReceipt{Project: t.project, Path: t.path, Harness: "cursor", Scope: "project", AlwaysApply: true})
		}
	}
	plans := make([]*plannedWrite, 0, len(targets))
	for _, t := range targets {
		p, err := planSection(s, t)
		if err != nil {
			fmt.Fprintf(env.Stderr, "rice-agent: section: %v; nothing changed\n", err)
			return cli.ExitNegative
		}
		plans = append(plans, p)
	}
	receipt := sectionReceipt{Name: s.name, Source: s.sourcePath, SourceSHA256: sha256Bytes([]byte(s.source)),
		Destination: home, DryRun: dryRun, Targets: []SectionResult{}, Entries: entries, ProjectRules: []projectRuleReceipt{}}
	receipt.ProjectRules = append(receipt.ProjectRules, rules...)
	for _, p := range plans {
		want := 1 // copies of the block the file must hold afterwards
		if p.action == "removed" || p.action == "deferred" {
			want = 0
		}
		if !dryRun && p.action != "unchanged" && p.action != "deferred" {
			if p.t.project != "" {
				raw, err := os.ReadFile(p.t.path)
				now := string(raw)
				if (err == nil) != p.t.exists || (err == nil && now != p.before) {
					fmt.Fprintf(env.Stderr, "rice-agent: section: project policy changed after preflight: %s\n", p.t.path)
					return cli.ExitNegative
				}
			}
			if err := os.MkdirAll(filepath.Dir(p.t.path), 0o755); err != nil {
				return cli.Envf(env, "section: %v", err)
			}
			if err := writeFileAtomic(p.t.path, []byte(p.after), p.mode); err != nil {
				return cli.Envf(env, "section: write %s: %v", p.t.path, err)
			}
		}
		content := p.after
		if !dryRun {
			raw, err := os.ReadFile(p.t.path)
			if err != nil || string(raw) != p.after || strings.Count(string(raw), s.source) != want {
				return cli.Envf(env, "section: instruction verification failed: %s", p.t.path)
			}
			content = string(raw)
		}
		receipt.Targets = append(receipt.Targets, SectionResult{Path: p.t.path, Entries: p.t.entries, Action: p.action,
			SHA256: sha256Bytes([]byte(content))})
	}
	if err := writeJSON(env.Stdout, receipt); err != nil {
		return cli.Envf(env, "section: %v", err)
	}
	return cli.ExitOK
}

// SectionCheck is one parity result.
type SectionCheck struct {
	Path        string   `json:"path"`
	Entries     []string `json:"entries"`
	Pass        bool     `json:"pass"`
	Problems    []string `json:"problems,omitempty"`
	BlockSHA256 string   `json:"block_sha256,omitempty"`
	Deferred    bool     `json:"deferred,omitempty"` // loads sections from shared/KERNEL.md
}

type sectionCheckReport struct {
	Name         string         `json:"name"`
	SourceSHA256 string         `json:"source_sha256"`
	Destination  string         `json:"destination"`
	Pass         bool           `json:"pass"`
	Files        []SectionCheck `json:"files"`
}

func sectionCheck(env *cli.Env, s *section, home string, asJSON bool) int {
	targets, _, err := discoverTargets(home, false)
	if err != nil {
		fmt.Fprintf(env.Stderr, "rice-agent: section: %v\n", err)
		return cli.ExitNegative
	}
	rep := sectionCheckReport{Name: s.name, SourceSHA256: sha256Bytes([]byte(s.source)), Destination: home, Pass: true}
	if hasDash(s.source) {
		rep.Pass = false
		fmt.Fprintf(env.Stderr, "rice-agent: section: source %s contains an em or en dash\n", s.sourcePath)
	}
	for _, t := range targets {
		c := SectionCheck{Path: t.path, Entries: t.entries}
		if !t.exists {
			c.Problems = append(c.Problems, "dangling symlink")
		} else if raw, err := os.ReadFile(t.path); err != nil {
			c.Problems = append(c.Problems, "unreadable: "+err.Error())
		} else if content := string(raw); defers(content) {
			c.Deferred = true
			if _, _, starts, ends, _ := blockSpan(content, s); starts != 0 || ends != 0 {
				c.Problems = append(c.Problems, "loads sections from shared/KERNEL.md but carries a copy of the block")
			}
		} else {
			i, j, starts, ends, ok := blockSpan(content, s)
			if !ok && starts == 1 && ends == 1 {
				c.Problems = append(c.Problems, "end marker precedes start marker")
			} else if !ok {
				c.Problems = append(c.Problems, fmt.Sprintf("want exactly one block, found %d start and %d end marker(s)", starts, ends))
			} else {
				block := content[i:j]
				c.BlockSHA256 = sha256Bytes([]byte(block))
				if block != s.source {
					c.Problems = append(c.Problems, "block differs from source")
				}
				if hasDash(block) {
					c.Problems = append(c.Problems, "block contains an em or en dash")
				}
			}
		}
		c.Pass = len(c.Problems) == 0
		rep.Pass = rep.Pass && c.Pass
		rep.Files = append(rep.Files, c)
	}
	if len(targets) == 0 {
		rep.Pass = false
	}
	if asJSON {
		if err := writeJSON(env.Stdout, rep); err != nil {
			return cli.Envf(env, "section: %v", err)
		}
	} else {
		fmt.Fprintf(env.Stdout, "section %s: source sha256 %s\n", s.name, rep.SourceSHA256)
		for _, c := range rep.Files {
			if c.Pass && c.Deferred {
				fmt.Fprintf(env.Stdout, "PASS %s (loads it from shared/KERNEL.md)\n", c.Path)
			} else if c.Pass {
				fmt.Fprintf(env.Stdout, "PASS %s\n", c.Path)
			} else {
				fmt.Fprintf(env.Stdout, "FAIL %s: %s\n", c.Path, strings.Join(c.Problems, "; "))
			}
		}
		if len(targets) == 0 {
			fmt.Fprintf(env.Stdout, "FAIL no instruction files under %s\n", home)
		}
	}
	if !rep.Pass {
		return cli.ExitNegative
	}
	return cli.ExitOK
}
