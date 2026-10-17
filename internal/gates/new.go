package gates

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/moabualruz/agent-dry/internal/cli"
	"github.com/moabualruz/agent-dry/internal/manifest"
	"github.com/moabualruz/agent-dry/internal/paths"
	"github.com/moabualruz/agent-dry/internal/registry"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

var templates = template.Must(template.New("").
	Funcs(template.FuncMap{"yaml": strconv.Quote}).
	ParseFS(templateFS, "templates/*.tmpl"))

// now is replaced in tests.
var now = time.Now

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// ProjectList is the project-scope command list that find reads from the Git root: a JSON array
// of {name, summary, intents, entry, kind} with entry relative to the Git root.
const ProjectList = ".agent-dry.json"

// projectEntry is one ProjectList item.
type projectEntry struct {
	Name    string   `json:"name"`
	Summary string   `json:"summary"`
	Intents []string `json:"intents"`
	Entry   string   `json:"entry"`
	Kind    string   `json:"kind,omitempty"`
}

const newUsage = `Usage: agent-dry new <name> --kind script|cli|skill --scope global|project --task <ref> [flags]

Run the duplicate check (gate G2, against this repository's own tools and skills), then the reuse
advisory, then scaffold a tool that already passes its tests and agent-dry check. The advisory
prints on stderr, under "consider reusing before building:", up to 3 installed skills, project
commands and external CLIs that may already cover the job; it never blocks or changes the exit
code, and it says nothing when none match or the full registry does not load. Global tools go to
<root>/tools/<name>/; project tools to <git root>/scripts/, listed in <git root>/.agent-dry.json;
skills to skills/<name>/SKILL.md (project scope: .agents/skills/<name>/SKILL.md). Existing files
are never overwritten.

Flags:
  --kind script|cli|skill   what to build (required)
  --scope global|project    where it lives (required)
  --task <ref>              motivating task reference, recorded in origin (required)
  --language bash|go        default bash for script, go for cli
  --summary "Use when ..."  trigger-first summary, at most 80 characters
  --intent <phrase>         a phrase an agent would type to find it; repeat, 3 to 12
                            (padded from the name when fewer than 3 are given)
  --side-effects <level>    read (default), write-local, remote-read, remote-write, destructive;
                            anything above read adds --dry-run
  --extends <tool>          existing tool this one extends; clears a G2 match on it
  --distinct-from <tool>    similar existing tool this one stays apart from; needs --reason
  --reason <text>           why it is distinct, at least 10 characters
  --dry-run                 print the plan and write nothing
  --no-advisory             skip the reuse advisory (for tests and scripts)

Environment: AGENT_DRY_BUILDER names the builder model recorded in origin (default "unknown").

Exit codes: 0 created or planned, 1 refused (duplicate, PATH collision or existing file),
2 usage error, 3 environment (no agent-dry root, no Git repository, unreadable registry).

Example:
  agent-dry new repo-state --kind script --scope global --task rice-kb:issues/11 \
    --summary "Use when you need branch, HEAD and dirty state in one block." \
    --intent "git status summary" --intent "repo state" --intent "count dirty files"
`

type newOpts struct {
	name, kind, scope, task, language, summary, sideEffects string
	extends, distinctFrom, reason                           string
	intents                                                 []string
	dryRun, noAdvisory                                      bool
}

// tmplData feeds the templates.
type tmplData struct {
	Name, Summary, DryRunFlag, DryRunName, TestCmd, GoUsage string
	Date, Builder, Task                                     string
	DryRun                                                  bool
}

// planFile is one file new writes.
type planFile struct {
	path string
	mode os.FileMode
	data []byte
}

// newPlan is everything new writes.
type newPlan struct {
	files    []planFile
	listPath string // ProjectList to create or merge into; empty when none
	listData []byte
	next     string
}

func runNew(env *cli.Env) int {
	o, code := parseNew(env)
	if code >= 0 {
		return code
	}

	// G2 first: nothing is resolved or written before the duplicate check.
	ix, err := loadIndex(env.Getenv)
	if err != nil {
		return cli.Envf(env, "G2 duplicate: load registry: %v", err)
	}
	query := strings.Join(append([]string{o.name, o.summary}, o.intents...), " ")
	covered := map[string]bool{}
	if o.extends != "" {
		covered[o.extends] = true
	}
	if o.distinctFrom != "" {
		covered[o.distinctFrom] = true
	}
	all := duplicates(ix, query, "", nil, false)
	if hits := duplicates(ix, query, "", covered, false); len(hits) > 0 {
		fmt.Fprintf(env.Stderr, "agent-dry: G2 duplicate: %d existing entries match %q; reuse or extend one, or pass --extends <tool> or --distinct-from <tool> --reason <text>\n", len(hits), o.name)
		for _, h := range hits {
			fmt.Fprintln(env.Stdout, "  "+registryResultLine(h))
		}
		return cli.ExitNegative
	}
	if len(all) > 0 {
		names := make([]string, 0, len(all))
		for _, h := range all {
			names = append(names, h.Name)
		}
		fmt.Fprintf(env.Stdout, "G2 duplicate: PASS, matches declared through --extends or --distinct-from: %s\n", strings.Join(names, ", "))
	} else {
		fmt.Fprintln(env.Stdout, "G2 duplicate: PASS, no existing entry at or above the threshold")
	}
	if !o.noAdvisory {
		adviseReuse(env, query)
	}

	p, code := planNew(env, o)
	if code >= 0 {
		return code
	}
	var exists []string
	for _, f := range p.files {
		if _, err := os.Lstat(f.path); err == nil {
			exists = append(exists, f.path)
		}
	}
	if len(exists) > 0 {
		fmt.Fprintf(env.Stderr, "agent-dry: refusing to overwrite: %s\n", strings.Join(exists, ", "))
		return cli.ExitNegative
	}

	verb, listVerb := "created", "added"
	if o.dryRun {
		verb, listVerb = "plan: create", "plan: add"
	} else if err := writePlan(p); err != nil {
		return cli.Envf(env, "write scaffold: %v", err)
	}
	for _, f := range p.files {
		fmt.Fprintf(env.Stdout, "%s %s\n", verb, f.path)
	}
	if p.listPath != "" {
		fmt.Fprintf(env.Stdout, "%s entry %q to %s\n", listVerb, o.name, p.listPath)
	}
	if o.dryRun {
		fmt.Fprintln(env.Stdout, "dry run: nothing written")
		return cli.ExitOK
	}
	fmt.Fprintln(env.Stdout, "next: "+p.next)
	return cli.ExitOK
}

// parseNew returns the options, or an exit code (>= 0) when the command is already done.
func parseNew(env *cli.Env) (*newOpts, int) {
	o := &newOpts{}
	var intents stringList
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.kind, "kind", "", "")
	fs.StringVar(&o.scope, "scope", "", "")
	fs.StringVar(&o.task, "task", "", "")
	fs.StringVar(&o.language, "language", "", "")
	fs.StringVar(&o.summary, "summary", "", "")
	fs.Var(&intents, "intent", "")
	fs.StringVar(&o.sideEffects, "side-effects", "read", "")
	fs.StringVar(&o.extends, "extends", "", "")
	fs.StringVar(&o.distinctFrom, "distinct-from", "", "")
	fs.StringVar(&o.reason, "reason", "", "")
	fs.BoolVar(&o.dryRun, "dry-run", false, "")
	fs.BoolVar(&o.noAdvisory, "no-advisory", false, "")
	pos, err := parseInterleaved(fs, env.Args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(env.Stdout, newUsage)
		return nil, cli.ExitOK
	}
	if err != nil {
		return nil, cli.Usagef(env, "new: %v; run agent-dry new --help", err)
	}
	if len(pos) != 1 {
		return nil, cli.Usagef(env, "new: want exactly one <name>, got %d; run agent-dry new --help", len(pos))
	}
	o.name = pos[0]
	if !namePattern.MatchString(o.name) || len(o.name) > 40 {
		return nil, cli.Usagef(env, "new: name %q must be kebab-case, at most 40 characters", o.name)
	}
	switch o.kind {
	case "script", "cli", "skill":
	default:
		return nil, cli.Usagef(env, "new: --kind must be script, cli or skill")
	}
	if o.scope != "global" && o.scope != "project" {
		return nil, cli.Usagef(env, "new: --scope must be global or project")
	}
	if err := checkText(o.task, 3, 200); err != nil {
		return nil, cli.Usagef(env, "new: --task %v", err)
	}
	switch {
	case o.kind == "skill" && o.language != "":
		return nil, cli.Usagef(env, "new: --language does not apply to a skill")
	case o.kind == "skill":
		o.language = "markdown"
	case o.language == "" && o.kind == "cli":
		o.language = "go"
	case o.language == "":
		o.language = "bash"
	case o.language != "bash" && o.language != "go":
		return nil, cli.Usagef(env, "new: --language must be bash or go")
	}
	switch o.sideEffects {
	case "read", "write-local", "remote-read", "remote-write", "destructive":
	default:
		return nil, cli.Usagef(env, "new: --side-effects must be read, write-local, remote-read, remote-write or destructive")
	}
	if o.summary == "" {
		o.summary = "Use when you need " + strings.ReplaceAll(o.name, "-", " ") + "."
	}
	if err := checkText(o.summary, 10, 80); err != nil {
		return nil, cli.Usagef(env, "new: --summary %v", err)
	}
	if !strings.HasPrefix(o.summary, "Use when ") {
		return nil, cli.Usagef(env, "new: --summary must start with \"Use when \"")
	}
	seen := map[string]bool{}
	for _, in := range intents {
		in = strings.TrimSpace(in)
		if err := checkText(in, 3, 60); err != nil {
			return nil, cli.Usagef(env, "new: --intent %q: %v", in, err)
		}
		if !seen[in] {
			seen[in] = true
			o.intents = append(o.intents, in)
		}
	}
	// Padding adds only the name's own words ("use" and "need" are registry stopwords), so
	// placeholder intents never make two scaffolds look alike.
	spaced := strings.ReplaceAll(o.name, "-", " ")
	for _, in := range []string{spaced, "use " + spaced, "need " + spaced, o.name} {
		if len(o.intents) >= 3 {
			break
		}
		if utf8.RuneCountInString(in) >= 3 && !seen[in] {
			seen[in] = true
			o.intents = append(o.intents, in)
		}
	}
	if len(o.intents) > 12 {
		return nil, cli.Usagef(env, "new: at most 12 intents, got %d", len(o.intents))
	}
	if (o.distinctFrom == "") != (o.reason == "") {
		return nil, cli.Usagef(env, "new: --distinct-from and --reason go together")
	}
	if o.reason != "" {
		if err := checkText(o.reason, 10, 300); err != nil {
			return nil, cli.Usagef(env, "new: --reason %v", err)
		}
	}
	for _, ref := range []string{o.extends, o.distinctFrom} {
		if ref == o.name {
			return nil, cli.Usagef(env, "new: a tool cannot extend or be distinct from itself")
		}
	}
	return o, -1
}

// checkText enforces a single-line, dash-free text of min to max characters.
func checkText(s string, min, max int) error {
	n := utf8.RuneCountInString(s)
	if n < min || n > max {
		return fmt.Errorf("must be %d to %d characters, got %d", min, max, n)
	}
	if hasDash(s) {
		return errors.New("contains an em or en dash; use - , ; : or parentheses")
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return errors.New("must be a single line without control characters")
		}
	}
	return nil
}

// planNew resolves the home and renders every file, or returns an exit code (>= 0).
func planNew(env *cli.Env, o *newOpts) (*newPlan, int) {
	d := tmplData{
		Name: o.name, Summary: o.summary, Task: o.task,
		Date:    now().Format("2006-01-02"),
		Builder: builder(env.Getenv),
		DryRun:  o.sideEffects != "read",
	}
	if d.DryRun {
		d.DryRunFlag, d.DryRunName = "--dry-run", "dry-run"
	}
	p := &newPlan{}

	if o.scope == "global" {
		root, err := paths.Root(env.Getenv)
		if err != nil {
			return nil, cli.Envf(env, "%v", err)
		}
		if o.kind == "skill" {
			p.files = []planFile{{path: filepath.Join(root, "skills", o.name, "SKILL.md"), mode: 0o644, data: render("skill.tmpl", d)}}
			p.next = "write the judgment steps in SKILL.md; keep deterministic steps as tool calls"
			return p, -1
		}
		toolsDir := filepath.Join(root, "tools")
		if hit := pathCollision(o.name, toolsDir); hit != "" {
			fmt.Fprintf(env.Stderr, "agent-dry: G1 manifest: %q already resolves on PATH to %s; choose another name\n", o.name, hit)
			return nil, cli.ExitNegative
		}
		dir := filepath.Join(toolsDir, o.name)
		if _, err := os.Lstat(dir); err == nil {
			fmt.Fprintf(env.Stderr, "agent-dry: refusing to overwrite: %s exists; extend that tool instead\n", dir)
			return nil, cli.ExitNegative
		}
		t := toolManifest(o, d)
		if o.language == "bash" {
			d.TestCmd = "bash test.sh"
			p.files = []planFile{
				{path: filepath.Join(dir, o.name), mode: 0o755, data: render("bash-entry.tmpl", d)},
				{path: filepath.Join(dir, "test.sh"), mode: 0o644, data: render("bash-test.tmpl", d)},
			}
		} else {
			if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
				return nil, cli.Envf(env, "%s has no go.mod; Go tools are built inside the agent-dry module (a clone, not a bundle)", root)
			}
			d.GoUsage = strconv.Quote(string(render("usage", d)))
			p.files = []planFile{
				{path: filepath.Join(dir, o.name), mode: 0o755, data: render("go-launcher.tmpl", d)},
				{path: filepath.Join(dir, "main.go"), mode: 0o644, data: render("go-main.tmpl", d)},
				{path: filepath.Join(dir, "main_test.go"), mode: 0o644, data: render("go-main-test.tmpl", d)},
			}
		}
		raw, err := json.MarshalIndent(t, "", "  ")
		if err != nil {
			return nil, cli.Envf(env, "encode manifest: %v", err)
		}
		raw = append(raw, '\n')
		if err := manifest.Validate(raw); err != nil {
			return nil, cli.Usagef(env, "new: generated manifest is invalid: %v", err)
		}
		p.files = append([]planFile{{path: filepath.Join(dir, manifest.FileName), mode: 0o644, data: raw}}, p.files...)
		p.next = "implement the placeholder action and its tests, refine summary and intents in tool.json, then run: agent-dry check " + o.name
		return p, -1
	}

	gitRoot, err := gitTopLevel()
	if err != nil {
		return nil, cli.Envf(env, "project scope needs a Git repository: %v", err)
	}
	if o.kind == "skill" {
		p.files = []planFile{{path: filepath.Join(gitRoot, ".agents", "skills", o.name, "SKILL.md"), mode: 0o644, data: render("skill.tmpl", d)}}
		p.next = "write the judgment steps in SKILL.md; keep deterministic steps as tool calls"
		return p, -1
	}
	entry := "scripts/" + o.name
	if o.language == "bash" {
		d.TestCmd = "bash scripts/" + o.name + ".test.sh"
		p.files = []planFile{
			{path: filepath.Join(gitRoot, "scripts", o.name), mode: 0o755, data: render("bash-entry.tmpl", d)},
			{path: filepath.Join(gitRoot, "scripts", o.name+".test.sh"), mode: 0o644, data: render("bash-test.tmpl", d)},
		}
		p.next = "implement the placeholder action, then run: " + d.TestCmd + " && shellcheck scripts/" + o.name + " scripts/" + o.name + ".test.sh"
	} else {
		if _, err := os.Stat(filepath.Join(gitRoot, "go.mod")); err != nil {
			return nil, cli.Usagef(env, "new: %s has no go.mod; use --language bash for this project", gitRoot)
		}
		entry = "go run ./scripts/" + o.name
		d.GoUsage = strconv.Quote(string(render("usage", d)))
		p.files = []planFile{
			{path: filepath.Join(gitRoot, "scripts", o.name, "main.go"), mode: 0o644, data: render("go-main.tmpl", d)},
			{path: filepath.Join(gitRoot, "scripts", o.name, "main_test.go"), mode: 0o644, data: render("go-main-test.tmpl", d)},
		}
		p.next = "implement the placeholder action, then run: go vet ./scripts/" + o.name + " && go test ./scripts/" + o.name
	}
	p.listPath = filepath.Join(gitRoot, ProjectList)
	data, err := mergeProjectList(p.listPath, projectEntry{Name: o.name, Summary: o.summary, Intents: o.intents, Entry: entry, Kind: o.kind})
	if errors.Is(err, errListed) {
		fmt.Fprintf(env.Stderr, "agent-dry: refusing to overwrite: %q is already listed in %s\n", o.name, p.listPath)
		return nil, cli.ExitNegative
	}
	if err != nil {
		return nil, cli.Envf(env, "%v", err)
	}
	p.listData = data
	return p, -1
}

func builder(getenv func(string) string) string {
	if b := strings.TrimSpace(getenv("AGENT_DRY_BUILDER")); b != "" {
		return b
	}
	return "unknown"
}

func toolManifest(o *newOpts, d tmplData) *manifest.Tool {
	t := &manifest.Tool{
		Schema: "tool/v1", Name: o.name, Summary: o.summary, Intents: o.intents,
		Kind: o.kind, Scope: o.scope, Entry: o.name, Language: o.language,
		SideEffects: o.sideEffects, DryRun: d.DryRunFlag,
		ExitCodes: map[string]string{
			"0": "success",
			"1": "negative result: a file is missing",
			"2": "usage error",
			"3": "missing required command",
		},
		Platforms: []string{"darwin-arm64", "linux-amd64"},
		Test:      "bash test.sh",
		Status:    "trial",
		Extends:   o.extends,
		Origin:    manifest.Origin{Date: d.Date, Builder: d.Builder, Task: o.task},
	}
	if o.language == "bash" {
		t.Requires = []string{"wc"}
	} else {
		t.Test = "go test ."
	}
	if o.distinctFrom != "" {
		t.DistinctFrom = []manifest.Distinct{{Tool: o.distinctFrom, Reason: o.reason}}
	}
	return t
}

func render(name string, d tmplData) []byte {
	var b bytes.Buffer
	if err := templates.ExecuteTemplate(&b, name, d); err != nil {
		panic("agent-dry: template " + name + ": " + err.Error()) // embedded templates are tested
	}
	return b.Bytes()
}

func gitTopLevel() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	r := runProc(wd, 10*time.Second, false, "git", "rev-parse", "--show-toplevel")
	if r.startErr != nil {
		return "", r.startErr
	}
	if r.code != 0 {
		return "", fmt.Errorf("git rev-parse: %s", lastLine(r.stderr))
	}
	return strings.TrimSpace(r.stdout), nil
}

var errListed = errors.New("already listed")

// mergeProjectList returns the ProjectList content with e appended, keeping existing entries
// (and any fields this version does not know) as they are.
func mergeProjectList(path string, e projectEntry) ([]byte, error) {
	var items []json.RawMessage
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	case len(bytes.TrimSpace(raw)) > 0:
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("%s must be a JSON array of {name, summary, intents, entry}: %v", path, err)
		}
	}
	for _, it := range items {
		var existing projectEntry
		if json.Unmarshal(it, &existing) == nil && existing.Name == e.Name {
			return nil, errListed
		}
	}
	add, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	out, err := json.MarshalIndent(append(items, add), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// writePlan creates every file exclusively and rolls back what it created on failure.
func writePlan(p *newPlan) (err error) {
	var created []string // files and directories, in creation order
	defer func() {
		if err != nil {
			for i := len(created) - 1; i >= 0; i-- {
				_ = os.Remove(created[i])
			}
		}
	}()
	for _, f := range p.files {
		dirs, mkErr := mkdirTracked(filepath.Dir(f.path))
		created = append(created, dirs...)
		if mkErr != nil {
			return mkErr
		}
		fh, openErr := os.OpenFile(f.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, f.mode)
		if openErr != nil {
			return openErr
		}
		created = append(created, f.path)
		_, wErr := fh.Write(f.data)
		cErr := fh.Close()
		if wErr != nil {
			return wErr
		}
		if cErr != nil {
			return cErr
		}
		if chErr := os.Chmod(f.path, f.mode); chErr != nil { // umask may have narrowed it
			return chErr
		}
	}
	if p.listPath != "" {
		tmp := p.listPath + ".tmp"
		if wErr := os.WriteFile(tmp, p.listData, 0o644); wErr != nil {
			return wErr
		}
		if rErr := os.Rename(tmp, p.listPath); rErr != nil {
			_ = os.Remove(tmp)
			return rErr
		}
	}
	return nil
}

// mkdirTracked creates dir and its missing parents, returning the ones it created, outermost first.
func mkdirTracked(dir string) ([]string, error) {
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil {
			break
		}
		missing = append([]string{d}, missing...)
		if filepath.Dir(d) == d {
			break
		}
	}
	var made []string
	for _, d := range missing {
		if err := os.Mkdir(d, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return made, err
		}
		made = append(made, d)
	}
	return made, nil
}

// pathCollision returns where name resolves on PATH when that is not this tool: neither inside
// toolsDir, nor a symlink into it, nor the same tool inside another agent-dry root (an installed
// bundle). It returns "" when name does not resolve.
func pathCollision(name, toolsDir string) string {
	found, err := exec.LookPath(name)
	if err != nil && !errors.Is(err, exec.ErrDot) {
		return ""
	}
	abs, err := filepath.Abs(found)
	if err != nil {
		return found
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		real = abs
	}
	toolsReal, err := filepath.EvalSymlinks(toolsDir)
	if err != nil {
		toolsReal = toolsDir
	}
	for _, p := range []string{abs, real} {
		for _, t := range []string{toolsDir, toolsReal} {
			if strings.HasPrefix(p, t+string(filepath.Separator)) {
				return ""
			}
		}
	}
	for d := filepath.Dir(real); filepath.Dir(d) != d; d = filepath.Dir(d) {
		if filepath.Base(d) == name && filepath.Base(filepath.Dir(d)) == "tools" {
			if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(d)), paths.Marker)); err == nil {
				return ""
			}
		}
	}
	return abs
}

// loadAdvisory loads every source (project commands, installed skills and external providers as
// well as owned entries) for the reuse advisory. Tests replace it.
var loadAdvisory = func(getenv func(string) string) (searcher, error) {
	ix, err := registry.Load(getenv, registry.Options{})
	if err != nil {
		return nil, err
	}
	return ix, nil
}

// advisoryLimit caps the reuse advisory lines.
const advisoryLimit = 3

// adviseReuse prints on stderr the capabilities agent-dry does not own that may already cover the
// job, so scripted callers parsing stdout see nothing new. It is advisory only: it never blocks or
// changes the exit code, and it prints nothing when the full registry does not load.
func adviseReuse(env *cli.Env, query string) {
	ix, err := loadAdvisory(env.Getenv)
	if err != nil {
		return
	}
	hits := reuseCandidates(ix, query)
	if len(hits) == 0 {
		return
	}
	fmt.Fprintln(env.Stderr, "consider reusing before building:")
	for _, h := range hits {
		fmt.Fprintln(env.Stderr, "  "+advisoryLine(h))
	}
}

// reuseCandidates returns, best first, at most advisoryLimit hits at or above the threshold from
// sources agent-dry does not own: external providers, installed skills and project commands.
func reuseCandidates(ix searcher, query string) []registry.Result {
	var out []registry.Result
	for _, h := range ix.Search(query, 0) {
		if h.Score < registry.Threshold || h.Semantic || h.Kind == "wanted" || h.Kind == "seen" {
			continue
		}
		if h.Source != "external" && h.Source != "skill" && !strings.HasPrefix(h.Source, "project:") {
			continue
		}
		out = append(out, h)
		if len(out) == advisoryLimit {
			break
		}
	}
	return out
}

// advisoryLine formats one reuse candidate the way find prints it, with the installed or missing
// marker for an external provider.
func advisoryLine(h registry.Result) string {
	line := fmt.Sprintf("%s  %s  [%s %s] %s", h.Name, shorten(h.Summary, 90), h.Kind, h.Scope, h.Entry)
	if h.Installed != nil {
		if *h.Installed {
			return line + " (installed)"
		}
		return line + " (missing: " + shorten(h.Install, 72) + ")"
	}
	return line
}

// shorten collapses whitespace and cuts s to n runes, ending in "..." when cut.
func shorten(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimRight(string(r[:n-3]), " ") + "..."
}

// registryResultLine formats one hit the way find prints it.
func registryResultLine(h registry.Result) string {
	return fmt.Sprintf("%s  %s  [%s %s] %s  (score %.2f)", h.Name, h.Summary, h.Kind, h.Scope, h.Entry, h.Score)
}
