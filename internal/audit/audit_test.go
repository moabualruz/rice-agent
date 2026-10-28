package audit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moabualruz/rice-agent/internal/cli"
	"github.com/moabualruz/rice-agent/internal/manifest"
)

var fixedNow = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

const (
	sessA = "11111111-aaaa-4aaa-8aaa-000000000001"
	sessB = "22222222-bbbb-4bbb-8bbb-000000000002"
	sessC = "33333333-cccc-4ccc-8ccc-000000000003"
	sessD = "44444444-dddd-4ddd-8ddd-000000000004"
)

func testdata(t *testing.T, parts ...string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join(append([]string{"..", "..", "testdata", "audit"}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// fixture copies the transcript fixtures into a temporary root and stamps their mtimes: every file
// inside the window except session D, which is older than any window the tests use.
type fixture struct {
	projects string
	home     string
	root     string
	config   string
	vars     map[string]string
}

func setup(t *testing.T, mtimes map[string]time.Time) *fixture {
	t.Helper()
	old := now
	oldHermesLookPath := hermesLookPath
	now = func() time.Time { return fixedNow }
	hermesLookPath = func(string) (string, error) { return "", os.ErrNotExist }
	t.Cleanup(func() {
		now = old
		hermesLookPath = oldHermesLookPath
	})

	tmp := t.TempDir()
	f := &fixture{projects: filepath.Join(tmp, "projects"), home: filepath.Join(tmp, "home")}
	if err := os.MkdirAll(f.home, 0o755); err != nil {
		t.Fatal(err)
	}
	src := testdata(t, "projects")
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		dst := filepath.Join(f.projects, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dst, raw, 0o644); err != nil {
			return err
		}
		mt := fixedNow.Add(-24 * time.Hour)
		if strings.HasPrefix(d.Name(), sessD) {
			mt = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
		}
		for prefix, m := range mtimes {
			if strings.HasPrefix(d.Name(), prefix) {
				mt = m
			}
		}
		return os.Chtimes(dst, mt, mt)
	})
	if err != nil {
		t.Fatal(err)
	}
	srcRoot := testdata(t, "root")
	f.root = filepath.Join(tmp, "root")
	err = filepath.WalkDir(srcRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(srcRoot, p)
		dst := filepath.Join(f.root, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, raw, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "tools", "check-no-dashes", "test.sh"), []byte("exit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.config = filepath.Join(tmp, "config.json")
	f.writeConfig(t, map[string]any{
		"hosts": map[string]any{
			"alpha-host": map[string]string{"ssh": "mkh@192.0.2.10", "shell": "fish"},
			"beta-host":  map[string]string{"ssh": "ops@198.51.100.7"},
		},
		"repos": []string{testdata(t, "repos", "repo-a"), testdata(t, "repos", "repo-b"), testdata(t, "repos", "repo-c"), testdata(t, "repos", "repo-d")},
	})
	f.vars = map[string]string{
		"HOME":             f.home,
		"AGENT_DRY_ROOT":   f.root,
		"AGENT_DRY_CONFIG": f.config,
		"AGENT_DRY_STATE":  testdata(t, "state"),
	}
	return f
}

func (f *fixture) writeConfig(t *testing.T, cfg map[string]any) {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.config, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	env := &cli.Env{Args: args, Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb,
		Getenv: func(k string) string { return f.vars[k] }}
	code := run(env)
	return code, out.String(), errb.String()
}

func (f *fixture) report(t *testing.T, args ...string) report {
	t.Helper()
	code, out, errs := f.run(t, append([]string{"--transcripts", f.projects, "--json"}, args...)...)
	if code != cli.ExitOK {
		t.Fatalf("exit %d, stderr: %s", code, errs)
	}
	var r report
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("decode report: %v\n%s", err, out)
	}
	return r
}

func eq[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func eqf(t *testing.T, what string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Errorf("%s = nil, want %v", what, want)
		return
	}
	if d := *got - want; d > 1e-9 || d < -1e-9 {
		t.Errorf("%s = %v, want %v", what, *got, want)
	}
}

func eqs(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func TestMetrics(t *testing.T) {
	f := setup(t, nil)
	r := f.report(t)

	tr := r.Transcripts
	eq(t, "files", tr.Files, 4)
	eq(t, "main", tr.Main, 3)
	eq(t, "subagent", tr.Subagent, 1)
	eq(t, "sessions", tr.Sessions, 4)
	eq(t, "tool uses", tr.ToolUses, 34)
	eq(t, "bash commands", tr.BashCommands, 31)
	eq(t, "duplicate ids", tr.DuplicateIDs, 2)
	eq(t, "window from", r.Window.From, "2026-10-03")

	// 1: A and the subagent look up first; B writes before its lookup; C never mutates.
	eq(t, "mutating sessions", r.LookupFirst.MutatingSessions, 3)
	eq(t, "lookup first", r.LookupFirst.LookupFirst, 2)
	eq(t, "find commands", r.LookupFirst.FindCommands, 3)
	eqf(t, "lookup rate", r.LookupFirst.Rate, 2.0/3.0)

	// 2: only tools with replaces patterns report inline counts.
	if len(r.Inline) != 1 {
		t.Fatalf("inline rows = %d, want 1", len(r.Inline))
	}
	in := r.Inline[0]
	eq(t, "inline tool", in.Tool, "check-no-dashes")
	eq(t, "inline", in.Inline, 2)
	eq(t, "inline sessions", in.InlineSessions, 2)
	eq(t, "runs", in.Runs, 3)
	eq(t, "run sessions", in.RunSessions, 3)
	eqf(t, "inline share", in.InlineShare, 0.4)
	eq(t, "inline baseline", in.Baseline, "259 inline, 26 runs")

	// 3: lifecycle commands in sessions that merged, per merge; C's view and its out-of-window
	// merge, the blob replay and session D never count.
	eq(t, "merge sessions", r.PR.MergeSessions, 2)
	eq(t, "lifecycle", r.PR.Lifecycle, 6)
	eq(t, "merges", r.PR.Merges, 3)
	eqf(t, "per merge", r.PR.PerMerge, 2.0)
	eq(t, "lifecycle all", r.PR.LifecycleAll, 7)

	// 4: raw ssh by address or host name; host-run is not raw ssh.
	eq(t, "hosts configured", r.SSH.HostsConfigured, 2)
	eq(t, "ssh commands", r.SSH.Commands, 6)
	eq(t, "ssh sessions", r.SSH.Sessions, 3)
	eqf(t, "ssh per session", r.SSH.PerSession, 2.0)
	eq(t, "host-run", r.SSH.HostRun, 1)
	if len(r.SSH.Hosts) != 2 {
		t.Fatalf("hosts = %v", r.SSH.Hosts)
	}
	eq(t, "alpha", r.SSH.Hosts[0], hostMetric{Host: "alpha-host", Commands: 5, Sessions: 2})
	eq(t, "beta", r.SSH.Hosts[1], hostMetric{Host: "beta-host", Commands: 1, Sessions: 1})

	// 5
	eq(t, "bao logins", r.OpenBao.Logins, 2)
	eq(t, "bao sessions", r.OpenBao.Sessions, 1)
	eq(t, "bao-run", r.OpenBao.BaoRun, 1)

	// 6: node_modules is skipped; the vendored copy matches the canonical entry.
	if r.Copies == nil {
		t.Fatal("no copy census")
	}
	eq(t, "copy source", r.Copies.Source, "config repos")
	var dash copyStat
	for _, c := range r.Copies.Tools {
		if c.Tool == "check-no-dashes" {
			dash = c
		} else if c.Copies != 0 {
			t.Errorf("%s copies = %d, want 0", c.Tool, c.Copies)
		}
	}
	eq(t, "copies", dash.Copies, 4)
	eq(t, "variants", dash.Variants, 3)
	eq(t, "identical", dash.Canonical, 1)
	eq(t, "copy baseline", dash.Baseline, "10 copies, 5 variants")

	// 7
	if r.Hygiene == nil {
		t.Fatal("no hygiene")
	}
	eqs(t, "trial older than 30d", r.Hygiene.TrialOlder30d, "legacy-sync")
	eqs(t, "unused", r.Hygiene.Unused, "legacy-sync")
	eqs(t, "promotion", r.Hygiene.PromotionCandidates, "check-no-dashes")
	eqs(t, "promoted", r.Hygiene.Promoted, "check-no-dashes")
	eqs(t, "failing tests", r.Hygiene.FailingTests, "legacy-sync", "stable-tool")
	eqs(t, "unused for 60 days", r.Hygiene.Unused60d, "legacy-sync")
	manifestBytes, err := os.ReadFile(filepath.Join(f.root, "tools", "check-no-dashes", "tool.json"))
	if err != nil {
		t.Fatal(err)
	}
	var promotedManifest struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(manifestBytes, &promotedManifest); err != nil {
		t.Fatal(err)
	}
	eq(t, "promoted manifest status", promotedManifest.Status, "active")

	// 8
	m := r.Misses
	eq(t, "miss log present", m.Present, true)
	eq(t, "misses total", m.Total, 6)
	eq(t, "misses malformed", m.Malformed, 1)
	eq(t, "misses in window", m.InWindow, 5)
	if len(m.Top) != 4 {
		t.Fatalf("top = %v", m.Top)
	}
	eqs(t, "top 1", m.Top[0].Tokens, "host", "run", "ssh")
	eq(t, "top 1 count", m.Top[0].Count, 2)
	eqs(t, "redacted set", m.Top[1].Tokens, "<redacted>", "deploy")
	eqs(t, "keyword string", m.Top[2].Tokens, "bao", "kv", "login")
}

// TestRedaction proves no command text, address, path or secret-like token reaches either output.
func TestUnusedSixtyDaysIsIndependentOfReportWindow(t *testing.T) {
	f := setup(t, map[string]time.Time{
		sessA:   fixedNow.Add(-45 * 24 * time.Hour),
		sessB:   fixedNow.Add(-45 * 24 * time.Hour),
		"agent": fixedNow.Add(-45 * 24 * time.Hour),
	})
	r := f.report(t, "--since", "14d")
	if !contains(r.Hygiene.Unused, "check-no-dashes") {
		t.Fatalf("check-no-dashes should be unused in the report window: %v", r.Hygiene.Unused)
	}
	if contains(r.Hygiene.Unused60d, "check-no-dashes") {
		t.Fatalf("check-no-dashes was used in the last 60 days: %v", r.Hygiene.Unused60d)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestFailingToolTestPreventsPromotion(t *testing.T) {
	f := setup(t, nil)
	if err := os.WriteFile(filepath.Join(f.root, "tools", "check-no-dashes", "test.sh"), []byte("exit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := f.report(t)
	eqs(t, "failing tests", r.Hygiene.FailingTests, "check-no-dashes", "legacy-sync", "stable-tool")
	if len(r.Hygiene.Promoted) != 0 {
		t.Fatalf("failing test promoted a tool: %v", r.Hygiene.Promoted)
	}
}

func TestRedaction(t *testing.T) {
	f := setup(t, nil)
	var commands []string
	err := filepath.WalkDir(f.projects, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".jsonl") {
			return err
		}
		file, err := os.Open(p)
		if err != nil {
			return err
		}
		defer file.Close()
		sc := bufio.NewScanner(file)
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		for sc.Scan() {
			var l struct {
				Message struct {
					Content json.RawMessage `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(sc.Bytes(), &l) != nil {
				continue
			}
			var blocks []struct {
				Input struct {
					Command string `json:"command"`
				} `json:"input"`
			}
			_ = json.Unmarshal(l.Message.Content, &blocks)
			for _, b := range blocks {
				if b.Input.Command != "" {
					commands = append(commands, b.Input.Command)
				}
			}
		}
		return sc.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) < 30 {
		t.Fatalf("read %d fixture commands, want at least 30", len(commands))
	}
	markers := []string{"SENTINEL", "REDACTME", "hvs.", "192.0.2.10", "198.51.100.7", "userpass",
		"BatchMode", "fixture-access", "uptime", "delete-branch", "/work/alpha", "client_token", "--tree"}
	for _, format := range []string{"text", "json"} {
		args := []string{"--transcripts", f.projects}
		if format == "json" {
			args = append(args, "--json")
		}
		code, out, errs := f.run(t, args...)
		if code != cli.ExitOK {
			t.Fatalf("%s: exit %d: %s", format, code, errs)
		}
		for _, s := range []string{out, errs} {
			for _, c := range commands {
				if strings.Contains(s, c) {
					t.Errorf("%s output contains command text %q", format, c)
				}
			}
			for _, mk := range markers {
				if strings.Contains(s, mk) {
					t.Errorf("%s output contains %q", format, mk)
				}
			}
		}
	}
}

func TestTextTable(t *testing.T) {
	f := setup(t, nil)
	code, out, errs := f.run(t, "--transcripts", f.projects)
	if code != cli.ExitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) >= 40 {
		t.Errorf("text report has %d lines, want under 40", len(lines))
	}
	for i, l := range lines {
		if strings.HasSuffix(l, " ") {
			t.Errorf("line %d has trailing spaces", i+1)
		}
	}
	for _, want := range []string{
		"1 lookup before first mutation", "2 of 3 sessions (66.7%)",
		"2 inline vs runs: check-no-dashes", "2 inline, 3 runs (40.0% inline)", "259 inline, 26 runs",
		"3 PR lifecycle per merge", "2.0 (6 commands, 3 merges, 2 sessions)", "18.6",
		"4 raw ssh per touching session", "2.0 (6 commands, 3 sessions)", "about 100", "alpha-host",
		"5 hand-built OpenBao logins", "2 (1 sessions)",
		"6 copies: check-no-dashes", "4 copies, 3 variants, 1 identical", "10 copies, 5 variants",
		"7 trial older than 30 days", "legacy-sync", "unused in 60 days", "failing tool tests",
		"promotion candidates", "check-no-dashes", "promoted", "8 find misses", "5 in window of 6", "host run ssh (2)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text report lacks %q\n%s", want, out)
		}
	}
}

func TestMaxFilesKeepsMainSessionsFirst(t *testing.T) {
	f := setup(t, map[string]time.Time{
		sessA:   fixedNow.Add(-2 * time.Hour),
		sessB:   fixedNow.Add(-3 * time.Hour),
		sessC:   fixedNow.Add(-4 * time.Hour),
		"agent": fixedNow.Add(-1 * time.Hour),
	})
	r := f.report(t, "--max-files", "1")
	eq(t, "files", r.Transcripts.Files, 1)
	eq(t, "main", r.Transcripts.Main, 1)
	eq(t, "not read", r.Transcripts.NotRead, 3)
	eq(t, "bao logins (session A only)", r.OpenBao.Logins, 2)
	eq(t, "merges (session A only)", r.PR.Merges, 1)
}

func TestNoToolsStillReportsTranscriptMetrics(t *testing.T) {
	f := setup(t, nil)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "registry"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "registry", "wanted.json"), []byte(`{"schema":"wanted/v1","families":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	f.vars["AGENT_DRY_ROOT"] = root
	r := f.report(t)
	eq(t, "tools", r.Tools, 0)
	eq(t, "lookup first", r.LookupFirst.LookupFirst, 2)
	eq(t, "merges", r.PR.Merges, 3)
	eq(t, "ssh", r.SSH.Commands, 6)
	eq(t, "bao", r.OpenBao.Logins, 2)
	if r.Copies != nil || r.Hygiene != nil || len(r.Inline) != 0 {
		t.Errorf("tool metrics reported without tools: %+v %+v %+v", r.Copies, r.Hygiene, r.Inline)
	}
	code, out, _ := f.run(t, "--transcripts", f.projects)
	eq(t, "exit", code, cli.ExitOK)
	for _, want := range []string{"1 lookup", "no tools registered", "3 PR lifecycle", "4 raw ssh", "5 hand-built OpenBao"} {
		if !strings.Contains(out, want) {
			t.Errorf("text lacks %q\n%s", want, out)
		}
	}
}

func TestCopyFallbackToWorkspaceScripts(t *testing.T) {
	f := setup(t, nil)
	f.writeConfig(t, map[string]any{})
	write := func(rel, body string) {
		p := filepath.Join(f.home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("workspace/one/scripts/check-no-dashes.sh", "echo one\n")
	write("workspace/two/scripts/ci/check-no-dashes.sh", "echo two\n")
	write("workspace/three/other/check-no-dashes.sh", "echo three\n")
	r := f.report(t)
	eq(t, "source", r.Copies.Source, "fallback ~/workspace/*/scripts")
	eq(t, "roots", r.Copies.Roots, 2)
	for _, c := range r.Copies.Tools {
		if c.Tool == "check-no-dashes" {
			eq(t, "copies", c.Copies, 2)
			eq(t, "variants", c.Variants, 2)
		}
	}
	eq(t, "hosts configured", r.SSH.HostsConfigured, 0)
	eq(t, "ssh commands without hosts", r.SSH.Commands, 0)
}

func TestExitCodes(t *testing.T) {
	f := setup(t, nil)
	code, out, _ := f.run(t, "--help")
	eq(t, "help exit", code, cli.ExitOK)
	if !strings.HasPrefix(out, "Usage: rice-agent audit") {
		t.Errorf("help starts with %q", strings.SplitN(out, "\n", 2)[0])
	}
	for _, args := range [][]string{
		{"--since", "soon"}, {"--since", "0d"}, {"--max-files", "-1"}, {"--bogus"}, {"extra"},
	} {
		if code, _, _ := f.run(t, args...); code != cli.ExitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, cli.ExitUsage)
		}
	}
	if code, _, _ := f.run(t, "--transcripts", filepath.Join(f.home, "missing")); code != cli.ExitEnv {
		t.Errorf("missing transcript root: exit %d, want %d", code, cli.ExitEnv)
	}
	empty := filepath.Join(f.home, "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := f.run(t, "--transcripts", empty); code != cli.ExitNegative {
		t.Errorf("empty window: exit %d, want %d", code, cli.ExitNegative)
	}
	if err := os.WriteFile(f.config, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := f.run(t, "--transcripts", f.projects); code != cli.ExitEnv {
		t.Errorf("bad config: exit %d, want %d", code, cli.ExitEnv)
	}
}

func TestParseSince(t *testing.T) {
	for in, want := range map[string]time.Duration{"14d": 14 * 24 * time.Hour, "2w": 14 * 24 * time.Hour, "36h": 36 * time.Hour, "90m": 90 * time.Minute} {
		got, err := parseSince(in)
		if err != nil || got != want {
			t.Errorf("parseSince(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "d", "-3d", "0h", "fortnight"} {
		if _, err := parseSince(in); err == nil {
			t.Errorf("parseSince(%q) accepted", in)
		}
	}
}

func TestHermesExportIsParsedWithoutPrintingCommands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hermes")
	line := `{"id":"session-a","messages":[{"role":"assistant","timestamp":1792238400,"tool_calls":[{"id":"call-a","function":{"name":"terminal","arguments":"{\"command\":\"rice-agent find audit && rice-agent check-no-dashes\"}"}}]},{"role":"tool","tool_call_id":"call-a","content":"{\"output\":\"ok\",\"exit_code\":0}"}]}`
	script := "#!/bin/sh\nprintf '%s\\n' '" + line + "'\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	old := hermesLookPath
	hermesLookPath = func(string) (string, error) { return path, nil }
	t.Cleanup(func() { hermesLookPath = old })

	var warnings []string
	c := newCollector([]*manifest.Tool{{Name: "check-no-dashes"}}, nil, func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	})
	info, notRead := collectHermes(c, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), 0, func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	})
	eq(t, "files", info.Files, 1)
	eq(t, "not read", notRead, 0)
	eq(t, "tool uses", c.toolUses, 1)
	eq(t, "bash commands", c.bashCommands, 1)
	eq(t, "find commands", c.findCommands, 1)
	eq(t, "tool invocations", c.tools[0].invocations, 1)
	if !c.tools[0].okSess["hermes:session-a"] {
		t.Fatal("successful Hermes tool result did not count as a successful use")
	}
	if strings.Join(warnings, " ") != "" {
		t.Fatalf("unexpected warning: %v", warnings)
	}
}

func TestHermesUnknownToolResultFailsClosed(t *testing.T) {
	line := []byte(`{"id":"session-a","messages":[{"role":"assistant","tool_calls":[{"id":"call-a","function":{"name":"terminal","arguments":"{\"command\":\"rice-agent check-no-dashes\"}"}}]},{"role":"tool","tool_call_id":"call-a","content":"unstructured result"}]}`)
	_, events, results, ok := scanHermesLine(line, time.Time{})
	if !ok || len(events) != 1 || len(results) != 1 || !results[0].isError {
		t.Fatalf("Hermes parse = ok:%v events:%d results:%+v", ok, len(events), results)
	}
}

func TestFindPatternSupportsRiceAgentAndLegacyAlias(t *testing.T) {
	for _, command := range []string{"rice-agent find issue", "agent-dry find issue"} {
		if !findRe.MatchString(command) {
			t.Errorf("find pattern did not match %q", command)
		}
	}
}

func TestPatterns(t *testing.T) {
	mut := map[string]bool{
		"git commit -m x":              true,
		"git -C /r push origin main":   true,
		"gh pr create --fill":          true,
		"rm -rf build":                 true,
		"cd x && mv a b":               true,
		"sed -i '' 's/a/b/' f":         true,
		"sed -E -i 's/a/b/' f":         true,
		"perl -0pi -e 's/a/b/' f":      true,
		"perl -0777 -pi -e 's/a/b/' f": true,
		"echo perform the task":        false,
		"docker run --rm img":          false,
		"sed -n '1,5p' f":              false,
		"git commit-tree x":            false,
		"git log --oneline":            false,
		"gh pr view 3":                 false,
		"printf 'rmdir'":               false,
	}
	for cmd, want := range mut {
		if got := mutateBashRe.MatchString(cmd); got != want {
			t.Errorf("mutation %q = %v, want %v", cmd, got, want)
		}
	}
	inv := invokeRe("host-run")
	for cmd, want := range map[string]bool{
		"host-run box -- uptime":         true,
		"~/.local/bin/host-run box true": true,
		"x=$(host-run box true)":         true,
		"cat tools/host-run/tool.json":   false,
		"my-host-run box":                false,
		"host-runner box":                false,
	} {
		if got := inv.MatchString(cmd); got != want {
			t.Errorf("invoke %q = %v, want %v", cmd, got, want)
		}
	}
	h := hostRe("box", "10.0.0.5")
	for cmd, want := range map[string]bool{
		"ssh me@10.0.0.5 true": true,
		"ssh box true":         true,
		"ssh 10.0.0.50 true":   false,
		"ssh mybox-2 true":     false,
	} {
		if got := h.MatchString(cmd); got != want {
			t.Errorf("host %q = %v, want %v", cmd, got, want)
		}
	}
	for tok, want := range map[string]string{"ssh": "ssh", "hvs.abc": "<redacted>", strings.Repeat("a", 40): "<redacted>"} {
		if got := redactToken(tok); got != want {
			t.Errorf("redactToken(%q) = %q, want %q", tok, got, want)
		}
	}
}

// TestLongLine checks a tool_use line far larger than the read buffer.
func TestLongLine(t *testing.T) {
	pad := strings.Repeat("x", 3<<20)
	line := `{"type":"assistant","timestamp":"2026-10-16T00:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_long","name":"Bash","input":{"command":"gh pr merge 1 # ` + pad + `"}}]}}` + "\n"
	p := filepath.Join(t.TempDir(), "long.jsonl")
	if err := os.WriteFile(p, []byte(line+line[:20]), 0o644); err != nil {
		t.Fatal(err)
	}
	var got []event
	err := scanFile(p, time.Time{}, func(ev event) { got = append(got, ev) }, func([]byte) bool { return false }, func(result) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].id != "toolu_long" || !ghMergeRe.MatchString(got[0].cmd) {
		t.Fatalf("long line events = %d", len(got))
	}
}

func TestCommandUsageAcrossClaudeAndCodex(t *testing.T) {
	f := setup(t, nil)
	f.projects = filepath.Join(f.home, ".claude", "projects")
	claudeSource := testdata(t, "usage", "claude", "-work-usage", "aaaaaaaa-aaaa-4aaa-8aaa-000000000001.jsonl")
	claudeTarget := filepath.Join(f.projects, "-work-usage", filepath.Base(claudeSource))
	if err := os.MkdirAll(filepath.Dir(claudeTarget), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(claudeSource, claudeTarget); err != nil {
		t.Fatal(err)
	}
	codexSource := testdata(t, "usage", "codex", "2026", "10", "16", "rollout-2026-10-16T12-00-00-aaaaaaaa-aaaa-4aaa-8aaa-000000000002.jsonl")
	codexTarget := filepath.Join(f.home, ".codex", "sessions", "2026", "10", "16", filepath.Base(codexSource))
	if err := os.MkdirAll(filepath.Dir(codexTarget), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(codexSource, codexTarget); err != nil {
		t.Fatal(err)
	}
	f.vars["HOSTNAME"] = "test-mac"

	code, output, stderr := f.run(t, "--transcripts", f.projects, "--since", "14d", "--json")
	if code != cli.ExitOK {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	var got struct {
		Transcripts  transcriptInfo `json:"transcripts"`
		CommandUsage struct {
			ByDay []struct {
				Day   string `json:"day"`
				Find  int    `json:"find"`
				New   int    `json:"new"`
				Check int    `json:"check"`
				Guide int    `json:"guide"`
			} `json:"by_day"`
			ByHost []struct {
				Host  string `json:"host"`
				Find  int    `json:"find"`
				New   int    `json:"new"`
				Check int    `json:"check"`
				Guide int    `json:"guide"`
			} `json:"by_host"`
			ByHarness []struct {
				Harness string `json:"harness"`
				Find    int    `json:"find"`
				New     int    `json:"new"`
				Check   int    `json:"check"`
				Guide   int    `json:"guide"`
			} `json:"by_harness"`
		} `json:"command_usage"`
	}
	if err := json.Unmarshal([]byte(output), &got); err != nil {
		t.Fatalf("decode report: %v\n%s", err, output)
	}
	if got.Transcripts.Files != 2 || got.Transcripts.Sessions != 2 {
		t.Fatalf("transcripts = %+v, want Claude and Codex fixture sessions", got.Transcripts)
	}
	findDay := func(day string) [4]int {
		for _, row := range got.CommandUsage.ByDay {
			if row.Day == day {
				return [4]int{row.Find, row.New, row.Check, row.Guide}
			}
		}
		return [4]int{}
	}
	findHost := func(host string) [4]int {
		for _, row := range got.CommandUsage.ByHost {
			if row.Host == host {
				return [4]int{row.Find, row.New, row.Check, row.Guide}
			}
		}
		return [4]int{}
	}
	findHarness := func(harness string) [4]int {
		for _, row := range got.CommandUsage.ByHarness {
			if row.Harness == harness {
				return [4]int{row.Find, row.New, row.Check, row.Guide}
			}
		}
		return [4]int{}
	}
	if got := findDay("2026-10-15"); got != [4]int{1, 1, 0, 0} {
		t.Errorf("2026-10-15 commands = %v, want find=1 new=1 check=0 guide=0", got)
	}
	if got := findDay("2026-10-16"); got != [4]int{1, 0, 1, 2} {
		t.Errorf("2026-10-16 commands = %v, want find=1 new=0 check=1 guide=2", got)
	}
	if got := findHost("test-mac"); got != [4]int{2, 0, 0, 2} {
		t.Errorf("test-mac commands = %v, want find=2 new=0 check=0 guide=2", got)
	}
	if got := findHost("alpha-host"); got != [4]int{0, 1, 1, 0} {
		t.Errorf("alpha-host commands = %v, want find=0 new=1 check=1 guide=0", got)
	}
	if got := findHarness("claude"); got != [4]int{1, 1, 1, 1} {
		t.Errorf("Claude commands = %v, want one of each", got)
	}
	if got := findHarness("codex"); got != [4]int{1, 0, 0, 1} {
		t.Errorf("Codex commands = %v, want find=1 new=0 check=0 guide=1", got)
	}

	textCode, text, textStderr := f.run(t, "--transcripts", f.projects, "--since", "14d")
	if textCode != cli.ExitOK {
		t.Fatalf("text exit %d, stderr: %s", textCode, textStderr)
	}
	for _, want := range []string{"day 2026-10-15", "host alpha-host", "harness codex", "find=1 new=0 check=0 guide=1"} {
		if !strings.Contains(text, want) {
			t.Errorf("text report missing %q", want)
		}
	}
	if strings.Contains(text, "rice-agent find codex") {
		t.Error("text report leaked command text")
	}
}

func copyFile(src, dst string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, raw, 0o644)
}
