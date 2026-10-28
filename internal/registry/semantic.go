package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/moabualruz/rice-agent/internal/paths"
)

// Semantic recall delegates to the owner's zvec-grep (CLI zg): rice-agent never embeds or grows its
// own search. The lexical scorer stays the deterministic match decision; zvec-grep runs only after
// a lexical miss, only over an index the owner created with `rice-agent index --semantic`, and its
// hit counts as a match only when the FTS and vector routes both rank it first.

// DefaultEmbedding is the model a new corpus index uses unless config semantic.embedding names one.
const DefaultEmbedding = "local/potion-code-16m-v2"

const srcSemantic = "semantic"

// Bounds for zg calls. Tests shorten them.
var (
	semanticTimeout = 3 * time.Second  // zg query during find
	refreshTimeout  = 10 * time.Second // incremental zg index of a changed corpus
	indexTimeout    = 10 * time.Minute // explicit index --semantic (may fetch the model once)
	killGrace       = 2 * time.Second
)

// maxSemanticMaybes bounds the MAYBE hint lines. Ranks cannot tell an unrelated query from a paraphrase, so one
// line keeps the useful pointer while halving the noise.
const maxSemanticMaybes = 1

// zgOutputCap bounds what is kept of zg's output.
const zgOutputCap = 256 << 10

type capWriter struct {
	buf bytes.Buffer
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := zgOutputCap - w.buf.Len(); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		w.buf.Write(p[:room])
	}
	return len(p), nil
}

// runZg runs zg with args in dir within timeout. The child runs in its own process group so a
// timeout stops everything it started. stdout and stderr are returned separately.
func runZg(zg, dir string, timeout time.Duration, args ...string) (stdout, stderr string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, zg, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	var out, errb capWriter
	cmd.Stdout, cmd.Stderr = &out, &errb
	ownGroup(cmd)
	cmd.WaitDelay = killGrace
	err = cmd.Run()
	if ctx.Err() != nil {
		killGroup(cmd)
		return out.buf.String(), errb.buf.String(), fmt.Errorf("zg %s timed out after %s", args[0], timeout)
	}
	if err != nil {
		msg := lastLine(errb.buf.String())
		if msg == "" {
			msg = lastLine(out.buf.String())
		}
		if msg != "" {
			err = fmt.Errorf("zg %s: %v: %s", args[0], err, truncate(redact(msg), 160))
		} else {
			err = fmt.Errorf("zg %s: %v", args[0], err)
		}
	}
	return out.buf.String(), errb.buf.String(), err
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// semanticHit is one parsed zg query hit.
type semanticHit struct {
	Rank      int
	MatchedBy string
	Score     float64
	Path      string // as printed, relative to the corpus root
	Routes    map[string]int
}

// agree is the match rule: the FTS and vector routes both rank the hit first.
func (h semanticHit) agree() bool { return h.Routes["fts"] == 1 && h.Routes["vector"] == 1 }

var (
	reHit   = regexp.MustCompile(`^#(\d+)\s+matchedBy=(\S+)\s+score=(\S+)\s+(\S.*?)(?::\d+(?:-\d+)?)?\s*$`)
	reTrace = regexp.MustCompile(`^\s*trace:\s*(.*)$`)
	// The route list closes the trace line, after the quoted query, so a query that contains
	// route-like words cannot forge a rank.
	reRoutes = regexp.MustCompile(`((?:(?:fts|vector|fused|hybrid)\s+#\d+\s*[,;]?\s*)+)$`)
	reRoute  = regexp.MustCompile(`(fts|vector|fused|hybrid)\s+#(\d+)`)
)

// parseSemantic reads zg's agent markdown: `#N matchedBy=... score=... <path>:<lines>` lines,
// each followed by its `trace: query "...": fts #1, vector #1; fused #1` line. Other lines are
// ignored, so additions to the format do not break parsing.
func parseSemantic(out string) []semanticHit {
	var hits []semanticHit
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if m := reHit.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			rank, _ := strconv.Atoi(m[1])
			score, _ := strconv.ParseFloat(m[3], 64)
			hits = append(hits, semanticHit{Rank: rank, MatchedBy: m[2], Score: score, Path: strings.TrimSpace(m[4]), Routes: map[string]int{}})
			continue
		}
		if m := reTrace.FindStringSubmatch(line); m != nil && len(hits) > 0 {
			last := &hits[len(hits)-1]
			if rl := reRoutes.FindStringSubmatch(strings.TrimSpace(m[1])); rl != nil {
				for _, r := range reRoute.FindAllStringSubmatch(rl[1], -1) {
					n, _ := strconv.Atoi(r[2])
					if old, ok := last.Routes[r[1]]; !ok || n < old {
						last.Routes[r[1]] = n
					}
				}
			}
		}
	}
	return hits
}

// recallResult is what semantic recall adds to a lexical miss.
type recallResult struct {
	Ran     bool     // zg answered and its output was parsed
	Results []Result // hits both routes rank first
	Maybes  []Hint   // other hits, at most maxSemanticMaybes
	Wanted  []Hint   // agreeing hits on wanted families
	Note    string   // one line for --debug
}

// recall runs zg over the corpus index for a query the lexical pass missed. Every failure leaves
// the lexical result alone and is reported only in Note.
func (ix *Index) recall(query string) recallResult {
	dir := ix.corpusDir()
	zg := lookPath(ix.getenv, "zg")
	switch {
	case zg == "":
		return recallResult{Note: "semantic recall off: zg not on PATH"}
	case !semanticIndexed(dir):
		return recallResult{Note: "semantic recall off: no corpus index; run rice-agent index --semantic"}
	}
	var notes []string
	if err := ix.autoRefresh(zg, dir); err != nil {
		notes = append(notes, "refresh failed ("+err.Error()+"), querying the previous index")
	}
	out, _, err := runZg(zg, dir, semanticTimeout, "query", redact(query), "--mode", "auto", "--limit", "3", "--trace")
	if err != nil {
		notes = append(notes, "semantic recall failed: "+err.Error())
		return recallResult{Note: strings.Join(notes, "; ")}
	}
	byCard := map[string]*doc{}
	for _, d := range ix.docs {
		if rel := cardRel(d); rel != "" {
			if _, ok := byCard[rel]; !ok {
				byCard[rel] = d
			}
		}
	}
	r := recallResult{Ran: true}
	hits := parseSemantic(out)
	seen := map[*doc]bool{}
	for _, h := range hits {
		p := filepath.ToSlash(h.Path)
		if filepath.IsAbs(h.Path) {
			if rel, err := filepath.Rel(dir, h.Path); err == nil {
				p = filepath.ToSlash(rel)
			}
		}
		d := byCard[strings.TrimPrefix(p, "./")]
		if d == nil || seen[d] {
			continue
		}
		seen[d] = true
		switch {
		case d.Wanted != nil:
			if h.agree() {
				r.Wanted = append(r.Wanted, wantedHint(d.Wanted))
			}
		case h.agree():
			res := ix.result(d, h.Score)
			res.Source, res.Semantic = srcSemantic, true
			r.Results = append(r.Results, res)
		case len(r.Maybes) < maxSemanticMaybes:
			r.Maybes = append(r.Maybes, Hint{
				Kind: "maybe", Name: d.Name, Summary: d.Summary,
				Text: fmt.Sprintf("MAYBE %s: %s", d.Name, truncate(d.Summary, summaryRune)),
			})
		}
	}
	notes = append(notes, fmt.Sprintf("semantic recall: %d hits, %d agreed", len(hits), len(r.Results)+len(r.Wanted)))
	r.Note = strings.Join(notes, "; ")
	return r
}

// autoRefresh updates the corpus index after cards changed, bounded by refreshTimeout. It runs
// only when a refresh is pending and the last automatic attempt did not fail; a failure is kept
// in the marker so later lookups do not pay for it again (rice-agent index retries).
func (ix *Index) autoRefresh(zg, dir string) error {
	pending, failure := pendingState(dir)
	if !pending || failure != "" {
		return nil
	}
	return refreshCorpus(zg, dir, refreshTimeout)
}

// refreshCorpus runs an incremental `zg index` on an existing corpus index; the index keeps its
// stored embedding schema. It uses --mode auto: once a zvec-grep server has served a query for the
// corpus it holds the write lease, and a direct write is refused (ENGINE.DAEMON_LEASE_ACTIVE).
func refreshCorpus(zg, dir string, timeout time.Duration) error {
	if _, _, err := runZg(zg, dir, timeout, "index", dir, "--mode", "auto"); err != nil {
		markPending(dir, "failed "+time.Now().UTC().Format(time.RFC3339)+": "+err.Error())
		return err
	}
	clearPending(dir)
	return nil
}

// syncCorpus writes the corpus after a cache rebuild and marks an existing index for refresh
// when cards changed. Admission loads write it too (the files are deterministic) but never run zg.
func (ix *Index) syncCorpus(c *cacheFile) {
	var all []*doc
	for _, ds := range [][]*doc{c.Tools, c.Skills, c.Providers, c.Wanted} {
		all = append(all, ds...)
	}
	dir := ix.corpusDir()
	changed, _, err := writeCorpus(dir, ix.root, all)
	if err != nil {
		ix.warnings = append(ix.warnings, fmt.Sprintf("corpus not written: %v", err))
		return
	}
	if changed && semanticIndexed(dir) {
		markPending(dir, "")
	}
}

var reCoverage = regexp.MustCompile(`(\d+)\s*/\s*(\d+)\s+files`)

// indexedFiles asks zg status how many corpus files the index covers, or -1 when it cannot tell.
// It catches cards zvec-grep's default ignore rules leave out.
func indexedFiles(zg, dir string) int {
	out, _, err := runZg(zg, dir, 10*time.Second, "status", dir, "--mode", "auto")
	if err != nil {
		return -1
	}
	m := reCoverage.FindStringSubmatch(out)
	if m == nil {
		return -1
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

var reModel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`)

// embeddingModel is config semantic.embedding, or DefaultEmbedding when unset.
func embeddingModel(getenv func(string) string) (string, error) {
	raw, err := os.ReadFile(paths.ConfigPath(getenv))
	if errors.Is(err, os.ErrNotExist) {
		return DefaultEmbedding, nil
	}
	if err != nil {
		return "", err
	}
	var c struct {
		Semantic struct {
			Embedding string `json:"embedding"`
		} `json:"semantic"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", fmt.Errorf("%s: %w", paths.ConfigPath(getenv), err)
	}
	m := strings.TrimSpace(c.Semantic.Embedding)
	if m == "" {
		return DefaultEmbedding, nil
	}
	if !reModel.MatchString(m) {
		return "", fmt.Errorf("%s: semantic.embedding %q is not a model name", paths.ConfigPath(getenv), m)
	}
	return m, nil
}

// SemanticCheck is one doctor line about semantic recall. Status is PASS or WARN, never FAIL:
// find works without zvec-grep.
type SemanticCheck struct {
	Status string
	Name   string
	Detail string
}

// SemanticChecks reports zg presence and the corpus index state for rice-agent doctor.
func SemanticChecks(getenv func(string) string) []SemanticCheck {
	var out []SemanticCheck
	add := func(status, name, format string, a ...any) {
		out = append(out, SemanticCheck{Status: status, Name: name, Detail: fmt.Sprintf(format, a...)})
	}
	if zg := lookPath(getenv, "zg"); zg != "" {
		add("PASS", "zvec-grep", "zg at %s", zg)
	} else {
		add("WARN", "zvec-grep", "zg not on PATH; find uses lexical ranking only")
	}
	dir := filepath.Join(paths.StateDir(getenv), CorpusDir)
	cards := countCards(dir)
	pending, failure := pendingState(dir)
	switch {
	case cards == 0:
		add("WARN", "semantic index", "no corpus at %s yet; rice-agent index writes it", dir)
	case !semanticIndexed(dir):
		add("WARN", "semantic index", "%d cards at %s, not indexed; run rice-agent index --semantic to enable recall", cards, dir)
	case failure != "":
		add("WARN", "semantic index", "%d cards at %s; last refresh %s; run rice-agent index --semantic", cards, dir, failure)
	case pending:
		add("WARN", "semantic index", "%d cards at %s; refresh pending, the next recall or rice-agent index runs it", cards, dir)
	default:
		add("PASS", "semantic index", "%d cards at %s indexed", cards, dir)
	}
	return out
}
