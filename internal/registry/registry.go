// Package registry builds the lookup index over every source and ranks queries against it.
// It registers the find, show, index and guide commands.
//
// Interface contract used by other packages (keep signatures stable):
//
//	Load(getenv, opts) (*Index, error)
//	(*Index).Search(query, limit) []Result
//	Threshold
//
// Sources: agent-dry tools (listed statuses) and skills, the current project's commands (just
// recipes, package.json scripts, Makefile targets, executable scripts/*, .agent-dry.json), installed
// skills (~/.agents/skills, ~/.claude/skills, project .agents/skills and .claude/skills), external
// providers (registry/providers.json: mature CLIs from upstream or other owner projects, listed as
// data with no wrapper code), wanted families and the miss log. Search ranks only things that
// exist; wanted families and earlier misses are hints that find prints separately. An external
// provider ranks like a tool and carries whether its command resolves on PATH at query time.
package registry

import (
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/moabualruz/agent-dry/internal/manifest"
	"github.com/moabualruz/agent-dry/internal/paths"
)

// Result is one ranked lookup hit.
type Result struct {
	Name    string  `json:"name"`
	Summary string  `json:"summary"`
	Kind    string  `json:"kind"`   // script, cli, skill, recipe, external
	Scope   string  `json:"scope"`  // global or project
	Entry   string  `json:"entry"`  // command or path to run or read
	Source  string  `json:"source"` // agent-dry, project:just, project:npm, project:make, project:scripts, project:agent-dry, skill, external
	Score   float64 `json:"score"`

	// External providers only: whether Entry resolves on PATH now, and the install hint.
	Installed *bool  `json:"installed,omitempty"`
	Install   string `json:"install,omitempty"`

	// Semantic marks a zvec-grep recall hit (Source "semantic"); its Score is zvec-grep's fused
	// rank score, which is not comparable to lexical scores or Threshold.
	Semantic bool `json:"semantic,omitempty"`
}

// Options select the sources Load reads.
type Options struct {
	ProjectRoot string // Git root to discover project commands from; empty means the working directory's root
	NoProject   bool   // skip project discovery (used by gates and tests)
	// NoInstalled is admission mode: skip skills installed under HOME and external providers, so
	// duplicate scores compare owned entries only and do not depend on the host.
	NoInstalled bool
}

// Threshold is the minimum score for a result to count as a match. Scores are BM25F over name,
// intents, summary and tags, multiplied by the share of the query's IDF mass the entry matches, plus
// an exact-name boost. Tuned with the fixtures in testdata/registry: a query naming one distinctive
// intent clears it, a query sharing one incidental word does not.
var Threshold = 2.0

// Source names used in Result.Source.
const (
	srcAgentDry = "agent-dry"
	srcSkill    = "skill"
	srcWanted   = "wanted"
	srcExternal = "external"

	kindExternal = "external"
)

// doc is one indexed entry. The exported fields are cached in index.json.
type doc struct {
	Name     string         `json:"name"`
	Summary  string         `json:"summary,omitempty"`
	Intents  []string       `json:"intents,omitempty"`
	Tags     []string       `json:"tags,omitempty"`
	Kind     string         `json:"kind"`
	Scope    string         `json:"scope"`
	Entry    string         `json:"entry"`
	Source   string         `json:"source"`
	Path     string         `json:"path,omitempty"`    // defining file or tool directory
	Command  string         `json:"command,omitempty"` // package.json script body
	Hidden   bool           `json:"hidden,omitempty"`  // deprecated or retired tool: show only
	Tool     *manifest.Tool `json:"tool,omitempty"`
	Wanted   *Family        `json:"wanted,omitempty"`
	Provider *Provider      `json:"provider,omitempty"`

	tf   map[string][nFields]int
	lens [nFields]int
}

// Family is one wanted/v1 entry: recurring work with evidence and no tool yet.
type Family struct {
	Family        string   `json:"family"`
	Intents       []string `json:"intents"`
	Evidence      string   `json:"evidence"`
	Research      string   `json:"research,omitempty"`
	SuggestedName string   `json:"suggested_name"`
	SuggestedKind string   `json:"suggested_kind"`
}

// Index is the loaded, ranked lookup structure.
type Index struct {
	docs []*doc
	df   map[string]int
	avg  [nFields]float64

	root        string
	home        string
	stateDir    string
	project     string // Git root of the project, or ""
	projectName string
	getenv      func(string) string
	warnings    []string
	fromCache   bool
	cacheErr    error
	counts      map[string]int
}

// Ranking fields and their weights.
const (
	fName = iota
	fIntents
	fSummary
	fTags
	nFields
)

var (
	fieldWeight = [nFields]float64{3.0, 2.0, 1.0, 1.5}
	fieldB      = [nFields]float64{0.3, 0.5, 0.75, 0.3}
)

const (
	k1 = 1.2
	// minDocs floors N in the IDF so a small registry still scores a distinctive term as rare.
	minDocs     = 40
	exactBoost  = 10.0
	wordBoost   = 4.0
	summaryRune = 90
)

// Load reads every source: cached agent-dry and installed sources from StateDir/index.json when
// fresh, project commands live. The error is non-nil only for an unreadable root or index.
func Load(getenv func(string) string, opts Options) (*Index, error) {
	return load(getenv, opts, false)
}

func load(getenv func(string) string, opts Options, rebuild bool) (*Index, error) {
	root, err := paths.Root(getenv)
	if err != nil {
		return nil, err
	}
	ix := &Index{
		root:     root,
		home:     paths.Home(getenv),
		stateDir: paths.StateDir(getenv),
		getenv:   getenv,
		counts:   map[string]int{},
	}
	c, err := ix.cached(rebuild)
	if err != nil {
		return nil, err
	}
	ix.warnings = append(ix.warnings, c.Warnings...)

	var projectDocs, projectSkills []*doc
	if !opts.NoProject {
		ix.project = projectRoot(opts.ProjectRoot)
		if ix.project != "" {
			ix.projectName = filepath.Base(ix.project)
			var warns []string
			projectDocs, projectSkills, warns = discoverProject(ix.project, getenv)
			ix.warnings = append(ix.warnings, warns...)
		}
	}

	// Priority order; skills are deduplicated by name with the first one kept.
	seenSkill := map[string]bool{}
	add := func(ds []*doc) {
		for _, d := range ds {
			if d.Kind == "skill" && d.Source != srcWanted {
				key := strings.ToLower(d.Name)
				if seenSkill[key] {
					continue
				}
				seenSkill[key] = true
			}
			ix.docs = append(ix.docs, d)
			ix.counts[d.Source]++
		}
	}
	add(c.Tools)
	add(c.Skills)
	add(projectDocs)
	add(projectSkills)
	if !opts.NoInstalled {
		add(c.Installed)
		add(c.Providers)
	}
	add(c.Wanted)
	ix.prepare()
	return ix, nil
}

// prepare tokenizes every document and computes document frequencies and field lengths.
func (ix *Index) prepare() {
	ix.df = map[string]int{}
	var total [nFields]int
	for _, d := range ix.docs {
		fields := [nFields][]string{
			tokenize(d.Name),
			tokenize(strings.Join(d.Intents, " . ")),
			tokenize(d.Summary),
			tokenize(strings.Join(d.Tags, " ")),
		}
		d.tf = map[string][nFields]int{}
		for f, toks := range fields {
			d.lens[f] = len(toks)
			total[f] += len(toks)
			for _, t := range toks {
				v := d.tf[t]
				v[f]++
				d.tf[t] = v
			}
		}
		for t := range d.tf {
			ix.df[t]++
		}
	}
	for f := range total {
		if len(ix.docs) > 0 {
			ix.avg[f] = float64(total[f]) / float64(len(ix.docs))
		}
		if ix.avg[f] == 0 {
			ix.avg[f] = 1
		}
	}
}

// query is a prepared search: unique terms with their IDF and the raw text for name boosts.
type query struct {
	raw   string
	terms []string
	idf   []float64
	mass  float64
}

func (ix *Index) prepareQuery(q string) query {
	pq := query{raw: strings.ToLower(strings.TrimSpace(q))}
	n := float64(max(len(ix.docs), minDocs))
	seen := map[string]bool{}
	for _, t := range tokenize(q) {
		if seen[t] {
			continue
		}
		seen[t] = true
		df := float64(ix.df[t])
		idf := math.Log(1 + (n-df+0.5)/(df+0.5))
		pq.terms = append(pq.terms, t)
		pq.idf = append(pq.idf, idf)
		pq.mass += idf
	}
	return pq
}

// score is BM25F over the weighted fields times the matched share of the query's IDF mass, plus
// the exact-name boost.
func (ix *Index) score(d *doc, q query) float64 {
	var raw, matched float64
	for i, t := range q.terms {
		tf, ok := d.tf[t]
		if !ok {
			continue
		}
		var w float64
		for f := 0; f < nFields; f++ {
			if tf[f] == 0 {
				continue
			}
			norm := 1 - fieldB[f] + fieldB[f]*float64(d.lens[f])/ix.avg[f]
			w += fieldWeight[f] * float64(tf[f]) / norm
		}
		raw += q.idf[i] * w * (k1 + 1) / (w + k1)
		matched += q.idf[i]
	}
	s := 0.0
	if q.mass > 0 {
		s = raw * matched / q.mass
	}
	return s + nameBoost(d.Name, q.raw)
}

func nameBoost(name, raw string) float64 {
	name = strings.ToLower(name)
	if raw == "" || name == "" {
		return 0
	}
	words := strings.Fields(raw)
	if raw == name || strings.Join(words, "-") == name {
		return exactBoost
	}
	// A compound name typed as one word inside a longer query ("show host-run flags"). Plain
	// words such as "check" or "build" get no boost: the name field already weighs them.
	if strings.ContainsAny(name, "-_:.") {
		for _, w := range words {
			if w == name {
				return wordBoost
			}
		}
	}
	return 0
}

type scored struct {
	d *doc
	s float64
}

// rank scores every document the filter admits and returns those at or above Threshold, best first.
func (ix *Index) rank(q query, keep func(*doc) bool) []scored {
	var out []scored
	for _, d := range ix.docs {
		if !keep(d) {
			continue
		}
		if s := ix.score(d, q); s >= Threshold {
			out = append(out, scored{d, s})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].s != out[j].s {
			return out[i].s > out[j].s
		}
		return out[i].d.Name < out[j].d.Name
	})
	return out
}

func searchable(d *doc) bool { return d.Source != srcWanted && !d.Hidden }

// Search returns at most limit results at or above Threshold, best first. A limit of 0 or less
// returns every result that clears the threshold. Wanted families and earlier misses are not
// results.
func (ix *Index) Search(query string, limit int) []Result {
	ranked := ix.rank(ix.prepareQuery(query), searchable)
	if limit > 0 && len(ranked) > limit {
		ranked = ranked[:limit]
	}
	out := make([]Result, 0, len(ranked))
	for _, r := range ranked {
		out = append(out, ix.result(r.d, math.Round(r.s*100)/100))
	}
	return out
}

// result is the Result for d with score s.
func (ix *Index) result(d *doc, s float64) Result {
	res := Result{
		Name:    d.Name,
		Summary: d.Summary,
		Kind:    d.Kind,
		Scope:   d.Scope,
		Entry:   ix.displayEntry(d),
		Source:  d.Source,
		Score:   s,
	}
	if p := d.Provider; p != nil {
		installed := ix.installedAt(p) != ""
		res.Installed, res.Install = &installed, p.Install
	}
	return res
}

// wanted returns the best wanted family clearing the threshold, or nil.
func (ix *Index) wanted(q query) *doc {
	ranked := ix.rank(q, func(d *doc) bool { return d.Source == srcWanted })
	if len(ranked) == 0 {
		return nil
	}
	return ranked[0].d
}

// displayEntry is what an agent runs or reads: the bare command name when the tool is linked on
// PATH, otherwise its path with the home directory shortened to ~.
func (ix *Index) displayEntry(d *doc) string {
	if d.Tool != nil {
		if p := lookPath(ix.getenv, d.Name); p != "" && sameFile(p, d.Tool.EntryPath()) {
			return d.Name
		}
		return ix.tilde(d.Tool.EntryPath())
	}
	return ix.tilde(d.Entry)
}

func (ix *Index) tilde(p string) string {
	if ix.home != "" && ix.home != "/" && strings.HasPrefix(p, ix.home+string(filepath.Separator)) {
		return "~" + p[len(ix.home):]
	}
	return p
}

// lookPath resolves name on the PATH that getenv reports.
func lookPath(getenv func(string) string, name string) string {
	for _, dir := range filepath.SplitList(getenv("PATH")) {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

func sameFile(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

// lookup returns every document named name (case-insensitive), in priority order. An external
// provider also answers to its command name.
func (ix *Index) lookup(name string) []*doc {
	var out []*doc
	for _, d := range ix.docs {
		if strings.EqualFold(d.Name, name) || (d.Wanted != nil && strings.EqualFold(d.Wanted.SuggestedName, name)) ||
			(d.Provider != nil && strings.EqualFold(d.Provider.Command, name)) {
			out = append(out, d)
		}
	}
	return out
}
