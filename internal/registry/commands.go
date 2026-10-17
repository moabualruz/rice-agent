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

	"github.com/moabualruz/agent-dry/docs"
	"github.com/moabualruz/agent-dry/internal/cli"
)

func init() {
	cli.Register(cli.Command{Name: "find", Summary: "rank existing tools, skills and project commands for intent words", Run: runFind})
	cli.Register(cli.Command{Name: "show", Summary: "print one entry's manifest, --help and how to run it", Run: runShow})
	cli.Register(cli.Command{Name: "index", Summary: "build or refresh the cached lookup index", Run: runIndex})
	cli.Register(cli.Command{Name: "guide", Summary: "print the DRY guide, or the tool contract with `guide tools`", Run: runGuide})
}

// DefaultLimit is the number of results find prints without --limit.
const DefaultLimit = 5

const maxLimit = 50

const findUsage = `Usage: agent-dry find <words...> [--json] [--project <root>] [--limit N]
Rank agent-dry tools and skills, the project's commands (just, package.json, Makefile, scripts/,
.agent-dry.json), installed skills, wanted families and earlier misses for the intent words.
Lines: name  summary  [kind scope] entry; hints: WANTED <family> and SEEN <date> <project>.

Options:
  --json            print {"results": [...], "hints": [...]}
  --project <root>  discover project commands from <root> (default: Git root of the working directory)
  --limit N         print at most N results (default 5)

Exit codes: 0 a result cleared the threshold, 1 none did (the miss is logged; hints alone do not
count), 2 usage error, 3 unreadable index or agent-dry root.
Example: agent-dry find scan for em dash
`

const showUsage = `Usage: agent-dry show <name> [--project <root>]
Print an entry's manifest fields, the first 40 lines of its --help (agent-dry tools) or how to run
it (project recipes and scripts), or the path to read (skills).

Exit codes: 0 shown, 1 no entry by that name, 2 usage error, 3 unreadable index or agent-dry root.
Example: agent-dry show check-no-dashes
`

const indexUsage = `Usage: agent-dry index [--rebuild]
Cache agent-dry tools, skills, installed skills and wanted families in <state>/index.json. find
uses the cache while the source modification times are unchanged; project commands stay live.

Options:
  --rebuild  rebuild even when the cache is fresh

Exit codes: 0 index fresh or rebuilt, 2 usage error, 3 unreadable root or index, or index not writable.
Example: agent-dry index --rebuild
`

const guideUsage = `Usage: agent-dry guide [tools]
Print the DRY guide (find, decide, reuse, extend, build, inline), or the tool contract with "tools".

Exit codes: 0 printed, 2 usage error.
Example: agent-dry guide tools
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
	Kind          string `json:"kind"` // wanted or seen
	Text          string `json:"text"`
	Family        string `json:"family,omitempty"`
	SuggestedName string `json:"suggested_name,omitempty"`
	Date          string `json:"date,omitempty"`
	Project       string `json:"project,omitempty"`
	Query         string `json:"query,omitempty"`
}

func runFind(env *cli.Env) int {
	var words []string
	asJSON, project, limit := false, "", DefaultLimit
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
			return cli.Usagef(env, "find: unknown flag %s; see agent-dry find --help", a)
		}
		words = append(words, a)
	}
	query := strings.TrimSpace(strings.Join(words, " "))
	if query == "" {
		return cli.Usagef(env, "find: give intent words, for example: agent-dry find scan for em dash")
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
	hints := ix.hints(query, results)
	if len(results) == 0 {
		if err := ix.recordMiss(query, time.Now()); err != nil {
			fmt.Fprintf(env.Stderr, "agent-dry: warning: miss not logged: %v\n", err)
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
func (ix *Index) hints(query string, results []Result) []Hint {
	hints := []Hint{}
	if d := ix.wanted(ix.prepareQuery(query)); d != nil && d.Wanted != nil {
		built := false
		for _, r := range results {
			if strings.EqualFold(r.Name, d.Wanted.SuggestedName) {
				built = true
			}
		}
		if !built {
			hints = append(hints, Hint{
				Kind: "wanted", Family: d.Wanted.Family, SuggestedName: d.Wanted.SuggestedName,
				Text: fmt.Sprintf("WANTED %s: build %s before use", d.Wanted.Family, d.Wanted.SuggestedName),
			})
		}
	}
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

// warn prints at most three source warnings to stderr.
func (ix *Index) warn(w io.Writer) {
	for i, msg := range ix.warnings {
		if i == 3 {
			fmt.Fprintf(w, "agent-dry: warning: %d more source warnings; run agent-dry index --rebuild\n", len(ix.warnings)-3)
			return
		}
		fmt.Fprintf(w, "agent-dry: warning: %s\n", msg)
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
			return cli.Usagef(env, "show: unknown flag %s; see agent-dry show --help", a)
		}
		names = append(names, a)
	}
	if len(names) != 1 {
		return cli.Usagef(env, "show: give exactly one name, for example: agent-dry show check-no-dashes")
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
		fmt.Fprintf(env.Stderr, "agent-dry: show: no entry named %q; try agent-dry find %s\n", redact(names[0]), redact(names[0]))
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
		fmt.Fprintf(w, "%s  [%s %s] agent-dry tool, status %s\n", t.Name, t.Kind, t.Scope, t.Status)
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

// toolHelp runs `<entry> --help` in the tool directory and returns at most max lines.
func toolHelp(entry, dir string, max int) string {
	ctx, cancel := context.WithTimeout(context.Background(), helpTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, entry, "--help")
	cmd.Dir = dir
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
	rebuild := false
	for _, a := range env.Args {
		switch {
		case isHelp(a):
			fmt.Fprint(env.Stdout, indexUsage)
			return cli.ExitOK
		case a == "--rebuild":
			rebuild = true
		default:
			return cli.Usagef(env, "index: unexpected argument %s; see agent-dry index --help", a)
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
	fmt.Fprintf(env.Stdout, "index %s: %d tools, %d skills, %d installed skills, %d wanted families at %s\n",
		state, tools, skills, ix.counts[srcSkill], ix.counts[srcWanted], ix.tilde(ix.indexPath()))
	return cli.ExitOK
}

func runGuide(env *cli.Env) int {
	switch {
	case len(env.Args) == 0:
		fmt.Fprint(env.Stdout, docs.Guide)
	case len(env.Args) == 1 && isHelp(env.Args[0]):
		fmt.Fprint(env.Stdout, guideUsage)
	case len(env.Args) == 1 && env.Args[0] == "tools":
		fmt.Fprint(env.Stdout, docs.ToolContract)
	default:
		return cli.Usagef(env, "guide: unknown topic %q; topics: tools", strings.Join(env.Args, " "))
	}
	return cli.ExitOK
}
