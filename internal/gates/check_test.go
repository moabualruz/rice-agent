package gates

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moabualruz/rice-agent/internal/registry"
)

// Each fixture under testdata/gates/root/tools fails exactly one gate (or none), and a failed
// gate stops the later ones.
func TestCheckFixtures(t *testing.T) {
	requireCommands(t, "shellcheck")
	isolate(t, fixtureRoot)
	useIndex(t, nil)
	cases := []struct {
		tool, want, reason string
		code               int
	}{
		{"good-tool", "PASS PASS PASS PASS PASS PASS", "", 0},
		{"g1-bad-manifest", "FAIL SKIPPED SKIPPED SKIPPED SKIPPED SKIPPED", "Use when", 1},
		{"g2-declared-distinct", "PASS PASS PASS PASS PASS PASS", "", 0},
		{"g3-bad-help", "PASS PASS FAIL SKIPPED SKIPPED SKIPPED", `does not start with "Usage:"`, 1},
		{"g3-undocumented-dry-run", "PASS PASS FAIL SKIPPED SKIPPED SKIPPED", "dry_run flag --dry-run", 1},
		{"g4-shellcheck", "PASS PASS PASS FAIL SKIPPED SKIPPED", "SC2086", 1},
		{"g5-failing-test", "PASS PASS PASS PASS FAIL SKIPPED", "FAIL motivating case", 1},
		{"g6-bad-origin", "PASS PASS PASS PASS PASS FAIL", "not a calendar date", 1},
		{"g6-network-read", "PASS PASS PASS PASS PASS WARN", "calls curl while side_effects is read", 0},
	}
	for _, c := range cases {
		t.Run(c.tool, func(t *testing.T) {
			code, out, _ := run(t, "check", c.tool)
			lines := parseCheck(t, out)
			if got := statuses(lines, c.tool); got != c.want {
				t.Fatalf("statuses %q, want %q\n%s", got, c.want, out)
			}
			if code != c.code {
				t.Errorf("exit %d, want %d\n%s", code, c.code, out)
			}
			if c.reason != "" && !strings.Contains(out, c.reason) {
				t.Errorf("output lacks %q\n%s", c.reason, out)
			}
			if !strings.Contains(out, "check: 1 tools:") {
				t.Errorf("no summary line\n%s", out)
			}
		})
	}
}

func TestCheckAllFixtures(t *testing.T) {
	requireCommands(t, "shellcheck")
	isolate(t, fixtureRoot)
	useIndex(t, nil)
	code, out, _ := run(t, "check", "--all")
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(out, "check: 9 tools: 3 passed, 6 failed, 0 incomplete") {
		t.Errorf("summary wrong\n%s", out)
	}
	if l := parseCheck(t, out)["(repository)/G4"]; l.status != "PASS" {
		t.Errorf("repository line %+v\n%s", l, out)
	}
}

func TestCheckDuplicate(t *testing.T) {
	requireCommands(t, "shellcheck")
	isolate(t, fixtureRoot)

	useIndex(t, nil, hit("other-tool", "script"))
	code, out, _ := run(t, "check", "good-tool")
	lines := parseCheck(t, out)
	if got := statuses(lines, "good-tool"); got != "PASS FAIL SKIPPED SKIPPED SKIPPED SKIPPED" || code != 1 {
		t.Fatalf("undeclared match: exit %d, statuses %q\n%s", code, got, out)
	}
	if !strings.Contains(lines["good-tool/G2"].reason, "other-tool") {
		t.Errorf("reason does not name the match: %q", lines["good-tool/G2"].reason)
	}

	// The tool itself, wanted families, earlier misses, low scores and skills installed in a home
	// directory (which differ per host) never count.
	low := hit("other-tool", "script")
	low.Score = registry.Threshold - 0.5
	installed := hit("home-skill", "skill")
	installed.Source = "skill"
	useIndex(t, nil, hit("good-tool", "script"), hit("good-family", "wanted"), hit("good-miss", "seen"), low, installed)
	if code, out, _ := run(t, "check", "good-tool"); code != 0 {
		t.Errorf("non-duplicates counted: exit %d\n%s", code, out)
	}

	// distinct_from naming the match clears it.
	useIndex(t, nil, hit("good-tool", "script"))
	if code, out, _ := run(t, "check", "g2-declared-distinct"); code != 0 {
		t.Errorf("declared distinct: exit %d\n%s", code, out)
	}

	// A declaration made by the other tool clears the pair from this side too.
	useIndex(t, nil, hit("g2-declared-distinct", "script"))
	if code, out, _ := run(t, "check", "good-tool"); code != 0 {
		t.Errorf("reverse declaration not honored: exit %d\n%s", code, out)
	}

	useIndex(t, errors.New("index unreadable"))
	code, out, _ = run(t, "check", "good-tool")
	if code != 3 || parseCheck(t, out)["good-tool/G2"].status != "FAIL" {
		t.Errorf("registry error: exit %d\n%s", code, out)
	}
}

func TestCheckPathCollision(t *testing.T) {
	requireCommands(t, "shellcheck")
	isolate(t, fixtureRoot)
	useIndex(t, nil)
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "good-tool"), "#!/bin/sh\nexit 0\n", 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	code, out, _ := run(t, "check", "good-tool")
	l := parseCheck(t, out)["good-tool/G1"]
	if code != 1 || l.status != "FAIL" || !strings.Contains(l.reason, "collides") {
		t.Fatalf("collision: exit %d, G1 %+v\n%s", code, l, out)
	}

	// A link into the tools directory (an install) is the tool itself.
	if err := os.Remove(filepath.Join(bin, "good-tool")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(fixtureRoot, "tools", "good-tool", "good-tool"), filepath.Join(bin, "good-tool")); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := run(t, "check", "good-tool"); code != 0 {
		t.Errorf("installed link counted as collision: exit %d\n%s", code, out)
	}
}

func TestCheckDashes(t *testing.T) {
	requireCommands(t, "shellcheck")
	root := rootWith(t, "good-tool")
	isolate(t, root)
	useIndex(t, nil)
	f, err := os.OpenFile(filepath.Join(root, "tools", "good-tool", "README.md"), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("A note " + enDash + " with a dash.\n")
	f.Close()
	code, out, _ := run(t, "check", "good-tool")
	l := parseCheck(t, out)["good-tool/G4"]
	if code != 1 || l.status != "FAIL" || !strings.Contains(l.reason, "README.md") {
		t.Fatalf("dash: exit %d, G4 %+v\n%s", code, l, out)
	}
}

func TestCheckPython(t *testing.T) {
	requireCommands(t, "shellcheck", "git")
	root := rootWith(t, "good-tool")
	isolate(t, root)
	useIndex(t, nil)
	gitInit(t, root)
	writeFile(t, filepath.Join(root, "scripts", "helper.py"), "print(1)\n", 0o644)

	// Untracked Python outside the tool is not the repository's.
	if code, out, _ := run(t, "check", "--all"); code != 0 {
		t.Fatalf("untracked file counted: exit %d\n%s", code, out)
	}
	gitAdd(t, root, "scripts/helper.py")
	code, out, _ := run(t, "check", "--all")
	lines := parseCheck(t, out)
	if code != 1 || lines["(repository)/G4"].status != "FAIL" || lines["good-tool/G4"].status != "FAIL" {
		t.Fatalf("tracked Python: exit %d\n%s", code, out)
	}
	if !strings.Contains(lines["(repository)/G4"].reason, "scripts/helper.py") {
		t.Errorf("reason does not name the file\n%s", out)
	}

	// A Python shebang inside the tool fails even untracked and without the extension.
	root2 := rootWith(t, "good-tool")
	isolate(t, root2)
	writeFile(t, filepath.Join(root2, "tools", "good-tool", "helper"), "#!/usr/bin/env python3\nprint(1)\n", 0o755)
	code, out, _ = run(t, "check", "good-tool")
	if l := parseCheck(t, out)["good-tool/G4"]; code != 1 || l.status != "FAIL" || !strings.Contains(l.reason, "helper") {
		t.Fatalf("python shebang: exit %d\n%s", code, out)
	}
}

func TestCheckGitleaks(t *testing.T) {
	requireCommands(t, "shellcheck", "gitleaks")
	root := rootWith(t, "good-tool")
	isolate(t, root)
	useIndex(t, nil)
	// A token-shaped value built at run time, so no secret-shaped literal is tracked.
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	var b strings.Builder
	for i := 0; i < 36; i++ {
		b.WriteByte(alphabet[(i*7+3)%len(alphabet)])
	}
	writeFile(t, filepath.Join(root, "tools", "good-tool", "settings.txt"), "value="+"ghp"+"_"+b.String()+"\n", 0o644)
	code, out, _ := run(t, "check", "good-tool")
	l := parseCheck(t, out)["good-tool/G4"]
	if code != 1 || l.status != "FAIL" || !strings.Contains(l.reason, "gitleaks") {
		t.Fatalf("gitleaks: exit %d, G4 %+v\n%s", code, l, out)
	}
	if strings.Contains(out, b.String()) {
		t.Errorf("check printed the secret value")
	}
}

func TestCheckSkipUnavailable(t *testing.T) {
	isolate(t, fixtureRoot)
	useIndex(t, nil)
	pathOnly(t, "bash", "dirname")

	code, out, _ := run(t, "check", "good-tool")
	lines := parseCheck(t, out)
	if got := statuses(lines, "good-tool"); code != 3 || got != "PASS PASS PASS FAIL SKIPPED SKIPPED" {
		t.Fatalf("missing shellcheck: exit %d, statuses %q\n%s", code, got, out)
	}
	if !strings.Contains(lines["good-tool/G4"].reason, "shellcheck not installed") {
		t.Errorf("reason %q", lines["good-tool/G4"].reason)
	}

	code, out, _ = run(t, "check", "--skip-unavailable", "good-tool")
	lines = parseCheck(t, out)
	if got := statuses(lines, "good-tool"); code != 1 || got != "PASS PASS PASS SKIPPED PASS PASS" {
		t.Fatalf("--skip-unavailable: exit %d, statuses %q\n%s", code, got, out)
	}
	if !strings.Contains(out, "0 passed, 0 failed, 1 incomplete") {
		t.Errorf("summary does not count the incomplete tool\n%s", out)
	}
}

func TestCheckHelpTimeout(t *testing.T) {
	root := rootWith(t, "good-tool")
	isolate(t, root)
	useIndex(t, nil)
	writeFile(t, filepath.Join(root, "tools", "good-tool", "good-tool"), "#!/usr/bin/env bash\nsleep 30\n", 0o755)
	saved := helpTimeout
	helpTimeout = 500 * time.Millisecond
	t.Cleanup(func() { helpTimeout = saved })
	start := time.Now()
	code, out, _ := run(t, "check", "good-tool")
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("timeout did not stop the child: %s", elapsed)
	}
	l := parseCheck(t, out)["good-tool/G3"]
	if code != 1 || l.status != "FAIL" || !strings.Contains(l.reason, "did not finish") {
		t.Fatalf("timeout: exit %d, G3 %+v\n%s", code, l, out)
	}
}

func TestCheckExitCodesDocumented(t *testing.T) {
	root := rootWith(t, "good-tool")
	isolate(t, root)
	useIndex(t, nil)
	p := filepath.Join(root, "tools", "good-tool", "tool.json")
	writeFile(t, p, strings.Replace(readFile(t, p), `"2": "usage error"`, `"1": "negative result"`, 1), 0o644)
	code, out, _ := run(t, "check", "good-tool")
	l := parseCheck(t, out)["good-tool/G3"]
	if code != 1 || l.status != "FAIL" || !strings.Contains(l.reason, "document 2") {
		t.Fatalf("exit codes: exit %d, G3 %+v\n%s", code, l, out)
	}
}

func TestCheckUsageAndEnvironment(t *testing.T) {
	isolate(t, fixtureRoot)
	useIndex(t, nil)
	for _, args := range [][]string{{}, {"--all", "good-tool"}, {"--bogus"}} {
		if code, _, _ := run(t, "check", args...); code != 2 {
			t.Errorf("%q: exit %d, want 2", args, code)
		}
	}
	code, out, _ := run(t, "check", "--help")
	if code != 0 || !strings.HasPrefix(out, "Usage: rice-agent check") {
		t.Errorf("--help: exit %d\n%s", code, out)
	}
	code, out, _ = run(t, "check", "no-such-tool")
	if code != 1 || !strings.Contains(parseCheck(t, out)["no-such-tool/G1"].reason, "no tool directory") {
		t.Errorf("unknown tool: exit %d\n%s", code, out)
	}
	t.Setenv("AGENT_DRY_ROOT", t.TempDir())
	if code, _, _ := run(t, "check", "--all"); code != 3 {
		t.Errorf("no root: exit %d, want 3", code)
	}
}

func TestCheckEmptyRoot(t *testing.T) {
	isolate(t, emptyRoot(t))
	useIndex(t, nil)
	code, out, _ := run(t, "check", "--all")
	if code != 0 || !strings.Contains(out, "check: 0 tools") {
		t.Errorf("empty root: exit %d\n%s", code, out)
	}
}

func TestCheckPromotedToolKeepsUsageCode(t *testing.T) {
	root := rootWith(t, "good-tool")
	isolate(t, root)
	useIndex(t, nil)
	p := filepath.Join(root, "tools", "good-tool", "tool.json")
	raw := strings.Replace(readFile(t, p), `"2": "usage error"`, `"64": "usage error"`, 1)
	raw = strings.Replace(raw, `"origin": {`, `"origin": {"promoted_from": "other-repo/scripts/good-tool.sh", `, 1)
	writeFile(t, p, raw, 0o644)
	_, out, _ := run(t, "check", "good-tool")
	if l := parseCheck(t, out)["good-tool/G3"]; l.status != "PASS" {
		t.Fatalf("promoted tool with usage code 64: G3 %+v\n%s", l, out)
	}
}
