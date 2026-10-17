package registry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/moabualruz/agent-dry/internal/cli"
)

// Semantic tests never reach the real zvec-grep: PATH holds only the stub zg (testdata/registry/
// zgbin) and the fixture provider commands.

// semanticEnv uses the stub zg and records its calls in a log the test reads back.
func semanticEnv(t *testing.T, over map[string]string) (getenv func(string) string, calls func() []string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "zg.log")
	t.Setenv("ZG_STUB_LOG", log)
	t.Setenv("ZG_STUB_OUT", fixture("zg", "agree.md"))
	t.Setenv("ZG_STUB_QUERY", "ok")
	t.Setenv("ZG_STUB_INDEX", "ok")
	m := map[string]string{
		"PATH":             fixture("zgbin") + string(os.PathListSeparator) + fixture("bin"),
		"AGENT_DRY_CONFIG": filepath.Join(t.TempDir(), "config.json"),
	}
	for k, v := range over {
		m[k] = v
	}
	getenv = testEnv(t, m)
	calls = func() []string {
		raw, err := os.ReadFile(log)
		if err != nil {
			return nil
		}
		return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	}
	return getenv, calls
}

func corpusOf(getenv func(string) string) string {
	return filepath.Join(getenv("AGENT_DRY_STATE"), CorpusDir)
}

func cardList(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, k := range cardKinds {
		m, _ := filepath.Glob(filepath.Join(dir, k, "*.md"))
		for _, p := range m {
			out = append(out, k+"/"+filepath.Base(p))
		}
	}
	sort.Strings(out)
	return out
}

func TestCorpus(t *testing.T) {
	getenv := testEnv(t, map[string]string{"PATH": fixture("bin")})
	loadFixture(t, getenv)
	dir := corpusOf(getenv)
	want := []string{
		"provider/dupe-finder.md", "provider/sym-rename.md", "provider/token-meter.md",
		"tool/check-no-dashes.md", "tool/host-run.md", "tool/pr-flow.md",
		"wanted/named-jq-filters.md", "wanted/node-environment-shim.md",
		"skill/release-judgment.md",
	}
	sort.Strings(want)
	// No card for the retired tool, installed skills or project commands.
	if got := cardList(t, dir); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("cards\n got %v\nwant %v", got, want)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "tool", "host-run.md"))
	card := string(raw)
	for _, w := range []string{"# host-run\n\n", "\nkind: script global (agent-dry tool)\n", "\nentry: host-run (tools/host-run/host-run)\n",
		"\nsummary: Use when ", "\nintents: ssh to mkh-pc; "} {
		if !strings.Contains(card, w) {
			t.Errorf("host-run card lacks %q:\n%s", w, card)
		}
	}
	raw, _ = os.ReadFile(filepath.Join(dir, "provider", "dupe-finder.md"))
	for _, w := range []string{"# dupe-finder\n", "kind: external global (CLI from upstream:https://example.com/dupe-finder, MIT)",
		"entry: dupe-finder (install: brew install dupe-finder)", "notes: Fixture entry"} {
		if !strings.Contains(string(raw), w) {
			t.Errorf("dupe-finder card lacks %q:\n%s", w, raw)
		}
	}
	raw, _ = os.ReadFile(filepath.Join(dir, "wanted", "node-environment-shim.md"))
	if !strings.Contains(string(raw), "entry: build node-env (script)") {
		t.Errorf("wanted card:\n%s", raw)
	}
	if strings.Contains(card, fixtures) {
		t.Errorf("card holds a host path:\n%s", card)
	}
}

func TestCorpusPrune(t *testing.T) {
	root := copyTree(t, fixture("root"))
	getenv := testEnv(t, map[string]string{"AGENT_DRY_ROOT": root, "PATH": fixture("bin")})
	load := func() *Index {
		t.Helper()
		ix, err := Load(getenv, Options{NoProject: true})
		if err != nil {
			t.Fatal(err)
		}
		return ix
	}
	load()
	dir := corpusOf(getenv)
	stray := filepath.Join(dir, "tool", "stray.md")
	os.WriteFile(stray, []byte("# stray\n"), 0o600)
	kept := filepath.Join(dir, "tool", "pr-flow.md")
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	os.Chtimes(kept, old, old)

	// A removed tool loses its card, a stray card goes, an unchanged card is not rewritten.
	if err := os.RemoveAll(filepath.Join(root, "tools", "host-run")); err != nil {
		t.Fatal(err)
	}
	if ix := load(); ix.fromCache {
		t.Fatal("removed tool did not rebuild")
	}
	for _, p := range []string{filepath.Join(dir, "tool", "host-run.md"), stray} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s not pruned", p)
		}
	}
	if info, err := os.Stat(kept); err != nil || !info.ModTime().Equal(old) {
		t.Errorf("unchanged card rewritten: %v", err)
	}
	if pending, _ := pendingState(dir); pending {
		t.Error("refresh marked without a corpus index")
	}

	// With an index, a card change marks a refresh; a rebuild that changes no card does not.
	os.Mkdir(filepath.Join(dir, zvecIndexDir), 0o700)
	pj := filepath.Join(root, ProvidersFile)
	raw, _ := os.ReadFile(pj)
	os.WriteFile(pj, []byte(strings.Replace(string(raw), `"offline usage json"`, `"weekly spend digest"`, 1)), 0o644)
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(pj, future, future)
	load()
	if pending, failure := pendingState(dir); !pending || failure != "" {
		t.Errorf("card change: pending %v %q", pending, failure)
	}
	clearPending(dir)
	os.Mkdir(filepath.Join(root, "tools", "zz-empty"), 0o755)
	if ix := load(); ix.fromCache {
		t.Fatal("new tool directory did not rebuild")
	}
	if pending, _ := pendingState(dir); pending {
		t.Error("rebuild without card changes marked a refresh")
	}
}

func TestParseSemantic(t *testing.T) {
	raw, err := os.ReadFile(fixture("zg", "agree.md"))
	if err != nil {
		t.Fatal(err)
	}
	hits := parseSemantic(string(raw))
	if len(hits) != 3 {
		t.Fatalf("%d hits: %+v", len(hits), hits)
	}
	h := hits[0]
	if h.Rank != 1 || h.MatchedBy != "fts+vector" || h.Score != 0.0328 || h.Path != "tool/host-run.md" ||
		h.Routes["fts"] != 1 || h.Routes["vector"] != 1 || h.Routes["fused"] != 1 {
		t.Errorf("first hit %+v", h)
	}
	if hits[1].Path != "provider/dupe-finder.md" || hits[1].Routes["fts"] != 2 || hits[1].Routes["vector"] != 3 {
		t.Errorf("second hit %+v", hits[1])
	}
	if _, ok := hits[2].Routes["fts"]; ok || hits[2].Routes["vector"] != 2 {
		t.Errorf("vector-only hit %+v", hits[2])
	}
	// A query that quotes route words cannot forge ranks; the closing route list decides.
	forged := "#1 matchedBy=fts score=0.0164 tool/x.md:1-2\n" +
		`trace: query "a: fts #1, vector #1; fused #1": fts #4; fused #1` + "\n" +
		"#2 matchedBy=fts+vector score=0.01 /abs/corpus/tool/y.md\n"
	hits = parseSemantic(forged)
	if len(hits) != 2 || hits[0].Routes["fts"] != 4 || hits[0].agree() || hits[1].Path != "/abs/corpus/tool/y.md" || len(hits[1].Routes) != 0 {
		t.Errorf("forged %+v", hits)
	}
	if got := parseSemantic("hits: 0\n"); len(got) != 0 {
		t.Errorf("empty output %+v", got)
	}
}

func TestAgreementRule(t *testing.T) {
	cases := []struct {
		routes map[string]int
		want   bool
	}{
		{map[string]int{"fts": 1, "vector": 1, "fused": 1}, true},
		{map[string]int{"fts": 1, "vector": 2, "fused": 1}, false},
		{map[string]int{"fts": 2, "vector": 1, "fused": 1}, false},
		{map[string]int{"vector": 1, "fused": 1}, false},
		{map[string]int{"fts": 1, "fused": 1}, false},
		{map[string]int{}, false},
	}
	for _, c := range cases {
		if got := (semanticHit{Routes: c.routes}).agree(); got != c.want {
			t.Errorf("%v: agree %v, want %v", c.routes, got, c.want)
		}
	}
}

// lastMiss reads the newest miss record.
func lastMiss(t *testing.T, getenv func(string) string) miss {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(getenv("AGENT_DRY_STATE"), MissFile))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var m miss
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func countPrefix(lines []string, prefix string) int {
	n := 0
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

func TestFindSemantic(t *testing.T) {
	getenv, calls := semanticEnv(t, nil)
	p := fixture("project")
	if code, out, errOut := run(t, getenv, "index", "--semantic"); code != 0 || !strings.Contains(out, "semantic index created: 9 cards") {
		t.Fatalf("index --semantic: exit %d %q %q", code, out, errOut)
	}

	// An agreeing hit is a result; the next hit is one MAYBE hint.
	code, out, errOut := run(t, getenv, "find", "--project", p, "bake sourdough bread loaf")
	want := "host-run  Use when running a command on a configured host over ssh.  [script global] " + fixture("root", "tools", "host-run", "host-run") + "\n" +
		"MAYBE dupe-finder: Use when finding copy-pasted code blocks across several repositories.\n"
	if code != cli.ExitOK || errOut != "" {
		t.Errorf("agree: exit %d err %q", code, errOut)
	}
	if lines := strings.Split(out, "\n"); len(lines) != 3 || !strings.HasPrefix(lines[0], "host-run  ") || !strings.HasPrefix(lines[1], "MAYBE dupe-finder: ") {
		t.Errorf("agree output\n got %q\nwant shape %q", out, want)
	}
	if got := calls(); countPrefix(got, "query bake sourdough bread loaf --mode auto --limit 3 --trace") != 1 {
		t.Errorf("zg calls %v", got)
	}
	if _, err := os.Stat(filepath.Join(getenv("AGENT_DRY_STATE"), MissFile)); err == nil {
		t.Error("a semantic match logged a miss")
	}
	_, out, _ = run(t, getenv, "find", "--json", "--project", p, "bake sourdough bread loaf")
	var v struct {
		Results []Result
		Hints   []Hint
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil || len(v.Results) != 1 || len(v.Hints) != 1 {
		t.Fatalf("json: %v %s", err, out)
	}
	if r := v.Results[0]; r.Name != "host-run" || r.Source != srcSemantic || !r.Semantic || r.Score != 0.0328 {
		t.Errorf("json result %+v", r)
	}
	if !strings.Contains(out, `"semantic":true`) || v.Hints[0].Kind != "maybe" || v.Hints[0].Name != "dupe-finder" {
		t.Errorf("json %s", out)
	}

	// A wanted family hit becomes WANTED; an unknown card is ignored; MAYBE stops at one; a miss
	// that recall answered without agreement is logged with semantic true.
	t.Setenv("ZG_STUB_OUT", fixture("zg", "maybe.md"))
	code, out, _ = run(t, getenv, "find", "--project", p, "translate french poetry")
	if code != cli.ExitNegative || !strings.HasPrefix(out, "WANTED node-environment-shim: build node-env before use\nMAYBE release-judgment: ") ||
		strings.Count(out, "MAYBE ") != 1 || strings.Contains(out, "MAYBE pr-flow: ") || strings.Contains(out, "token-meter") || strings.Contains(out, "gone") {
		t.Errorf("maybe: exit %d\n%s", code, out)
	}
	if m := lastMiss(t, getenv); m.Query != "translate french poetry" || !m.Semantic {
		t.Errorf("miss record %+v", m)
	}

	// No zg call when the lexical pass matched.
	before := len(calls())
	if code, _, _ = run(t, getenv, "find", "--project", p, "watch pr checks"); code != 0 || len(calls()) != before {
		t.Errorf("lexical hit called zg: exit %d calls %v", code, calls()[before:])
	}
	_, _, errOut = run(t, getenv, "find", "--debug", "--project", p, "watch pr checks")
	if errOut != "agent-dry: debug: semantic recall skipped: lexical match\n" {
		t.Errorf("debug on hit %q", errOut)
	}
}

func TestFindSemanticFallback(t *testing.T) {
	getenv, calls := semanticEnv(t, nil)
	p := fixture("project")
	if code, _, errOut := run(t, getenv, "index", "--semantic"); code != 0 {
		t.Fatalf("index --semantic: %q", errOut)
	}
	cases := []struct {
		name, mode, query, debug string
	}{
		{"failure", "fail", "book a flight to paris next week", "semantic recall failed: zg query: exit status 1: Error: stub query failure"},
		{"timeout", "slow", "check the weather forecast", "semantic recall failed: zg query timed out after 300ms"},
	}
	saved := semanticTimeout
	semanticTimeout = 300 * time.Millisecond
	t.Cleanup(func() { semanticTimeout = saved })
	for _, c := range cases {
		t.Setenv("ZG_STUB_QUERY", c.mode)
		start := time.Now()
		code, out, errOut := run(t, getenv, "find", "--project", p, c.query)
		if el := time.Since(start); el > 3*time.Second {
			t.Errorf("%s took %s", c.name, el)
		}
		if code != cli.ExitNegative || out != "" || errOut != "" {
			t.Errorf("%s: exit %d out %q err %q", c.name, code, out, errOut)
		}
		if m := lastMiss(t, getenv); m.Semantic {
			t.Errorf("%s: miss logged semantic true", c.name)
		}
		_, _, errOut = run(t, getenv, "find", "--debug", "--project", p, c.query+" again")
		if !strings.Contains(errOut, c.debug) || strings.Count(errOut, "\n") != 1 {
			t.Errorf("%s debug %q, want %q", c.name, errOut, c.debug)
		}
	}

	// No zg on PATH: lexical only, no call.
	noZg := func(k string) string {
		if k == "PATH" {
			return fixture("bin")
		}
		return getenv(k)
	}
	before := len(calls())
	code, out, errOut := run(t, noZg, "find", "--debug", "--project", p, "summarize this quarterly sales spreadsheet")
	if code != cli.ExitNegative || out != "" || errOut != "agent-dry: debug: semantic recall off: zg not on PATH\n" || len(calls()) != before {
		t.Errorf("no zg: exit %d out %q err %q", code, out, errOut)
	}
	if m := lastMiss(t, noZg); m.Semantic {
		t.Error("no zg: miss logged semantic true")
	}

	// zg present but no corpus index: never called, never creates one.
	fresh, freshCalls := semanticEnv(t, nil)
	code, _, errOut = run(t, fresh, "find", "--debug", "--project", p, "bake sourdough bread loaf")
	if code != cli.ExitNegative || !strings.Contains(errOut, "no corpus index; run agent-dry index --semantic") || len(freshCalls()) != 0 {
		t.Errorf("no index: exit %d err %q calls %v", code, errOut, freshCalls())
	}
	if semanticIndexed(corpusOf(fresh)) {
		t.Error("find created a corpus index")
	}
}

func TestIndexSemantic(t *testing.T) {
	root := copyTree(t, fixture("root"))
	getenv, calls := semanticEnv(t, map[string]string{"AGENT_DRY_ROOT": root})
	dir := corpusOf(getenv)

	// The explicit run creates the index directly with the default model, then checks coverage.
	code, out, errOut := run(t, getenv, "index", "--semantic")
	if code != 0 || !strings.Contains(out, "semantic index created: 9 cards") || errOut != "" {
		t.Fatalf("create: exit %d %q %q", code, out, errOut)
	}
	if got := calls(); len(got) != 2 || got[0] != "index "+dir+" --mode direct --embedding "+DefaultEmbedding || got[1] != "status "+dir+" --mode auto" {
		t.Errorf("create calls %v", got)
	}
	// An existing index is refreshed through --mode auto; --rebuild rebuilds it.
	if _, out, _ = run(t, getenv, "index", "--semantic"); !strings.Contains(out, "semantic index refreshed") || calls()[2] != "index "+dir+" --mode auto" {
		t.Errorf("refresh: %q %v", out, calls())
	}
	if _, out, _ = run(t, getenv, "index", "--semantic", "--rebuild"); !strings.Contains(out, "semantic index rebuilt") ||
		calls()[4] != "index "+dir+" --mode auto --rebuild --embedding "+DefaultEmbedding {
		t.Errorf("rebuild: %q %v", out, calls())
	}

	// Admission loads rewrite cards but never run zg; the refresh waits for a recall or index.
	n := len(calls())
	pj := filepath.Join(root, ProvidersFile)
	raw, _ := os.ReadFile(pj)
	edit := func(from, to string) {
		os.WriteFile(pj, []byte(strings.Replace(string(raw), from, to, 1)), 0o644)
		future := time.Now().Add(time.Duration(len(calls())+2) * time.Second)
		os.Chtimes(pj, future, future)
	}
	edit(`"offline usage json"`, `"weekly spend digest"`)
	if _, err := Load(getenv, Options{NoProject: true, NoInstalled: true}); err != nil {
		t.Fatal(err)
	}
	if pending, _ := pendingState(dir); !pending || len(calls()) != n {
		t.Fatalf("admission load: pending %v calls %v", pending, calls()[n:])
	}
	if _, _, _ = run(t, getenv, "find", "watch pr checks"); len(calls()) != n {
		t.Errorf("lexical hit refreshed: %v", calls()[n:])
	}
	run(t, getenv, "find", "bake sourdough bread loaf")
	if got := calls()[n:]; len(got) != 2 || got[0] != "index "+dir+" --mode auto" || !strings.HasPrefix(got[1], "query ") {
		t.Errorf("recall refresh calls %v", got)
	}
	if pending, _ := pendingState(dir); pending {
		t.Error("marker kept after a refresh")
	}

	// A failed automatic refresh is recorded and not retried by find; agent-dry index retries.
	t.Setenv("ZG_STUB_INDEX", "fail")
	edit(`"offline usage json"`, `"monthly spend digest"`)
	n = len(calls())
	_, _, errOut = run(t, getenv, "find", "--debug", "translate french poetry")
	if !strings.Contains(errOut, "refresh failed (zg index: exit status 1: Error: stub index failure), querying the previous index") {
		t.Errorf("refresh failure debug %q", errOut)
	}
	if pending, failure := pendingState(dir); !pending || !strings.HasPrefix(failure, "failed ") {
		t.Errorf("failure marker %v %q", pending, failure)
	}
	run(t, getenv, "find", "book a flight to paris next week")
	if got := calls()[n:]; countPrefix(got, "index ") != 1 || countPrefix(got, "query ") != 2 {
		t.Errorf("failed refresh retried by find: %v", got)
	}
	if code, _, errOut = run(t, getenv, "index"); code != 0 || !strings.Contains(errOut, "semantic index refresh failed") {
		t.Errorf("index with failing zg: exit %d %q", code, errOut)
	}
	t.Setenv("ZG_STUB_INDEX", "ok")
	if code, out, _ = run(t, getenv, "index"); code != 0 || !strings.Contains(out, "semantic index refreshed at ") {
		t.Errorf("index retry: exit %d %q", code, out)
	}

	// The model comes from config semantic.embedding; a bad value or a missing zg is exit 3.
	other, otherCalls := semanticEnv(t, nil)
	os.WriteFile(other("AGENT_DRY_CONFIG"), []byte(`{"semantic": {"embedding": "local/other-model"}}`), 0o600)
	if code, _, _ = run(t, other, "index", "--semantic"); code != 0 || otherCalls()[0] != "index "+corpusOf(other)+" --mode direct --embedding local/other-model" {
		t.Errorf("config model: exit %d %v", code, otherCalls())
	}
	bad, _ := semanticEnv(t, nil)
	os.WriteFile(bad("AGENT_DRY_CONFIG"), []byte(`{"semantic": {"embedding": "--allow-remote"}}`), 0o600)
	if code, _, errOut = run(t, bad, "index", "--semantic"); code != cli.ExitEnv || !strings.Contains(errOut, "is not a model name") {
		t.Errorf("bad model: exit %d %q", code, errOut)
	}
	noZg := testEnv(t, map[string]string{"PATH": fixture("bin")})
	if code, _, errOut = run(t, noZg, "index", "--semantic"); code != cli.ExitEnv || !strings.Contains(errOut, "needs zvec-grep (zg) on PATH") {
		t.Errorf("no zg: exit %d %q", code, errOut)
	}
	if semanticIndexed(corpusOf(noZg)) {
		t.Error("index without zg created an index")
	}
}

func TestSemanticChecks(t *testing.T) {
	getenv, _ := semanticEnv(t, nil)
	dir := corpusOf(getenv)
	check := func(env func(string) string, name string) SemanticCheck {
		t.Helper()
		for _, c := range SemanticChecks(env) {
			if c.Status != "PASS" && c.Status != "WARN" {
				t.Errorf("%s status %s", c.Name, c.Status)
			}
			if c.Name == name {
				return c
			}
		}
		t.Fatalf("no %s check", name)
		return SemanticCheck{}
	}
	noZg := func(k string) string {
		if k == "PATH" {
			return fixture("bin")
		}
		return getenv(k)
	}
	if c := check(noZg, "zvec-grep"); c.Status != "WARN" || !strings.Contains(c.Detail, "zg not on PATH") {
		t.Errorf("no zg %+v", c)
	}
	if c := check(getenv, "zvec-grep"); c.Status != "PASS" || c.Detail != "zg at "+fixture("zgbin", "zg") {
		t.Errorf("zg %+v", c)
	}
	states := []struct {
		setup  func()
		status string
		detail string
	}{
		{func() {}, "WARN", "no corpus at "},
		{func() { loadFixture(t, getenv) }, "WARN", "9 cards at " + dir + ", not indexed; run agent-dry index --semantic"},
		{func() { os.Mkdir(filepath.Join(dir, zvecIndexDir), 0o700) }, "PASS", "9 cards at " + dir + " indexed"},
		{func() { markPending(dir, "") }, "WARN", "refresh pending"},
		{func() { markPending(dir, "failed 2026-10-17T00:00:00Z: zg index timed out after 10s") }, "WARN", "last refresh failed 2026-10-17T00:00:00Z"},
	}
	for _, s := range states {
		s.setup()
		if c := check(getenv, "semantic index"); c.Status != s.status || !strings.Contains(c.Detail, s.detail) {
			t.Errorf("state %+v, want %s %q", c, s.status, s.detail)
		}
	}
}
