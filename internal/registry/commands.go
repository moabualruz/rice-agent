package registry

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/moabualruz/rice-agent/docs"
	"github.com/moabualruz/rice-agent/internal/cli"
)

func init() {
	cli.Register(cli.Command{Name: "find", Summary: "rank existing tools, skills, project commands and external CLIs for intent words", Run: runFind})
	cli.Register(cli.Command{Name: "show", Summary: "print one entry's manifest, --help and how to run it", Run: runShow})
	cli.Register(cli.Command{Name: "index", Summary: "build or refresh the cached lookup index", Run: runIndex})
	cli.Register(cli.Command{Name: "guide", Summary: "print the DRY guide or one embedded topic (`guide --list`)", Run: runGuide})
}

// DefaultLimit is the number of results find prints without --limit.
const DefaultLimit = 5

const maxLimit = 50

const findUsage = `Usage: rice-agent find <words...> [--json] [--project <root>] [--limit N] [--debug]
Rank rice-agent tools and skills, the project's commands (just, package.json, Makefile, scripts/,
.agent-dry.json), installed skills, external CLIs (registry/providers.json), wanted families and
earlier misses for the intent words.
Lines: name  summary  [kind scope] entry; an external CLI ends with (installed) or
(missing: <install hint>), checked on PATH now; hints: WANTED <family>, MAYBE <name> and
SEEN <date> <project>.
When nothing clears the threshold and a corpus index exists (rice-agent index --semantic), zvec-grep
(zg) recalls paraphrases within 3 s: a hit both its FTS and vector routes rank first is a result
(source semantic); the next hit prints as one MAYBE hint. Without zg or the index, or on any zg
failure, find stays lexical.

Options:
  --json            print {"results": [...], "hints": [...]}; recall hits carry "semantic": true
  --project <root>  discover project commands from <root> (default: Git root of the working directory)
  --limit N         print at most N results (default 5)
  --debug           print one stderr line on what semantic recall did

Exit codes: 0 a result cleared the threshold or semantic recall agreed, 1 none did (the miss is
logged; hints alone do not count), 2 usage error, 3 unreadable index or rice-agent root.
Example: rice-agent find scan for em dash
`

const showUsage = `Usage: rice-agent show <name> [--project <root>]
Print an entry's manifest fields, the first 40 lines of its --help (rice-agent tools) or how to run
it (project recipes and scripts), the path to read (skills), or an external CLI's provider fields
and, when its command is on PATH, the first 30 lines of its --help.

Exit codes: 0 shown, 1 no entry by that name, 2 usage error, 3 unreadable index or rice-agent root.
Example: rice-agent show check-no-dashes
`

const indexUsage = `Usage: rice-agent index [--rebuild] [--semantic]
Cache rice-agent tools, skills, installed skills, external providers and wanted families in
<state>/index.json. find uses the cache while the source modification times are unchanged; project
commands and whether an external command is on PATH stay live. Every rebuild also writes one card
per tool, skill, external provider and wanted family to <state>/corpus/<kind>/<name>.md and
removes cards whose entry is gone.

Options:
  --rebuild   rebuild even when the cache is fresh
  --semantic  create or refresh the zvec-grep index of the corpus:
              zg index <corpus> --mode direct --embedding <config semantic.embedding,
              default local/potion-code-16m-v2>. Only this flag creates that index; once it
              exists, later rebuilds refresh it incrementally.

Exit codes: 0 index fresh or rebuilt, 2 usage error, 3 unreadable root or index, index not
writable, or --semantic without zg or with a failed zg index.
Example: rice-agent index --semantic
`

const guideUsage = `Usage: rice-agent guide [topic | --list]
Print the DRY guide (find, decide, reuse, extend, build, inline), or one embedded topic: "tools" is
the tool contract and every other docs/<topic>.md is a topic.

Options:
  --list  print the topic names, one per line

Exit codes: 0 printed, 2 usage error or unknown topic.
Example: rice-agent guide tools
`

func isHelp(a string) bool { return a == "-h" || a == "--help" }

// flagValue handles "--name value" and "--name=value"; it returns the value and how many
// arguments it consumed (0 when a is not this flag).
func flagValue(args []string, i int, name string) (string, int, error) {
	a := args[i]
	if v, ok := strings.CutPrefix(a, name+"="); ok {
		return v, 1, nil
	}
	if a != name {
		return "", 0, nil
	}
	if i+1 >= len(args) {
		return "", 0, fmt.Errorf("%s needs a value", name)
	}
	return args[i+1], 2, nil
}

func checkProject(p string) error {
	if p == "" {
		return nil
	}
	info, err := os.Stat(p)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("--project %s is not a directory", p)
	}
	return nil
}

// Hint is a WANTED or SEEN line.
type Hint struct {
	Kind          string `json:"kind"` // wanted, maybe or seen
	Text          string `json:"text"`
	Name          string `json:"name,omitempty"`    // maybe: the entry zvec-grep suggested
	Summary       string `json:"summary,omitempty"` // maybe: its summary
	Family        string `json:"family,omitempty"`
	SuggestedName string `json:"suggested_name,omitempty"`
	Date          string `json:"date,omitempty"`
	Project       string `json:"project,omitempty"`
	Query         string `json:"query,omitempty"`
}

func runFind(env *cli.Env) int {
	var words []string
	asJSON, debug, project, limit := false, false, "", DefaultLimit
	args := env.Args
	for i := 0; i < len(args); i++ {
		a := args[i]
		if isHelp(a) {
			fmt.Fprint(env.Stdout, findUsage)
			return cli.ExitOK
		}
		if a == "--" {
			words = append(words, args[i+1:]...)
			break
		}
		if a == "--json" {
			asJSON = true
			continue
		}
		if a == "--debug" {
			debug = true
			continue
		}
		if v, n, err := flagValue(args, i, "--project"); err != nil {
			return cli.Usagef(env, "find: %v", err)
		} else if n > 0 {
			project, i = v, i+n-1
			continue
		}
		if v, n, err := flagValue(args, i, "--limit"); err != nil {
			return cli.Usagef(env, "find: %v", err)
		} else if n > 0 {
			l, err := strconv.Atoi(v)
			if err != nil || l < 1 || l > maxLimit {
				return cli.Usagef(env, "find: --limit must be an integer from 1 to %d", maxLimit)
			}
			limit, i = l, i+n-1
			continue
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			return cli.Usagef(env, "find: unknown flag %s; see rice-agent find --help", a)
		}
		words = append(words, a)
	}
	query := strings.TrimSpace(strings.Join(words, " "))
	if query == "" {
		return cli.Usagef(env, "find: give intent words, for example: rice-agent find scan for em dash")
	}
	if err := checkProject(project); err != nil {
		return cli.Usagef(env, "find: %v", err)
	}
	ix, err := Load(env.Getenv, Options{ProjectRoot: project})
	if err != nil {
		return cli.Envf(env, "find: %v", err)
	}
	ix.warn(env.Stderr)

	results := ix.Search(query, limit)
	// Semantic recall runs only after a lexical miss; the lexical decision stands otherwise.
	var rec recallResult
	if len(results) == 0 {
		rec = ix.recall(query)
		results = append(results, rec.Results...)
		if len(results) > limit {
			results = results[:limit]
		}
	} else {
		rec.Note = "semantic recall skipped: lexical match"
	}
	if debug {
		fmt.Fprintf(env.Stderr, "rice-agent: debug: %s\n", rec.Note)
	}
	hints := ix.hints(query, results, rec)
	if len(results) == 0 {
		if err := ix.recordMiss(query, time.Now(), rec.Ran); err != nil {
			fmt.Fprintf(env.Stderr, "rice-agent: warning: miss not logged: %v\n", err)
		}
	}
	if asJSON {
		enc := json.NewEncoder(env.Stdout)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(struct {
			Results []Result `json:"results"`
			Hints   []Hint   `json:"hints"`
		}{results, hints})
	} else {
		w := bufio.NewWriter(env.Stdout)
		for _, r := range results {
			fmt.Fprintln(w, resultLine(r))
		}
		for _, h := range hints {
			fmt.Fprintln(w, h.Text)
		}
		w.Flush()
	}
	if len(results) == 0 {
		return cli.ExitNegative
	}
	return cli.ExitOK
}

func resultLine(r Result) string {
	var b strings.Builder
	b.WriteString(r.Name)
	if s := truncate(r.Summary, summaryRune); s != "" {
		b.WriteString("  ")
		b.WriteString(s)
	}
	fmt.Fprintf(&b, "  [%s %s] %s", r.Kind, r.Scope, tailTruncate(r.Entry, 72))
	if r.Installed != nil {
		if *r.Installed {
			b.WriteString(" (installed)")
		} else {
			fmt.Fprintf(&b, " (missing: %s)", truncate(r.Install, 72))
		}
	}
	return b.String()
}

// tailTruncate keeps the end of a long path, which is the part that names the file.
func tailTruncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "..." + string(r[len(r)-(n-3):])
}

// hints returns at most one WANTED and, when nothing matched, at most one SEEN line. SEEN is
// computed before the current miss is appended.
func (ix *Index) hints(query string, results []Result, rec recallResult) []Hint {
	hints := []Hint{}
	built := func(f *Family) bool {
		for _, r := range results {
			if strings.EqualFold(r.Name, f.SuggestedName) {
				return true
			}
		}
		return false
	}
	if d := ix.wanted(ix.prepareQuery(query)); d != nil && d.Wanted != nil && !built(d.Wanted) {
		hints = append(hints, wantedHint(d.Wanted))
	}
	for _, h := range rec.Wanted {
		if len(hints) == 0 || hints[0].Family != h.Family {
			hints = append(hints, h)
			break
		}
	}
	hints = append(hints, rec.Maybes...)
	if len(results) == 0 {
		if m := ix.seen(query); m != nil {
			date := m.TS
			if len(date) >= 10 {
				date = date[:10]
			}
			proj := m.Project
			if proj == "" {
				proj = "global"
			}
			q := truncate(redact(m.Query), 80)
			hints = append(hints, Hint{
				Kind: "seen", Date: date, Project: proj, Query: q,
				Text: fmt.Sprintf("SEEN %s %s: earlier miss %q; build before repeating", date, proj, q),
			})
		}
	}
	return hints
}

func wantedHint(f *Family) Hint {
	return Hint{
		Kind: "wanted", Family: f.Family, SuggestedName: f.SuggestedName,
		Text: fmt.Sprintf("WANTED %s: build %s before use", f.Family, f.SuggestedName),
	}
}

// warn prints at most three source warnings to stderr.
func (ix *Index) warn(w io.Writer) {
	for i, msg := range ix.warnings {
		if i == 3 {
			fmt.Fprintf(w, "rice-agent: warning: %d more source warnings; run rice-agent index --rebuild\n", len(ix.warnings)-3)
			return
		}
		fmt.Fprintf(w, "rice-agent: warning: %s\n", msg)
	}
}

func runShow(env *cli.Env) int {
	var names []string
	project := ""
	args := env.Args
	for i := 0; i < len(args); i++ {
		a := args[i]
		if isHelp(a) {
			fmt.Fprint(env.Stdout, showUsage)
			return cli.ExitOK
		}
		if v, n, err := flagValue(args, i, "--project"); err != nil {
			return cli.Usagef(env, "show: %v", err)
		} else if n > 0 {
			project, i = v, i+n-1
			continue
		}
		if strings.HasPrefix(a, "-") {
			return cli.Usagef(env, "show: unknown flag %s; see rice-agent show --help", a)
		}
		names = append(names, a)
	}
	if len(names) != 1 {
		return cli.Usagef(env, "show: give exactly one name, for example: rice-agent show check-no-dashes")
	}
	if err := checkProject(project); err != nil {
		return cli.Usagef(env, "show: %v", err)
	}
	ix, err := Load(env.Getenv, Options{ProjectRoot: project})
	if err != nil {
		return cli.Envf(env, "show: %v", err)
	}
	ds := ix.lookup(names[0])
	if len(ds) == 0 {
		fmt.Fprintf(env.Stderr, "rice-agent: show: no entry named %q; try rice-agent find %s\n", redact(names[0]), redact(names[0]))
		return cli.ExitNegative
	}
	w := bufio.NewWriter(env.Stdout)
	defer w.Flush()
	for i, d := range ds {
		if i == 3 {
			break
		}
		if i > 0 {
			fmt.Fprintln(w)
		}
		ix.showDoc(w, d)
	}
	return cli.ExitOK
}

func kv(w io.Writer, k, v string) {
	if v != "" {
		fmt.Fprintf(w, "  %-13s %s\n", k+":", v)
	}
}

func (ix *Index) showDoc(w io.Writer, d *doc) {
	switch {
	case d.Tool != nil:
		t := d.Tool
		fmt.Fprintf(w, "%s  [%s %s] rice-agent tool, status %s\n", t.Name, t.Kind, t.Scope, t.Status)
		kv(w, "summary", t.Summary)
		entry := ix.tilde(t.EntryPath())
		if e := ix.displayEntry(d); e != entry {
			entry = e + " (" + entry + ")"
		}
		kv(w, "entry", entry)
		kv(w, "side effects", t.SideEffects)
		dry := t.DryRun
		if dry == "" {
			dry = "none"
		}
		kv(w, "dry run", dry)
		kv(w, "exit codes", exitCodes(t.ExitCodes))
		kv(w, "test", t.Test+" (in "+ix.tilde(t.Dir)+")")
		kv(w, "status", t.Status)
		kv(w, "requires", strings.Join(t.Requires, ", "))
		kv(w, "config", strings.Join(t.Config, ", "))
		kv(w, "intents", strings.Join(t.Intents, "; "))
		kv(w, "superseded by", t.SupersededBy)
		fmt.Fprintf(w, "\n$ %s --help\n", t.Name)
		fmt.Fprint(w, toolHelp(t.EntryPath(), t.Dir, 40))
	case d.Wanted != nil:
		f := d.Wanted
		fmt.Fprintf(w, "%s  [wanted global] no tool yet\n", f.Family)
		kv(w, "build", f.SuggestedName+" ("+f.SuggestedKind+")")
		kv(w, "evidence", f.Evidence)
		kv(w, "research", f.Research)
		kv(w, "intents", strings.Join(f.Intents, "; "))
	case d.Provider != nil:
		p := d.Provider
		at := ix.installedAt(p)
		fmt.Fprintf(w, "%s  [%s %s] external CLI, no rice-agent wrapper\n", p.Name, d.Kind, d.Scope)
		kv(w, "summary", p.Summary)
		if at != "" {
			kv(w, "command", p.Command+" (installed: "+ix.tilde(at)+")")
		} else {
			kv(w, "command", p.Command+" (missing: "+p.Install+")")
		}
		kv(w, "owner", p.Owner)
		kv(w, "license", p.License)
		kv(w, "install", p.Install)
		kv(w, "homepage", p.Homepage)
		if p.RequiresPolicyCheck {
			kv(w, "policy check", "required: Python inside, so run only its supported CLI")
		}
		kv(w, "notes", p.Notes)
		kv(w, "intents", strings.Join(p.Intents, "; "))
		if at != "" {
			fmt.Fprintf(w, "\n$ %s --help\n", p.Command)
			fmt.Fprint(w, toolHelp(at, "", externalHelpLines))
		}
	case d.Kind == "skill":
		fmt.Fprintf(w, "%s  [skill %s] %s\n", d.Name, d.Scope, d.Source)
		kv(w, "description", d.Summary)
		kv(w, "read", ix.tilde(d.Entry))
	default:
		fmt.Fprintf(w, "%s  [%s %s] %s\n", d.Name, d.Kind, d.Scope, d.Source)
		kv(w, "summary", d.Summary)
		kv(w, "run", d.Entry)
		kv(w, "from", ix.tilde(ix.project))
		kv(w, "command", d.Command)
		kv(w, "defined in", ix.tilde(d.Path))
	}
}

func exitCodes(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, _ := strconv.Atoi(keys[i])
		b, _ := strconv.Atoi(keys[j])
		return a < b
	})
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+" "+m[k])
	}
	return strings.Join(parts, "; ")
}

// helpTimeout bounds `<entry> --help` in show.
var helpTimeout = 10 * time.Second

// externalHelpLines is how much of an external CLI's --help show prints.
const externalHelpLines = 30

// toolHelp runs `<entry> --help` in dir (the working directory when empty) and returns at most
// max lines. NO_COLOR keeps terminal escapes out of the captured text.
func toolHelp(entry, dir string, max int) string {
	ctx, cancel := context.WithTimeout(context.Background(), helpTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, entry, "--help")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	var b strings.Builder
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	for i, l := range lines {
		if i == max {
			fmt.Fprintf(&b, "... (%d more lines)\n", len(lines)-max)
			break
		}
		if l != "" || i < len(lines)-1 {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		fmt.Fprintf(&b, "(--help timed out after %s)\n", helpTimeout)
	case err != nil:
		fmt.Fprintf(&b, "(--help failed: %v)\n", err)
	}
	return b.String()
}

func runIndex(env *cli.Env) int {
	rebuild, semantic := false, false
	for _, a := range env.Args {
		switch {
		case isHelp(a):
			fmt.Fprint(env.Stdout, indexUsage)
			return cli.ExitOK
		case a == "--rebuild":
			rebuild = true
		case a == "--semantic":
			semantic = true
		default:
			return cli.Usagef(env, "index: unexpected argument %s; see rice-agent index --help", a)
		}
	}
	ix, err := load(env.Getenv, Options{NoProject: true}, rebuild)
	if err != nil {
		return cli.Envf(env, "index: %v", err)
	}
	ix.warn(env.Stderr)
	if ix.cacheErr != nil {
		return cli.Envf(env, "index: cannot write %s: %v", ix.indexPath(), ix.cacheErr)
	}
	tools, skills := 0, 0
	for _, d := range ix.docs {
		if d.Tool != nil && !d.Hidden {
			tools++
		}
		if d.Source == srcAgentDry && d.Kind == "skill" && d.Tool == nil {
			skills++
		}
	}
	state := "rebuilt"
	if ix.fromCache {
		state = "fresh"
	}
	fmt.Fprintf(env.Stdout, "index %s: %d tools, %d skills, %d installed skills, %d external providers, %d wanted families at %s\n",
		state, tools, skills, ix.counts[srcSkill], ix.counts[srcExternal], ix.counts[srcWanted], ix.tilde(ix.indexPath()))
	if semantic {
		return ix.indexSemantic(env, rebuild)
	}
	// An existing corpus index with changed cards is refreshed here, retrying a failed attempt.
	dir := ix.corpusDir()
	if pending, _ := pendingState(dir); pending && semanticIndexed(dir) {
		zg := lookPath(env.Getenv, "zg")
		if zg == "" {
			fmt.Fprintf(env.Stderr, "rice-agent: warning: semantic index refresh pending; zg not on PATH\n")
		} else if err := refreshCorpus(zg, dir, refreshTimeout); err != nil {
			fmt.Fprintf(env.Stderr, "rice-agent: warning: semantic index refresh failed: %v; run rice-agent index --semantic\n", err)
		} else {
			fmt.Fprintf(env.Stdout, "semantic index refreshed at %s\n", ix.tilde(dir))
		}
	}
	return cli.ExitOK
}

// indexSemantic writes the corpus and explicitly creates or refreshes its zvec-grep index. This is
// the only place a corpus index is created (persistent indexes need an explicit request).
func (ix *Index) indexSemantic(env *cli.Env, rebuild bool) int {
	dir := ix.corpusDir()
	_, cards, err := writeCorpus(dir, ix.root, ix.docs)
	if err != nil {
		return cli.Envf(env, "index: corpus %s: %v", dir, err)
	}
	zg := lookPath(env.Getenv, "zg")
	if zg == "" {
		return cli.Envf(env, "index: --semantic needs zvec-grep (zg) on PATH; the corpus is at %s", ix.tilde(dir))
	}
	model, err := embeddingModel(env.Getenv)
	if err != nil {
		return cli.Envf(env, "index: %v", err)
	}
	// A new index is built directly; an existing one goes through --mode auto, because a zvec-grep
	// server that has served the corpus holds its write lease (see refreshCorpus).
	args := []string{"index", dir, "--mode", "auto"}
	verb := "refreshed"
	switch {
	case !semanticIndexed(dir):
		args, verb = []string{"index", dir, "--mode", "direct", "--embedding", model}, "created"
	case rebuild:
		args, verb = append(args, "--rebuild", "--embedding", model), "rebuilt"
	}
	start := time.Now()
	if _, _, err := runZg(zg, dir, indexTimeout, args...); err != nil {
		return cli.Envf(env, "index: semantic index: %v", err)
	}
	if !semanticIndexed(dir) {
		return cli.Envf(env, "index: zg index finished but %s has no %s", ix.tilde(dir), zvecIndexDir)
	}
	clearPending(dir)
	fmt.Fprintf(env.Stdout, "semantic index %s: %d cards at %s in %.1fs (zg %s)\n",
		verb, cards, ix.tilde(dir), time.Since(start).Seconds(), strings.Join(args[2:], " "))
	if n := indexedFiles(zg, dir); n >= 0 && n < cards {
		fmt.Fprintf(env.Stderr, "rice-agent: warning: zg status reports %d of %d cards indexed; recall misses the rest\n", n, cards)
	}
	return cli.ExitOK
}

// guideDocs is where guide reads its text: the embedded docs package. Tests replace it.
var guideDocs = struct {
	guide  string
	topic  func(name string) (string, bool)
	topics func() []string
}{docs.Guide, docs.Topic, docs.Topics}

func runGuide(env *cli.Env) int {
	args := env.Args
	switch {
	case len(args) == 0:
		fmt.Fprint(env.Stdout, guideDocs.guide)
		return cli.ExitOK
	case len(args) > 1:
		return cli.Usagef(env, "guide: give at most one topic; see rice-agent guide --list")
	case isHelp(args[0]):
		fmt.Fprint(env.Stdout, guideUsage)
		return cli.ExitOK
	case args[0] == "--list":
		w := bufio.NewWriter(env.Stdout)
		for _, t := range guideDocs.topics() {
			fmt.Fprintln(w, t)
		}
		w.Flush()
		return cli.ExitOK
	case strings.HasPrefix(args[0], "-"):
		return cli.Usagef(env, "guide: unknown flag %s; see rice-agent guide --help", args[0])
	}
	text, ok := guideDocs.topic(args[0])
	if !ok {
		return cli.Usagef(env, "guide: unknown topic %q; topics: %s", redact(args[0]), strings.Join(guideDocs.topics(), ", "))
	}
	fmt.Fprint(env.Stdout, text)
	return cli.ExitOK
}
