package registry

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moabualruz/agent-dry/internal/cli"
)

// providerEnv uses the fixture PATH, where dupe-finder and srn exist and token-meter does not.
func providerEnv(t *testing.T, over map[string]string) func(string) string {
	t.Helper()
	m := map[string]string{"PATH": fixture("bin")}
	for k, v := range over {
		m[k] = v
	}
	return testEnv(t, m)
}

func TestProviderSchemaFixtures(t *testing.T) {
	for _, set := range []string{"valid", "invalid"} {
		files, err := filepath.Glob(fixture("providers", set, "*.json"))
		if err != nil || len(files) == 0 {
			t.Fatalf("%s fixtures: %v", set, err)
		}
		for _, f := range files {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			err = ValidateProviders(raw)
			if set == "valid" && err != nil {
				t.Errorf("%s: %v", filepath.Base(f), err)
			}
			if set == "invalid" && err == nil {
				t.Errorf("%s: accepted", filepath.Base(f))
			}
		}
	}
}

// TestRepositoryProviders checks the seeded registry/providers.json.
func TestRepositoryProviders(t *testing.T) {
	path := filepath.Join("..", "..", ProvidersFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateProviders(raw); err != nil {
		t.Fatal(err)
	}
	var st []stamp
	ds, warns := readProviders(path, &st)
	if len(warns) != 0 {
		t.Errorf("warnings: %v", warns)
	}
	byName := map[string]*Provider{}
	for _, d := range ds {
		byName[d.Name] = d.Provider
		if d.Kind != kindExternal || d.Source != srcExternal || d.Entry != d.Provider.Command {
			t.Errorf("%s: kind %s source %s entry %s", d.Name, d.Kind, d.Source, d.Entry)
		}
	}
	for _, n := range []string{"rice-edit", "gopls", "rust-analyzer", "ast-grep", "jscpd", "ccusage", "osv-scanner", "syft", "trivy", "renovate", "shellcheck"} {
		if byName[n] == nil {
			t.Errorf("missing provider %s", n)
		}
	}
	if p := byName["rice-edit"]; p != nil && p.Owner != "project:moabualruz/rice-edit" {
		t.Errorf("rice-edit owner %q", p.Owner)
	}
	if p := byName["renovate"]; p != nil && p.License != "AGPL-3.0-only" {
		t.Errorf("renovate license %q", p.License)
	}
}

// TestRepositoryProviderRanking runs the example queries against the real registry with an empty
// PATH and the fixture HOME, so the host's installed skills do not change the ranking.
func TestRepositoryProviderRanking(t *testing.T) {
	root := mustAbs(filepath.Join("..", ".."))
	ix, err := Load(testEnv(t, map[string]string{"AGENT_DRY_ROOT": root, "PATH": ""}), Options{NoProject: true})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"rename symbol":                      {"rice-edit", "gopls"},
		"duplicate code across repositories": {"jscpd"},
		"token usage cost":                   {"ccusage"},
	}
	for q, names := range want {
		got := map[string]Result{}
		for _, r := range ix.Search(q, DefaultLimit) {
			got[r.Name] = r
		}
		for _, n := range names {
			r, ok := got[n]
			if !ok {
				t.Errorf("%q: %s not in the first %d results (%v)", q, n, DefaultLimit, ix.Search(q, DefaultLimit))
				continue
			}
			if r.Kind != kindExternal || r.Installed == nil || *r.Installed || r.Install == "" {
				t.Errorf("%q: %s result %+v", q, n, r)
			}
		}
	}
}

func TestFindExternal(t *testing.T) {
	getenv := providerEnv(t, nil)
	code, out, errOut := run(t, getenv, "find", "--project", fixture("project"), "find duplicate code across repositories")
	if code != cli.ExitOK {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	first := strings.SplitN(out, "\n", 2)[0]
	if want := "dupe-finder  Use when finding copy-pasted code blocks across several repositories.  [external global] dupe-finder (installed)"; first != want {
		t.Errorf("installed line\n got %q\nwant %q", first, want)
	}
	code, out, _ = run(t, getenv, "find", "--project", fixture("project"), "token", "usage", "cost")
	first = strings.SplitN(out, "\n", 2)[0]
	if want := "token-meter  Use when reporting agent token usage and cost from local session logs.  [external global] token-meter (missing: npm install -g token-meter)"; code != 0 || first != want {
		t.Errorf("missing line: exit %d\n got %q\nwant %q", code, first, want)
	}
	// The command differs from the name: the entry is the command.
	if _, out, _ = run(t, getenv, "find", "--project", fixture("project"), "rename symbol"); !strings.HasPrefix(out, "sym-rename  ") || !strings.Contains(out, "[external global] srn (installed)") {
		t.Errorf("sym-rename: %q", out)
	}

	// JSON carries kind, source, installed and the install hint.
	_, out, _ = run(t, getenv, "find", "--json", "--project", fixture("project"), "token usage cost")
	var v struct{ Results []Result }
	if err := json.Unmarshal([]byte(out), &v); err != nil || len(v.Results) == 0 {
		t.Fatalf("json: %v %s", err, out)
	}
	r := v.Results[0]
	if r.Name != "token-meter" || r.Kind != kindExternal || r.Source != srcExternal || r.Installed == nil || *r.Installed || r.Install != "npm install -g token-meter" {
		t.Errorf("json result %+v", r)
	}
	if !strings.Contains(out, `"installed":false`) {
		t.Errorf("json lacks installed:false: %s", out)
	}
	_, out, _ = run(t, getenv, "find", "--json", "--project", fixture("project"), "watch pr checks")
	if strings.Contains(out, `"installed"`) || strings.Contains(out, `"install"`) {
		t.Errorf("owned tool carries provider fields: %s", out)
	}

	// The PATH lookup happens at query time, even when the index comes from the cache.
	noPath := func(k string) string {
		if k == "PATH" {
			return ""
		}
		return getenv(k)
	}
	ix, err := Load(noPath, Options{NoProject: true})
	if err != nil || !ix.fromCache {
		t.Fatalf("second load not cached: %v", err)
	}
	if res := ix.Search("find duplicate code across repositories", 1); len(res) != 1 || res[0].Name != "dupe-finder" || *res[0].Installed {
		t.Errorf("empty PATH still installed: %+v", res)
	}
}

func TestShowExternal(t *testing.T) {
	getenv := providerEnv(t, nil)
	p := fixture("project")
	code, out, errOut := run(t, getenv, "show", "dupe-finder", "--project", p)
	if code != 0 {
		t.Fatalf("exit %d %s", code, errOut)
	}
	for _, w := range []string{
		"dupe-finder  [external global] external CLI, no agent-dry wrapper",
		"command:      dupe-finder (installed: " + fixture("bin", "dupe-finder") + ")",
		"owner:        upstream:https://example.com/dupe-finder",
		"license:      MIT\n",
		"install:      brew install dupe-finder",
		"homepage:     https://example.com/dupe-finder",
		"notes:        Fixture entry",
		"intents:      find duplicate code across repositories; copy paste detection; clone report as sarif",
		"$ dupe-finder --help\n",
		"dupe-finder help line 30\n",
		"... (15 more lines)\n",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("show dupe-finder: missing %q in:\n%s", w, out)
		}
	}
	if strings.Contains(out, "help line 31") || strings.Contains(out, "policy check") {
		t.Errorf("show dupe-finder: past 30 lines or wrong policy line:\n%s", out)
	}

	_, out, _ = run(t, getenv, "show", "token-meter", "--project", p)
	for _, w := range []string{"command:      token-meter (missing: npm install -g token-meter)", "owner:        project:example/token-meter",
		"policy check: required: Python inside, so run only its supported CLI"} {
		if !strings.Contains(out, w) {
			t.Errorf("show token-meter: missing %q in:\n%s", w, out)
		}
	}
	if strings.Contains(out, "--help") {
		t.Errorf("show token-meter ran --help for a missing command:\n%s", out)
	}

	// An external entry also answers to its command name.
	_, out, _ = run(t, getenv, "show", "srn", "--project", p)
	if !strings.HasPrefix(out, "sym-rename  [external global]") || !strings.Contains(out, "Usage: srn <file> <offset> <new-name>") {
		t.Errorf("show srn:\n%s", out)
	}
}

// TestAdmissionIgnoresProviders: the gates load with NoInstalled, so G2 compares owned entries
// only, and providers do not shift the scores of owned tools.
func TestAdmissionIgnoresProviders(t *testing.T) {
	gate := Options{NoProject: true, NoInstalled: true}
	ix, err := Load(providerEnv(t, nil), gate)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"find duplicate code across repositories", "token usage cost", "rename symbol"} {
		for _, r := range ix.Search(q, 0) {
			if r.Kind == kindExternal || r.Source == srcExternal {
				t.Errorf("%q: admission search returned external %s", q, r.Name)
			}
		}
	}
	if ix.counts[srcExternal] != 0 {
		t.Errorf("admission index holds %d providers", ix.counts[srcExternal])
	}
	full, err := Load(providerEnv(t, nil), Options{NoProject: true})
	if err != nil || full.counts[srcExternal] != 3 {
		t.Fatalf("find index holds %d providers: %v", full.counts[srcExternal], err)
	}

	root := copyTree(t, fixture("root"))
	if err := os.Remove(filepath.Join(root, ProvidersFile)); err != nil {
		t.Fatal(err)
	}
	bare, err := Load(providerEnv(t, map[string]string{"AGENT_DRY_ROOT": root}), gate)
	if err != nil {
		t.Fatal(err)
	}
	q := "ssh to mkh-pc run command"
	a, b := ix.Search(q, 1), bare.Search(q, 1)
	if len(a) != 1 || len(b) != 1 || a[0].Score != b[0].Score {
		t.Errorf("providers changed the admission score: %v vs %v", a, b)
	}
}

func TestProviderCache(t *testing.T) {
	root := copyTree(t, fixture("root"))
	getenv := providerEnv(t, map[string]string{"AGENT_DRY_ROOT": root})
	pj := filepath.Join(root, ProvidersFile)
	touch := func() {
		future := time.Now().Add(2 * time.Second)
		os.Chtimes(pj, future, future)
	}
	if code, out, errOut := run(t, getenv, "index"); code != 0 || !strings.Contains(out, "3 external providers") {
		t.Fatalf("index: exit %d %q %q", code, out, errOut)
	}
	if ix, err := Load(getenv, Options{NoProject: true}); err != nil || !ix.fromCache {
		t.Fatalf("not served from cache: %v", err)
	}

	// An edited providers.json rebuilds.
	raw, _ := os.ReadFile(pj)
	os.WriteFile(pj, bytes.Replace(raw, []byte(`"offline usage json"`), []byte(`"weekly spend digest"`), 1), 0o644)
	touch()
	ix, err := Load(getenv, Options{NoProject: true})
	if err != nil || ix.fromCache {
		t.Fatalf("edited providers.json served from cache: %v", err)
	}
	if res := ix.Search("weekly spend digest", 1); len(res) != 1 || res[0].Name != "token-meter" {
		t.Errorf("rebuilt search %v", res)
	}

	// An invalid entry is skipped with a warning; the other entries still load.
	var pf map[string]any
	json.Unmarshal(raw, &pf)
	pf["providers"] = append(pf["providers"].([]any), map[string]any{"name": "bad-one", "summary": "no prefix here"})
	edited, _ := json.Marshal(pf)
	os.WriteFile(pj, edited, 0o644)
	touch()
	code, out, errOut := run(t, getenv, "find", "token usage cost")
	if code != 0 || !strings.HasPrefix(out, "token-meter  ") || !strings.Contains(errOut, "skipped provider bad-one") {
		t.Errorf("invalid entry: exit %d out %q err %q", code, out, errOut)
	}

	// Broken JSON skips every provider with a warning; owned tools still answer.
	os.WriteFile(pj, []byte("{not json"), 0o644)
	touch()
	code, out, errOut = run(t, getenv, "find", "watch pr checks")
	if code != 0 || !strings.HasPrefix(out, "pr-flow  ") || !strings.Contains(errOut, "skipped providers") {
		t.Errorf("broken providers.json: exit %d out %q err %q", code, out, errOut)
	}

	// Removing the file invalidates; creating it again invalidates through the missing stamp.
	os.Remove(pj)
	if ix, _ = Load(getenv, Options{NoProject: true}); ix.fromCache || ix.counts[srcExternal] != 0 {
		t.Errorf("removed providers.json: cached %v, %d providers", ix.fromCache, ix.counts[srcExternal])
	}
	if ix, _ = Load(getenv, Options{NoProject: true}); !ix.fromCache {
		t.Error("cache not reused without providers.json")
	}
	os.WriteFile(pj, raw, 0o644)
	if ix, _ = Load(getenv, Options{NoProject: true}); ix.fromCache || ix.counts[srcExternal] != 3 {
		t.Errorf("recreated providers.json: cached %v, %d providers", ix.fromCache, ix.counts[srcExternal])
	}
}
