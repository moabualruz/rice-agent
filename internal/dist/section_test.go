package dist

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	drySource      = filepath.Join(fixtures, "sections", "dry-policy.md")
	languageSource = filepath.Join(fixtures, "sections", "language-policy.md")
)

func sourceBlock(t *testing.T, path string) string {
	t.Helper()
	return strings.TrimSpace(readFile(t, path))
}

func sectionVars(home string) map[string]string {
	return map[string]string{"HOME": home, "PATH": os.Getenv("PATH")}
}

func installSection(t *testing.T, home, name, source string, extra ...string) (int, sectionReceipt, string) {
	t.Helper()
	args := append([]string{"section", "install", "--name", name, "--source", source, "--destination", home}, extra...)
	code, out, errb := run(t, sectionVars(home), args...)
	var r sectionReceipt
	if code == 0 {
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatalf("receipt %q: %v", out, err)
		}
	}
	return code, r, errb
}

func allUnchanged(r sectionReceipt) bool {
	for _, x := range r.Targets {
		if x.Action != "unchanged" {
			return false
		}
	}
	return len(r.Targets) > 0
}

// snapshot records every regular file and symlink under dir.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			dest, _ := os.Readlink(p)
			files[p] = "link:" + dest
		} else if info.Mode().IsRegular() {
			files[p] = readFile(t, p)
		}
		return nil
	})
	return files
}

func sameSnapshot(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestSectionPreservesContentSymlinksAndMDCFrontMatter(t *testing.T) {
	home := tempDir(t)
	block := sourceBlock(t, drySource)
	compiled := filepath.Join(home, ".config/agent-instructions/compiled/codex-AGENTS.md")
	writeFile(t, compiled, "Existing owner rule.\n", 0o640)
	link := filepath.Join(home, ".codex/AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(compiled, link); err != nil {
		t.Fatal(err)
	}
	mdc := filepath.Join(home, ".cursor/rules/agent-instructions.mdc")
	writeFile(t, mdc, "---\r\nalwaysApply: true\r\n---\r\nCursor owner rule.\r\n", 0o644)
	soul := filepath.Join(home, ".hermes/SOUL.md")
	writeFile(t, soul, "Existing Hermes identity.\n\n  \n", 0o644)

	code, r, errb := installSection(t, home, "DRY_POLICY", drySource)
	if code != 0 {
		t.Fatalf("install: %d %s", code, errb)
	}
	bytes := readFile(t, compiled)
	if bytes != block+"\n\nExisting owner rule.\n" {
		t.Fatalf("compiled: %q", bytes)
	}
	if dest, _ := os.Readlink(link); dest != compiled {
		t.Fatalf("symlink replaced: %q", dest)
	}
	if info, _ := os.Stat(compiled); info.Mode().Perm() != 0o640 {
		t.Fatalf("mode not preserved: %v", info.Mode())
	}
	if got := readFile(t, mdc); got != "---\r\nalwaysApply: true\r\n---\r\n"+block+"\n\nCursor owner rule.\r\n" {
		t.Fatalf("mdc: %q", got)
	}
	if got := readFile(t, soul); got != block+"\n\nExisting Hermes identity.\n\n  \n" {
		t.Fatalf("SOUL.md: %q", got)
	}
	// The reference creates the shared files and always-targets; existing-only targets stay absent.
	for _, p := range []string{".config/agent-instructions/shared/KERNEL.md", ".config/agent-instructions/shared/AGENTS.md",
		"AGENTS.md", ".claude/CLAUDE.md", ".config/opencode/AGENTS.md", ".gemini/GEMINI.md", ".copilot/copilot-instructions.md"} {
		if got := readFile(t, filepath.Join(home, p)); got != block+"\n\n" {
			t.Errorf("%s: %q", p, got)
		}
	}
	for _, p := range []string{"AGENTS.override.md", ".codex/AGENTS.override.md", ".grok/AGENTS.md", ".pi/agent/AGENTS.md", ".hermes/AGENTS.md", ".hermes/HERMES.md"} {
		if exists(filepath.Join(home, p)) {
			t.Errorf("existing-only target %s was created", p)
		}
	}
	written := 0
	for _, x := range r.Targets {
		if x.Path == compiled {
			written++
			if x.Action != "inserted" || len(x.Entries) != 2 {
				t.Errorf("compiled result: %+v", x)
			}
		}
		if x.SHA256 != sha256Bytes([]byte(readFile(t, x.Path))) {
			t.Errorf("%s: receipt sha256 mismatch", x.Path)
		}
	}
	if written != 1 || r.SourceSHA256 != sha256Bytes([]byte(block)) || len(r.Targets) != 10 {
		t.Fatalf("receipt: written %d targets %d %+v", written, len(r.Targets), r)
	}

	code, r, errb = installSection(t, home, "DRY_POLICY", drySource)
	if code != 0 || !allUnchanged(r) || readFile(t, compiled) != bytes {
		t.Fatalf("second run: %d %s %+v", code, errb, r.Targets)
	}

	code, out, errb := run(t, sectionVars(home), "section", "check", "--name", "DRY_POLICY", "--source", drySource, "--destination", home)
	if code != 0 || strings.Contains(out, "FAIL") || strings.Count(out, "PASS ") != 10 {
		t.Fatalf("check: %d %s %s", code, out, errb)
	}

	// Parity fails on a dash inside a live block and on a block that drifted from the source.
	claude := filepath.Join(home, ".claude/CLAUDE.md")
	writeFile(t, claude, strings.Replace(readFile(t, claude), "Decide reuse", "Decide "+emDash+" reuse", 1), 0o644)
	code, out, _ = run(t, sectionVars(home), "section", "check", "--name", "DRY_POLICY", "--source", drySource, "--destination", home)
	if code != 1 || !strings.Contains(out, "FAIL "+claude+": block differs from source; block contains an em or en dash") {
		t.Fatalf("dash check: %d %s", code, out)
	}
}

func TestSectionReplacesInPlaceAndInsertsAfterFrontMatter(t *testing.T) {
	home := tempDir(t)
	block := sourceBlock(t, drySource)
	agents := filepath.Join(home, "AGENTS.md")
	writeFile(t, agents, "Owner heading.\n<!-- DRY_POLICY_START -->\nStale owned guidance.\n<!-- DRY_POLICY_END -->\nOwner footer.\n", 0o644)
	gemini := filepath.Join(home, ".gemini/GEMINI.md")
	writeFile(t, gemini, "---\ntitle: gemini\n---\nGemini body.\n", 0o644)
	code, r, errb := installSection(t, home, "DRY_POLICY", drySource)
	if code != 0 {
		t.Fatalf("install: %s", errb)
	}
	if got := readFile(t, agents); got != "Owner heading.\n"+block+"\nOwner footer.\n" {
		t.Fatalf("replace in place: %q", got)
	}
	if got := readFile(t, gemini); got != "---\ntitle: gemini\n---\n"+block+"\n\nGemini body.\n" {
		t.Fatalf("front matter: %q", got)
	}
	for _, x := range r.Targets {
		if x.Path == agents && x.Action != "updated" {
			t.Fatalf("agents action %s", x.Action)
		}
	}
}

func TestSectionRefusesMalformedMarkersWithoutMutation(t *testing.T) {
	start, end := "<!-- DRY_POLICY_START -->", "<!-- DRY_POLICY_END -->"
	for _, malformed := range []string{
		"Owner text.\n" + start + "\nMissing end marker.\n",
		"Owner text.\n" + end + "\nReversed markers.\n" + start + "\n",
		start + "\none\n" + end + "\n" + start + "\ntwo\n" + end + "\n",
	} {
		home := tempDir(t)
		writeFile(t, filepath.Join(home, ".claude/CLAUDE.md"), "Clean file.\n", 0o644)
		writeFile(t, filepath.Join(home, ".hermes/SOUL.md"), malformed, 0o644)
		before := snapshot(t, home)
		code, _, errb := installSection(t, home, "DRY_POLICY", drySource)
		if code != 1 || !strings.Contains(errb, "ambiguous managed section: "+filepath.Join(home, ".hermes/SOUL.md")) {
			t.Fatalf("malformed %q: code %d %s", malformed, code, errb)
		}
		if !sameSnapshot(before, snapshot(t, home)) {
			t.Fatalf("malformed %q: files changed", malformed)
		}
	}
}

func TestSectionSecondNameCoexistsWithLanguagePolicy(t *testing.T) {
	home := tempDir(t)
	agents := filepath.Join(home, "AGENTS.md")
	writeFile(t, agents, "Owner rule.\n", 0o644)
	if code, _, errb := installSection(t, home, "LANGUAGE_POLICY", languageSource); code != 0 {
		t.Fatal(errb)
	}
	if code, _, errb := installSection(t, home, "DRY_POLICY", drySource); code != 0 {
		t.Fatal(errb)
	}
	dry, lang := sourceBlock(t, drySource), sourceBlock(t, languageSource)
	if got := readFile(t, agents); got != dry+"\n\n"+lang+"\n\nOwner rule.\n" {
		t.Fatalf("both sections: %q", got)
	}
	// A new LANGUAGE_POLICY revision replaces only its own block.
	revised := filepath.Join(tempDir(t), "language-policy.md")
	newLang := strings.Replace(lang, "smallest reliable option", "smallest sound option", 1)
	writeFile(t, revised, newLang+"\n", 0o644)
	code, r, errb := installSection(t, home, "LANGUAGE_POLICY", revised)
	if code != 0 || readFile(t, agents) != dry+"\n\n"+newLang+"\n\nOwner rule.\n" {
		t.Fatalf("revision: %d %s %q", code, errb, readFile(t, agents))
	}
	for _, x := range r.Targets {
		if x.Action != "updated" {
			t.Errorf("%s: %s", x.Path, x.Action)
		}
	}
	for _, s := range []struct{ name, src string }{{"DRY_POLICY", drySource}, {"LANGUAGE_POLICY", revised}} {
		if code, r, _ := installSection(t, home, s.name, s.src); code != 0 || !allUnchanged(r) {
			t.Errorf("%s rerun not unchanged", s.name)
		}
		if code, out, _ := run(t, sectionVars(home), "section", "check", "--name", s.name, "--source", s.src, "--destination", home); code != 0 {
			t.Errorf("%s check: %s", s.name, out)
		}
	}
}

func TestSectionRefusesTargetsEscapingDestination(t *testing.T) {
	fixture := tempDir(t)
	home := filepath.Join(fixture, "home")
	outside := filepath.Join(fixture, "outside.md")
	writeFile(t, outside, "Outside owner file.\n", 0o644)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, ".claude/CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, fixture)
	code, _, errb := installSection(t, home, "DRY_POLICY", drySource)
	if code != 1 || !strings.Contains(errb, "escapes destination") {
		t.Fatalf("escape: %d %s", code, errb)
	}
	if !sameSnapshot(before, snapshot(t, fixture)) {
		t.Fatal("escape refusal changed files")
	}
}

func TestSectionFollowsDanglingSymlink(t *testing.T) {
	home := tempDir(t)
	dest := filepath.Join(home, ".config/agent-instructions/compiled/gemini-GEMINI.md")
	link := filepath.Join(home, ".gemini/GEMINI.md")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dest, link); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run(t, sectionVars(home), "section", "check", "--name", "DRY_POLICY", "--source", drySource, "--destination", home)
	if code != 1 || !strings.Contains(out, "dangling symlink") {
		t.Fatalf("check before install: %d %s", code, out)
	}
	if code, _, errb := installSection(t, home, "DRY_POLICY", drySource); code != 0 {
		t.Fatal(errb)
	}
	if d, _ := os.Readlink(link); d != dest || readFile(t, link) != sourceBlock(t, drySource)+"\n\n" {
		t.Fatalf("dangling link not preserved: %q", d)
	}
}

// Grok loads each instruction file in full (its user guide, 12-project-rules.md: "no character
// cap and no truncation"), so a block deep in the Grok entry passes.
func TestSectionGrokEntryHasNoCharacterLimit(t *testing.T) {
	home := tempDir(t)
	block := sourceBlock(t, drySource)
	grok := filepath.Join(home, ".grok/AGENTS.md")
	writeFile(t, grok, strings.Repeat("g", 50000)+"\n"+block+"\n", 0o644)
	if code, _, errb := installSection(t, home, "DRY_POLICY", drySource); code != 0 {
		t.Fatal(errb)
	}
	code, out, _ := run(t, sectionVars(home), "section", "check", "--name", "DRY_POLICY", "--source", drySource, "--destination", home, "--json")
	var rep sectionCheckReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if code != 0 || !rep.Pass {
		t.Fatalf("grok entry: %d %+v", code, rep)
	}
}

// A wrapper that loads the kernel by import keeps no copy: install removes a stale block,
// a rerun leaves it alone, and check fails if a copy comes back.
func TestSectionWrapperDefersToKernel(t *testing.T) {
	home := tempDir(t)
	block := sourceBlock(t, drySource)
	compiled := filepath.Join(home, ".config/agent-instructions/compiled")
	wrapper := filepath.Join(compiled, "CLAUDE.md")
	body := "# Claude Code Global Rules\n\n" + deferLine + "\n\n@~/.config/agent-instructions/shared/KERNEL.md\n"
	writeFile(t, wrapper, block+"\n\n"+body, 0o644)
	link := filepath.Join(home, ".claude/CLAUDE.md")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(wrapper, link); err != nil {
		t.Fatal(err)
	}
	front := "---\ndescription: Shared owner instructions\nalwaysApply: true\n---\n"
	mdcBody := "# Cursor Global Rules\n" + deferLine + "\n"
	mdc := filepath.Join(compiled, "cursor-agent-instructions.mdc")
	writeFile(t, mdc, front+block+"\n\n"+mdcBody, 0o644)
	override := filepath.Join(compiled, "codex-home-AGENTS.override.md")
	writeFile(t, override, "# Home directory\n"+block+"\n"+deferLine+"\n", 0o644)
	kernel := filepath.Join(home, ".config/agent-instructions/shared/KERNEL.md")
	writeFile(t, kernel, "# Agent Kernel\n", 0o644)

	code, r, errb := installSection(t, home, "DRY_POLICY", drySource)
	if code != 0 {
		t.Fatalf("install: %d %s", code, errb)
	}
	for path, want := range map[string]string{wrapper: body, mdc: front + mdcBody, override: "# Home directory\n" + deferLine + "\n",
		kernel: block + "\n\n# Agent Kernel\n"} {
		if got := readFile(t, path); got != want {
			t.Errorf("%s: %q, want %q", path, got, want)
		}
	}
	if dest, _ := os.Readlink(link); dest != wrapper {
		t.Fatalf("symlink replaced: %q", dest)
	}
	removed := 0
	for _, x := range r.Targets {
		if x.Action == "removed" {
			removed++
		}
	}
	if removed != 3 {
		t.Fatalf("removed %d: %+v", removed, r.Targets)
	}
	code, r, errb = installSection(t, home, "DRY_POLICY", drySource)
	if code != 0 || readFile(t, wrapper) != body {
		t.Fatalf("rerun: %d %s", code, errb)
	}
	for _, x := range r.Targets {
		if x.Path == wrapper && x.Action != "deferred" {
			t.Fatalf("rerun wrapper action %s", x.Action)
		}
	}
	code, out, _ := run(t, sectionVars(home), "section", "check", "--name", "DRY_POLICY", "--source", drySource, "--destination", home)
	if code != 0 || !strings.Contains(out, "PASS "+wrapper+" (loads it from shared/KERNEL.md)") {
		t.Fatalf("check: %d %s", code, out)
	}
	writeFile(t, wrapper, block+"\n\n"+body, 0o644)
	code, out, _ = run(t, sectionVars(home), "section", "check", "--name", "DRY_POLICY", "--source", drySource, "--destination", home)
	if code != 1 || !strings.Contains(out, "FAIL "+wrapper+": loads sections from shared/KERNEL.md but carries a copy of the block") {
		t.Fatalf("duplicate check: %d %s", code, out)
	}
}

func TestSectionCursorProjectRules(t *testing.T) {
	requireCommands(t, "git")
	fixture := tempDir(t)
	home, project := filepath.Join(fixture, "home"), filepath.Join(fixture, "project")
	for _, d := range []string{home, project} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := exec.Command("git", "init", "-q", project).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	code, r, errb := installSection(t, home, "DRY_POLICY", drySource, "--project-root", project)
	if code != 0 {
		t.Fatalf("install: %s", errb)
	}
	rule := filepath.Join(project, ".cursor/rules/dry-policy.mdc")
	if len(r.ProjectRules) != 1 || r.ProjectRules[0].Scope != "project" || r.ProjectRules[0].Path != rule || !r.ProjectRules[0].AlwaysApply {
		t.Fatalf("project rules: %+v", r.ProjectRules)
	}
	soulEntry := false
	for _, e := range r.Entries {
		soulEntry = soulEntry || e.Path == filepath.Join(home, ".hermes/SOUL.md")
	}
	if !soulEntry || !strings.Contains(readFile(t, filepath.Join(home, ".hermes/SOUL.md")), "<!-- DRY_POLICY_START -->") {
		t.Fatal("Hermes SOUL.md not installed")
	}
	if !regexp.MustCompile(`^---\ndescription: [^\n]+\nalwaysApply: true\n---\n\n`).MatchString(readFile(t, rule)) {
		t.Fatalf("project rule: %q", readFile(t, rule))
	}
	if code, r, _ := installSection(t, home, "DRY_POLICY", drySource, "--project-root", project); code != 0 || !allUnchanged(r) {
		t.Fatal("second project run not unchanged")
	}

	writeFile(t, rule, "---\nalwaysApply: false\n---\nUser rule.\n", 0o644)
	code, _, errb = installSection(t, home, "DRY_POLICY", drySource, "--project-root", project)
	if code != 1 || !strings.Contains(errb, "another owner") || !strings.Contains(readFile(t, rule), "User rule.") {
		t.Fatalf("foreign rule: %d %s", code, errb)
	}

	if err := os.RemoveAll(filepath.Join(project, ".cursor")); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(fixture, "external")
	if err := os.MkdirAll(external, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(project, ".cursor")); err != nil {
		t.Fatal(err)
	}
	code, _, errb = installSection(t, home, "DRY_POLICY", drySource, "--project-root", project)
	if code != 1 || !strings.Contains(errb, "symlink") {
		t.Fatalf("symlinked .cursor: %d %s", code, errb)
	}
	if entries, _ := os.ReadDir(external); len(entries) != 0 {
		t.Fatal("wrote through the symlinked .cursor")
	}

	code, _, errb = installSection(t, home, "DRY_POLICY", drySource, "--project-root", home)
	if code != 1 || !strings.Contains(errb, "actual Git worktree root") {
		t.Fatalf("not a git root: %d %s", code, errb)
	}
}

func TestSectionUsageAndSourceValidation(t *testing.T) {
	home := tempDir(t)
	vars := sectionVars(home)
	bad := filepath.Join(home, "bad.md")
	writeFile(t, bad, "<!-- DRY_POLICY_START -->\nno end\n", 0o644)
	dashed := filepath.Join(home, "dashed.md")
	writeFile(t, dashed, "<!-- DRY_POLICY_START -->\nuse "+enDash+" here\n<!-- DRY_POLICY_END -->\n", 0o644)
	cases := []struct {
		args []string
		code int
	}{
		{[]string{"section"}, 2},
		{[]string{"section", "remove"}, 2},
		{[]string{"section", "install", "--source", drySource}, 2},
		{[]string{"section", "install", "--name", "dry-policy", "--source", drySource}, 2},
		{[]string{"section", "install", "--name", "DRY_POLICY", "--source", bad, "--destination", home}, 2},
		{[]string{"section", "install", "--name", "LANGUAGE_POLICY", "--source", drySource, "--destination", home}, 2},
		{[]string{"section", "install", "--name", "DRY_POLICY", "--source", filepath.Join(home, "missing.md"), "--destination", home}, 3},
		{[]string{"section", "install", "--name", "DRY_POLICY", "--source", dashed, "--destination", home}, 1},
		{[]string{"section", "check", "--name", "DRY_POLICY", "--source", drySource, "--destination", home}, 1},
		{[]string{"section", "check", "--name", "DRY_POLICY", "--source", drySource, "--project-root", home}, 2},
		{[]string{"section", "install", "--help"}, 0},
	}
	for _, c := range cases {
		if code, _, errb := run(t, vars, c.args...); code != c.code {
			t.Errorf("%v: code %d, want %d (%s)", c.args, code, c.code, errb)
		}
	}
	entries, _ := os.ReadDir(home)
	if len(entries) != 2 {
		t.Fatalf("usage cases wrote files: %v", entries)
	}
	code, out, _ := run(t, vars, "section", "install", "--name", "DRY_POLICY", "--source", drySource, "--destination", home, "--dry-run")
	if code != 0 || !strings.Contains(out, `"dry_run": true`) || exists(filepath.Join(home, "AGENTS.md")) {
		t.Fatalf("dry run: %d %s", code, out)
	}
}
