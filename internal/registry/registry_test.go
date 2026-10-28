package registry

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/moabualruz/rice-agent/docs"
	"github.com/moabualruz/rice-agent/internal/cli"
)

var fixtures = mustAbs(filepath.Join("..", "..", "testdata", "registry"))

func mustAbs(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		panic(err)
	}
	return a
}

func fixture(parts ...string) string { return filepath.Join(append([]string{fixtures}, parts...)...) }

// testEnv points every source at the fixtures and the state at a fresh temporary directory.
// Values in over replace the defaults; an empty value unsets the variable.
func testEnv(t *testing.T, over map[string]string) func(string) string {
	t.Helper()
	m := map[string]string{
		"AGENT_DRY_ROOT":  fixture("root"),
		"AGENT_DRY_STATE": t.TempDir(),
		"HOME":            fixture("home"),
		"PATH":            os.Getenv("PATH"),
	}
	for k, v := range over {
		m[k] = v
	}
	return func(k string) string { return m[k] }
}

func loadFixture(t *testing.T, getenv func(string) string) *Index {
	t.Helper()
	ix, err := Load(getenv, Options{ProjectRoot: fixture("project")})
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

func docsBySource(ix *Index) map[string]map[string]*doc {
	out := map[string]map[string]*doc{}
	for _, d := range ix.docs {
		if out[d.Source] == nil {
			out[d.Source] = map[string]*doc{}
		}
		out[d.Source][d.Name] = d
	}
	return out
}

// run invokes a registered command with captured output.
func run(t *testing.T, getenv func(string) string, args ...string) (int, string, string) {
	t.Helper()
	c, ok := cli.Lookup(args[0])
	if !ok {
		t.Fatalf("command %s not registered", args[0])
	}
	var out, errb bytes.Buffer
	code := c.Run(&cli.Env{Args: args[1:], Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb, Getenv: getenv})
	return code, out.String(), errb.String()
}

func TestSources(t *testing.T) {
	for _, mode := range []string{"just on PATH", "justfile parser"} {
		t.Run(mode, func(t *testing.T) {
			over := map[string]string{}
			if mode == "just on PATH" {
				if _, err := exec.LookPath("just"); err != nil {
					t.Skip("just not installed")
				}
			} else {
				over["PATH"] = ""
			}
			ix := loadFixture(t, testEnv(t, over))
			by := docsBySource(ix)
			want := map[string][]string{
				srcAgentDry: {"check-no-dashes", "host-run", "pr-flow", "old-scan", "release-judgment"},
				srcSkill:    {"tdd-loop", "diagram-maker", "claude-notes", "proj-style", "proj-review"},
				srcJust:     {"build", "migrate"},
				srcNPM:      {"lint:css", "test"},
				srcMake:     {"deploy-docs", "clean"},
				srcScripts:  {"rotate-logs"},
				srcProjFile: {"seed-demo-data"},
				srcWanted:   {"node-environment-shim", "named-jq-filters"},
				srcExternal: {"dupe-finder", "token-meter", "sym-rename"},
			}
			for src, names := range want {
				if len(by[src]) != len(names) {
					t.Errorf("%s: got %d entries %v, want %v", src, len(by[src]), keys(by[src]), names)
				}
				for _, n := range names {
					if by[src][n] == nil {
						t.Errorf("%s: missing %s", src, n)
					}
				}
			}
			checks := []struct{ src, name, field, want string }{
				{srcJust, "build", "summary", "Build the fixture binary for release."},
				{srcJust, "migrate", "entry", "just migrate"},
				{srcNPM, "lint:css", "entry", "npm run lint:css"},
				{srcMake, "deploy-docs", "summary", "Publish the documentation site."},
				{srcMake, "clean", "summary", "Remove build artifacts and caches"},
				{srcScripts, "rotate-logs", "summary", "Rotate and compress application log files older than a week."},
				{srcScripts, "rotate-logs", "entry", "scripts/rotate-logs.sh"},
				{srcProjFile, "seed-demo-data", "entry", "just seed"},
				{srcSkill, "diagram-maker", "summary", "Draw architecture diagrams as standalone HTML. Use when visualizing system architecture."},
				{srcSkill, "tdd-loop", "summary", "Test-driven development loop. Use when building features test-first with red-green-refactor."},
				{srcSkill, "claude-notes", "summary", "Keep session notes; use when summarizing a long session's decisions."},
				{srcSkill, "proj-style", "scope", "project"},
				{srcSkill, "tdd-loop", "scope", "global"},
				{srcAgentDry, "release-judgment", "kind", "skill"},
				{srcAgentDry, "pr-flow", "kind", "cli"},
			}
			for _, c := range checks {
				d := by[c.src][c.name]
				if d == nil {
					continue
				}
				got := map[string]string{"summary": d.Summary, "entry": d.Entry, "scope": d.Scope, "kind": d.Kind}[c.field]
				if got != c.want {
					t.Errorf("%s %s %s = %q, want %q", c.src, c.name, c.field, got, c.want)
				}
			}
			if !by[srcAgentDry]["old-scan"].Hidden {
				t.Error("retired old-scan is not hidden")
			}
			if ix.projectName != "project" {
				t.Errorf("project name %q", ix.projectName)
			}
		})
	}
}

func keys(m map[string]*doc) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestNoProject(t *testing.T) {
	ix, err := Load(testEnv(t, nil), Options{ProjectRoot: fixture("project"), NoProject: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range ix.docs {
		if strings.HasPrefix(d.Source, "project:") || d.Scope == "project" {
			t.Errorf("NoProject loaded %s %s", d.Source, d.Name)
		}
	}
}

// TestNoInstalled keeps admission scores independent of the skills a host has installed under HOME.
func TestNoInstalled(t *testing.T) {
	env := testEnv(t, nil)
	with, err := Load(env, Options{NoProject: true})
	if err != nil {
		t.Fatal(err)
	}
	without, err := Load(env, Options{NoProject: true, NoInstalled: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(without.docs) >= len(with.docs) {
		t.Fatalf("NoInstalled kept %d of %d docs; installed skills must be dropped", len(without.docs), len(with.docs))
	}
	for _, d := range without.docs {
		if d.Source == srcSkill && !strings.HasPrefix(d.Path, without.root) {
			t.Errorf("NoInstalled loaded installed skill %s from %s", d.Name, d.Path)
		}
		if d.Source == srcExternal || d.Provider != nil {
			t.Errorf("NoInstalled loaded external provider %s", d.Name)
		}
	}
}

func TestRanking(t *testing.T) {
	ix := loadFixture(t, testEnv(t, nil))
	hits := map[string]string{
		"scan for em dash":                  "check-no-dashes",
		"ssh to mkh-pc run command":         "host-run",
		"watch pr checks":                   "pr-flow",
		"check-no-dashes":                   "check-no-dashes",
		"find en dashes in my files":        "check-no-dashes",
		"run a script on the remote server": "host-run",
		"rotate log files":                  "rotate-logs",
		"seed demo data":                    "seed-demo-data",
		"deploy docs":                       "deploy-docs",
		"database migration":                "migrate",
		"test driven development":           "tdd-loop",
		"architecture diagram":              "diagram-maker",
		"is the release ready to ship":      "release-judgment",
	}
	for q, want := range hits {
		res := ix.Search(q, 5)
		if len(res) == 0 {
			t.Errorf("%q: no result, want %s (best %s)", q, want, best(ix, q))
			continue
		}
		if res[0].Name != want {
			t.Errorf("%q: first %s (%.2f), want %s", q, res[0].Name, res[0].Score, want)
		}
	}
	misses := []string{
		"bake sourdough bread loaf",
		"translate french poetry",
		"watch the football match tonight",
		"book a flight to paris next week",
		"check the weather forecast",
		"summarize this quarterly sales spreadsheet",
	}
	for _, q := range misses {
		if res := ix.Search(q, 5); len(res) != 0 {
			t.Errorf("%q: unrelated query matched %s (%.2f)", q, res[0].Name, res[0].Score)
		}
	}
	for _, r := range ix.Search("scan for em dash legacy dash scan old dash checker", 0) {
		if r.Name == "old-scan" {
			t.Error("retired tool returned by Search")
		}
	}
	for _, r := range ix.Search("source nvm before node", 0) {
		if r.Source == srcWanted {
			t.Error("wanted family returned by Search")
		}
	}
	if got := len(ix.Search("dash ssh pr build test", 2)); got > 2 {
		t.Errorf("limit 2 returned %d", got)
	}
}

// best reports the top score regardless of the threshold, for failure messages.
func best(ix *Index, q string) string {
	pq := ix.prepareQuery(q)
	top, s := "", -1.0
	for _, d := range ix.docs {
		if v := ix.score(d, pq); v > s {
			top, s = d.Name, v
		}
	}
	b, _ := json.Marshal(map[string]any{"name": top, "score": s})
	return string(b)
}

func TestTokenize(t *testing.T) {
	cases := map[string]string{
		"Scan for em-dashes":     "scan em dash",
		"check_no_dashes":        "check dash",
		"watches PR checks":      "watch pr check",
		"Use when policies pass": "policy pass",
		"ssh to mkh-pc":          "ssh mkh pc",
		"processes boxes":        "process box",
	}
	for in, want := range cases {
		if got := strings.Join(tokenize(in), " "); got != want {
			t.Errorf("tokenize(%q) = %q, want %q", in, got, want)
		}
	}
	if j := jaccard([]string{"a", "b", "c"}, []string{"b", "c", "d"}); j != 0.5 {
		t.Errorf("jaccard = %v", j)
	}
}

// canaries are built at run time so no token-shaped literal sits in the source.
func canaries() []string {
	return []string{
		"ghp_" + strings.Repeat("Ab1", 12),
		"github_pat_" + strings.Repeat("Z9y", 9),
		"hvs." + strings.Repeat("Qx7", 8),
		strings.Repeat("deadbeef01", 4),
		strings.Repeat("aB3+", 7),
		"eyJ" + strings.Repeat("hbGci0", 5),
	}
}

func TestRedact(t *testing.T) {
	for _, c := range canaries() {
		got := redact("deploy with " + c + " now")
		if strings.Contains(got, c) || !strings.Contains(got, redacted) {
			t.Errorf("redact left %q in %q", c, got)
		}
	}
	bearer := "Bearer " + "abc.def." + strings.Repeat("x", 6)
	if got := redact("curl -H 'Authorization: " + bearer + "'"); strings.Contains(got, "abc.def") {
		t.Errorf("bearer value kept: %q", got)
	}
	if got := redact("api_key=" + "hunter" + "22"); strings.Contains(got, "hunter") {
		t.Errorf("key=value kept: %q", got)
	}
	keep := "scan for em dash in scripts/check-no-dashes.sh"
	if got := redact(keep); got != keep {
		t.Errorf("redact changed a plain query: %q", got)
	}
}

func TestHarness(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cases := []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"CLAUDECODE": "1"}, "claude"},
		{map[string]string{"CODEX_SANDBOX": "seatbelt"}, "codex"},
		{map[string]string{"GEMINI_CLI": "1"}, "gemini"},
		{map[string]string{"HERMES_HOME": "/x"}, "hermes"},
		{map[string]string{}, "unknown"},
	}
	for _, c := range cases {
		if got := harness(env(c.env)); got != c.want {
			t.Errorf("harness(%v) = %s, want %s", c.env, got, c.want)
		}
	}
	if got := session(env(map[string]string{"CLAUDE_SESSION_ID": "s-1"})); got != "s-1" {
		t.Errorf("session = %q", got)
	}
}

func TestFindHitAndFormat(t *testing.T) {
	getenv := testEnv(t, nil)
	code, out, errOut := run(t, getenv, "find", "scan", "for", "em", "dash", "--project", fixture("project"))
	if code != cli.ExitOK {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	first := strings.SplitN(out, "\n", 2)[0]
	if !strings.HasPrefix(first, "check-no-dashes  Use when a change may add em or en dashes.") || !strings.Contains(first, "[script global] ") {
		t.Errorf("first line %q", first)
	}
	if lines := strings.Count(out, "\n"); lines > DefaultLimit+2 {
		t.Errorf("%d lines", lines)
	}
	code, out, _ = run(t, getenv, "find", "--limit", "1", "--project", fixture("project"), "ssh", "to", "mkh-pc", "run", "command")
	if code != 0 || strings.Count(out, "\n") != 1 || !strings.HasPrefix(out, "host-run  ") {
		t.Errorf("limit 1: exit %d out %q", code, out)
	}
	_, out, _ = run(t, getenv, "find", "--limit=50", "--project="+fixture("project"), "dash ssh pr checks build test docs log data release diagram notes style review")
	if len(out) > 2000 {
		t.Errorf("broad query printed %d characters", len(out))
	}
	_, out, _ = run(t, getenv, "find", "--project", fixture("project"), "dash ssh pr checks build test docs log data release diagram")
	if len(out) > 1100 {
		t.Errorf("default output %d characters, want about 1,000:\n%s", len(out), out)
	}
	if _, err := os.Stat(filepath.Join(getenv("AGENT_DRY_STATE"), MissFile)); err == nil {
		t.Error("a hit wrote the miss log")
	}
}

func TestFindJSON(t *testing.T) {
	getenv := testEnv(t, nil)
	for _, q := range []string{"watch pr checks", "bake sourdough bread loaf"} {
		code, out, _ := run(t, getenv, "find", "--json", "--project", fixture("project"), q)
		var v map[string]json.RawMessage
		if err := json.Unmarshal([]byte(out), &v); err != nil {
			t.Fatalf("%q: %v: %s", q, err, out)
		}
		if len(v) != 2 || v["results"] == nil || v["hints"] == nil {
			t.Fatalf("%q: keys %s", q, out)
		}
		var results []Result
		var hints []Hint
		if json.Unmarshal(v["results"], &results) != nil || json.Unmarshal(v["hints"], &hints) != nil {
			t.Fatalf("%q: shape %s", q, out)
		}
		if string(v["results"])[0] != '[' || string(v["hints"])[0] != '[' {
			t.Errorf("%q: results or hints not arrays: %s", q, out)
		}
		if q == "watch pr checks" && (code != 0 || len(results) == 0 || results[0].Name != "pr-flow" || results[0].Source != srcAgentDry || results[0].Score < Threshold) {
			t.Errorf("hit: exit %d %s", code, out)
		}
		if q != "watch pr checks" && (code != 1 || len(results) != 0) {
			t.Errorf("miss: exit %d %s", code, out)
		}
	}
}

func TestMissLogAndSeen(t *testing.T) {
	getenv := testEnv(t, map[string]string{"CLAUDECODE": "1", "CLAUDE_SESSION_ID": "sess-42"})
	state := getenv("AGENT_DRY_STATE")
	code, out, _ := run(t, getenv, "find", "--project", fixture("project"), "bake", "sourdough", "bread", "loaf")
	if code != cli.ExitNegative || out != "" {
		t.Fatalf("miss: exit %d out %q", code, out)
	}
	raw, err := os.ReadFile(filepath.Join(state, MissFile))
	if err != nil {
		t.Fatal(err)
	}
	var m miss
	if err := json.Unmarshal(bytes.TrimSpace(raw), &m); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	if m.Query != "bake sourdough bread loaf" || strings.Join(m.Tokens, ",") != "bake,bread,loaf,sourdough" ||
		m.Project != "project" || m.Harness != "claude" || m.Session != "sess-42" {
		t.Errorf("record %+v", m)
	}
	if _, err := time.Parse(time.RFC3339, m.TS); err != nil {
		t.Errorf("ts %q: %v", m.TS, err)
	}
	if info, _ := os.Stat(filepath.Join(state, MissFile)); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("miss log mode %v", info.Mode().Perm())
	}

	// Jaccard({sourdough bread loaf recipe}, {bake bread loaf sourdough}) = 3/5.
	code, out, _ = run(t, getenv, "find", "--project", fixture("project"), "sourdough bread loaf recipe")
	want := "SEEN " + m.TS[:10] + " project: earlier miss \"bake sourdough bread loaf\"; build before repeating\n"
	if code != cli.ExitNegative || out != want {
		t.Errorf("seen: exit %d out %q, want %q", code, out, want)
	}
	// Below 0.5: {sourdough starter} vs either record.
	if _, out, _ = run(t, getenv, "find", "--project", fixture("project"), "sourdough starter feeding schedule"); strings.Contains(out, "SEEN") {
		t.Errorf("dissimilar query printed %q", out)
	}
	raw, _ = os.ReadFile(filepath.Join(state, MissFile))
	if n := bytes.Count(raw, []byte("\n")); n != 3 {
		t.Errorf("%d miss records, want 3", n)
	}
	// A hit after earlier misses prints no SEEN.
	if _, out, _ = run(t, getenv, "find", "--project", fixture("project"), "watch pr checks"); strings.Contains(out, "SEEN") {
		t.Errorf("hit printed SEEN: %q", out)
	}
}

func TestWantedHint(t *testing.T) {
	getenv := testEnv(t, nil)
	code, out, _ := run(t, getenv, "find", "--project", fixture("project"), "source nvm before node")
	if code != cli.ExitNegative {
		t.Errorf("hints alone must exit 1, got %d", code)
	}
	if out != "WANTED node-environment-shim: build node-env before use\n" {
		t.Errorf("out %q", out)
	}
	code, out, _ = run(t, getenv, "find", "--json", "--project", fixture("project"), "extract gh run ids with jq")
	if code != 1 || !strings.Contains(out, `"kind":"wanted"`) || !strings.Contains(out, `"suggested_name":"jq-filters"`) {
		t.Errorf("json wanted: exit %d %s", code, out)
	}
}

func TestRedactionCanary(t *testing.T) {
	getenv := testEnv(t, nil)
	state := getenv("AGENT_DRY_STATE")
	cs := canaries()
	args := append([]string{"find", "--project", fixture("project"), "bake", "bread", "with"}, cs...)
	args = append(args, "Bearer", "bearer.value."+strings.Repeat("q", 5))
	var outputs []string
	for i := 0; i < 2; i++ { // the second run prints SEEN with the stored query
		code, out, errOut := run(t, getenv, args...)
		if code != cli.ExitNegative {
			t.Fatalf("exit %d", code)
		}
		outputs = append(outputs, out, errOut)
	}
	raw, err := os.ReadFile(filepath.Join(state, MissFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(outputs[2], "SEEN") || !strings.Contains(outputs[2], redacted) {
		t.Errorf("second run did not print a redacted SEEN line: %q", outputs[2])
	}
	all := string(raw) + strings.Join(outputs, "")
	for _, c := range append(cs, "bearer.value") {
		if strings.Contains(all, c) {
			t.Errorf("canary %q leaked", c[:6])
		}
	}
	if !strings.Contains(string(raw), redacted) {
		t.Error("log does not show <redacted>")
	}
}

func TestFindUsageAndEnvErrors(t *testing.T) {
	getenv := testEnv(t, nil)
	usage := [][]string{
		{"find"},
		{"find", "--json"},
		{"find", "--bogus", "x"},
		{"find", "--limit", "0", "x"},
		{"find", "--limit", "abc", "x"},
		{"find", "x", "--limit"},
		{"find", "--project", filepath.Join(t.TempDir(), "missing"), "x"},
		{"show"},
		{"show", "a", "b"},
		{"index", "--bogus"},
		{"guide", "bogus"},
	}
	for _, a := range usage {
		if code, _, _ := run(t, getenv, a...); code != cli.ExitUsage {
			t.Errorf("%v: exit %d, want 2", a, code)
		}
	}
	for _, c := range []string{"find", "show", "index", "guide"} {
		code, out, _ := run(t, getenv, c, "--help")
		if code != 0 || !strings.HasPrefix(out, "Usage: rice-agent "+c) {
			t.Errorf("%s --help: exit %d %q", c, code, out)
		}
	}

	noRoot := testEnv(t, map[string]string{"AGENT_DRY_ROOT": t.TempDir()})
	for _, a := range [][]string{{"find", "x"}, {"show", "x"}, {"index"}} {
		if code, _, errOut := run(t, noRoot, a...); code != cli.ExitEnv || !strings.Contains(errOut, "not an rice-agent root") {
			t.Errorf("%v without root: exit %d %q", a, code, errOut)
		}
	}

	if os.Geteuid() != 0 {
		state := getenv("AGENT_DRY_STATE")
		if code, _, _ := run(t, getenv, "index"); code != 0 {
			t.Fatalf("index exit %d", code)
		}
		p := filepath.Join(state, IndexFile)
		if err := os.Chmod(p, 0); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(p, 0o600)
		if code, _, errOut := run(t, getenv, "find", "--project", fixture("project"), "watch pr checks"); code != cli.ExitEnv || !strings.Contains(errOut, "unreadable index") {
			t.Errorf("unreadable index: exit %d %q", code, errOut)
		}
	}
}

func TestShow(t *testing.T) {
	getenv := testEnv(t, nil)
	p := fixture("project")
	cases := []struct {
		name string
		want []string
	}{
		{"check-no-dashes", []string{"rice-agent tool, status trial", "side effects: read", "dry run:      none",
			"exit codes:   0 no dash found; 1 a dash was found; 2 usage error", "test:         bash test.sh", "Usage: check-no-dashes"}},
		{"host-run", []string{"dry run:      --dry-run", "requires:     ssh", "config:       hosts", "3 host unreachable"}},
		{"old-scan", []string{"status retired", "superseded by: check-no-dashes"}},
		{"build", []string{"[recipe project] project:just", "run:          just build", "Build the fixture binary"}},
		{"lint:css", []string{"run:          npm run lint:css", "command:      stylelint"}},
		{"clean", []string{"run:          make clean"}},
		{"rotate-logs", []string{"run:          scripts/rotate-logs.sh", "Rotate and compress"}},
		{"seed-demo-data", []string{"run:          just seed"}},
		{"tdd-loop", []string{"[skill global]", "read:", "SKILL.md"}},
		{"release-judgment", []string{"[skill global] agent-dry"}},
		{"node-env", []string{"node-environment-shim  [wanted global] no tool yet", "build:        node-env (script)"}},
	}
	for _, c := range cases {
		code, out, errOut := run(t, getenv, "show", c.name, "--project", p)
		if code != 0 {
			t.Errorf("show %s: exit %d %s", c.name, code, errOut)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(out, w) {
				t.Errorf("show %s: missing %q in:\n%s", c.name, w, out)
			}
		}
	}
	if code, _, errOut := run(t, getenv, "show", "no-such-thing", "--project", p); code != cli.ExitNegative || !strings.Contains(errOut, "no entry named") {
		t.Errorf("unknown: exit %d %q", code, errOut)
	}
}

func TestToolHelpBounded(t *testing.T) {
	dir := t.TempDir()
	long := filepath.Join(dir, "long")
	os.WriteFile(long, []byte("#!/bin/sh\ni=0; while [ $i -lt 100 ]; do echo line $i; i=$((i+1)); done\n"), 0o755)
	out := toolHelp(long, dir, 40)
	if n := strings.Count(out, "\n"); n != 41 || !strings.Contains(out, "60 more lines") {
		t.Errorf("%d lines: %q", n, out)
	}
	slow := filepath.Join(dir, "slow")
	os.WriteFile(slow, []byte("#!/bin/sh\nexec sleep 5\n"), 0o755)
	old := helpTimeout
	helpTimeout = 200 * time.Millisecond
	defer func() { helpTimeout = old }()
	start := time.Now()
	if out := toolHelp(slow, dir, 40); !strings.Contains(out, "timed out") {
		t.Errorf("slow: %q", out)
	}
	if el := time.Since(start); el > 4*time.Second {
		t.Errorf("timeout took %s", el)
	}
}

// copyTree copies a fixture tree so a test can change modification times.
func copyTree(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func TestIndexCache(t *testing.T) {
	root := copyTree(t, fixture("root"))
	getenv := testEnv(t, map[string]string{"AGENT_DRY_ROOT": root})
	code, out, errOut := run(t, getenv, "index")
	if code != 0 || !strings.HasPrefix(out, "index rebuilt: 3 tools, 1 skills, 3 installed skills, 3 external providers, 2 wanted families at ") {
		t.Fatalf("first index: exit %d %q %q", code, out, errOut)
	}
	if code, out, _ = run(t, getenv, "index"); code != 0 || !strings.HasPrefix(out, "index fresh:") {
		t.Errorf("second index: %q", out)
	}
	if code, out, _ = run(t, getenv, "index", "--rebuild"); code != 0 || !strings.HasPrefix(out, "index rebuilt:") {
		t.Errorf("--rebuild: %q", out)
	}
	ix, err := Load(getenv, Options{NoProject: true})
	if err != nil || !ix.fromCache {
		t.Fatalf("find did not use the fresh cache: %v", err)
	}
	if res := ix.Search("ssh to mkh-pc", 1); len(res) != 1 || res[0].Name != "host-run" {
		t.Errorf("cached search %v", res)
	}
	// Edit a manifest: the stamp changes and the next load rebuilds.
	tj := filepath.Join(root, "tools", "host-run", "tool.json")
	raw, _ := os.ReadFile(tj)
	os.WriteFile(tj, bytes.Replace(raw, []byte(`"ssh to mkh-pc"`), []byte(`"open a shell on the build box"`), 1), 0o644)
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(tj, future, future)
	ix, err = Load(getenv, Options{NoProject: true})
	if err != nil || ix.fromCache {
		t.Fatalf("edited manifest still served from cache: %v", err)
	}
	if res := ix.Search("open a shell on the build box", 1); len(res) != 1 || res[0].Name != "host-run" {
		t.Errorf("rebuilt search %v", res)
	}
	// A new tool directory invalidates through the tools directory stamp.
	os.MkdirAll(filepath.Join(root, "tools", "zz-new"), 0o755)
	if ix, _ = Load(getenv, Options{NoProject: true}); ix.fromCache {
		t.Error("new tool directory did not invalidate")
	}
	// A broken manifest is skipped with a warning, not a failure.
	os.WriteFile(filepath.Join(root, "tools", "zz-new", "tool.json"), []byte(`{"schema":"tool/v1"}`), 0o644)
	code, _, errOut = run(t, getenv, "find", "ssh to mkh-pc run command")
	if code != 0 || !strings.Contains(errOut, "skipped manifest") {
		t.Errorf("broken manifest: exit %d %q", code, errOut)
	}
	// A corrupt cache is rebuilt silently.
	os.WriteFile(filepath.Join(getenv("AGENT_DRY_STATE"), IndexFile), []byte("{not json"), 0o600)
	if code, _, _ = run(t, getenv, "find", "watch pr checks"); code != 0 {
		t.Errorf("corrupt cache: exit %d", code)
	}
	// A broken wanted.json (the root marker) is an unreadable root.
	os.WriteFile(filepath.Join(root, "registry", "wanted.json"), []byte(`{"schema":"wanted/v0","families":[]}`), 0o644)
	if code, _, errOut = run(t, getenv, "find", "watch pr checks"); code != cli.ExitEnv || !strings.Contains(errOut, "wanted/v1") {
		t.Errorf("bad wanted.json: exit %d %q", code, errOut)
	}
}

func TestGitRoot(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, ".git"), 0o755)
	sub := filepath.Join(dir, "a", "b")
	os.MkdirAll(sub, 0o755)
	real, _ := filepath.EvalSymlinks(dir)
	if got, _ := filepath.EvalSymlinks(gitRoot(sub)); got != real {
		t.Errorf("gitRoot = %q, want %q", got, real)
	}
	if got := projectRoot(sub); got != sub {
		t.Errorf("explicit project root %q", got)
	}
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(sub)
	if got, _ := filepath.EvalSymlinks(projectRoot("")); got != real {
		t.Errorf("working directory project root %q, want %q", got, real)
	}
}

func TestGuide(t *testing.T) {
	getenv := testEnv(t, nil)
	if code, out, _ := run(t, getenv, "guide"); code != 0 || out != docs.Guide {
		t.Errorf("guide: exit %d", code)
	}
	if code, out, _ := run(t, getenv, "guide", "tools"); code != 0 || out != docs.ToolContract {
		t.Errorf("guide tools: exit %d", code)
	}
	// Every embedded topic prints, and --list names them.
	topics := docs.Topics()
	if code, out, _ := run(t, getenv, "guide", "--list"); code != 0 || out != strings.Join(topics, "\n")+"\n" {
		t.Errorf("guide --list: exit %d %q, want %v", code, out, topics)
	}
	for _, name := range topics {
		want, _ := docs.Topic(name)
		if code, out, _ := run(t, getenv, "guide", name); code != 0 || out == "" || out != want {
			t.Errorf("guide %s: exit %d, %d bytes", name, code, len(out))
		}
	}
}

// TestGuideTopics drives guide through a fake docs source.
func TestGuideTopics(t *testing.T) {
	saved := guideDocs
	t.Cleanup(func() { guideDocs = saved })
	fake := map[string]string{"alpha": "Alpha topic.\n", "tools": "Tool contract.\n"}
	guideDocs.guide = "Main guide.\n"
	guideDocs.topic = func(name string) (string, bool) { s, ok := fake[name]; return s, ok }
	guideDocs.topics = func() []string { return []string{"alpha", "tools"} }

	getenv := testEnv(t, nil)
	cases := []struct {
		args []string
		code int
		out  string
		err  string
	}{
		{nil, 0, "Main guide.\n", ""},
		{[]string{"alpha"}, 0, "Alpha topic.\n", ""},
		{[]string{"tools"}, 0, "Tool contract.\n", ""},
		{[]string{"--list"}, 0, "alpha\ntools\n", ""},
		{[]string{"beta"}, cli.ExitUsage, "", `unknown topic "beta"; topics: alpha, tools`},
		{[]string{"alpha", "tools"}, cli.ExitUsage, "", "at most one topic"},
		{[]string{"--bogus"}, cli.ExitUsage, "", "unknown flag --bogus"},
	}
	for _, c := range cases {
		code, out, errOut := run(t, getenv, append([]string{"guide"}, c.args...)...)
		if code != c.code || out != c.out || !strings.Contains(errOut, c.err) {
			t.Errorf("guide %v: exit %d out %q err %q, want %d %q %q", c.args, code, out, errOut, c.code, c.out, c.err)
		}
	}
}

// TestRepositoryWanted checks the seeded registry/wanted.json.
func TestRepositoryWanted(t *testing.T) {
	var st []stamp
	ds, err := readWanted(filepath.Join("..", "..", "registry", "wanted.json"), &st)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 4 {
		t.Fatalf("%d families, want 4", len(ds))
	}
	for _, d := range ds {
		f := d.Wanted
		if len(f.Intents) < 3 || f.Evidence == "" || f.SuggestedName == "" || f.SuggestedKind == "" ||
			f.Research != "rice-kb:docs/research/agent-dry-local-evidence-20261017.md" {
			t.Errorf("incomplete family %+v", f)
		}
	}
}
