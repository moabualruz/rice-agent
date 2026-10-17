package gates

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moabualruz/agent-dry/internal/manifest"
)

func fixedNow(t *testing.T) {
	t.Helper()
	saved := now
	now = func() time.Time { return time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { now = saved })
}

// Scaffolds pass every gate as written. These use the real registry on an empty root, so G2
// sees only the new tool itself.
func TestNewGlobalBashPassesCheck(t *testing.T) {
	requireCommands(t, "shellcheck")
	for _, se := range []string{"read", "destructive"} {
		t.Run(se, func(t *testing.T) {
			root := emptyRoot(t)
			isolate(t, root)
			fixedNow(t)
			name := "zz-sample-" + se
			args := []string{name, "--kind", "script", "--scope", "global", "--task", "rice-kb:issues/04", "--side-effects", se,
				"--summary", "Use when a gate test needs a scaffold.", "--intent", "scaffold sample", "--intent", "sample scaffold tool"}

			code, out, errOut := run(t, "new", append(args, "--dry-run")...)
			if code != 0 || !strings.Contains(out, "plan: create") || !strings.Contains(out, "dry run: nothing written") {
				t.Fatalf("--dry-run: exit %d\n%s%s", code, out, errOut)
			}
			if exists(filepath.Join(root, "tools", name)) {
				t.Fatal("--dry-run wrote files")
			}

			code, out, errOut = run(t, "new", args...)
			if code != 0 {
				t.Fatalf("new: exit %d\n%s%s", code, out, errOut)
			}
			dir := filepath.Join(root, "tools", name)
			tool, err := manifest.Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			if tool.Status != "trial" || tool.Origin.Builder != "test-model" || tool.Origin.Date != "2026-10-17" ||
				tool.Origin.Task != "rice-kb:issues/04" || len(tool.Intents) != 3 {
				t.Errorf("manifest %+v", tool)
			}
			if (se == "read") != (tool.DryRun == "") {
				t.Errorf("side effects %s: dry_run %q", se, tool.DryRun)
			}
			if info, err := os.Stat(tool.EntryPath()); err != nil || info.Mode()&0o111 == 0 {
				t.Errorf("entry not executable: %v", err)
			}
			if hasDash(readFile(t, tool.EntryPath())) || hasDash(readFile(t, filepath.Join(dir, "test.sh"))) {
				t.Error("scaffold contains a dash")
			}

			code, out, _ = run(t, "check", name)
			if got := statuses(parseCheck(t, out), name); code != 0 || got != "PASS PASS PASS PASS PASS PASS" {
				t.Fatalf("check: exit %d, statuses %q\n%s", code, got, out)
			}

			// A second scaffold of the same name refuses and leaves the first untouched: G2 sees
			// the existing tool, and without a registry hit the overwrite guard still holds.
			before := readFile(t, filepath.Join(dir, "tool.json"))
			code, _, errOut = run(t, "new", args...)
			if code != 1 || !strings.Contains(errOut, "G2 duplicate") {
				t.Errorf("same name through G2: exit %d, stderr %q", code, errOut)
			}
			useIndex(t, nil)
			code, _, errOut = run(t, "new", args...)
			if code != 1 || !strings.Contains(errOut, "refusing to overwrite") {
				t.Errorf("overwrite: exit %d, stderr %q", code, errOut)
			}
			if readFile(t, filepath.Join(dir, "tool.json")) != before {
				t.Error("refused scaffold changed tool.json")
			}
		})
	}
}

func TestNewGlobalGoPassesCheck(t *testing.T) {
	requireCommands(t, "shellcheck", "go")
	root := emptyRoot(t)
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/scaffold\n\ngo 1.27\n", 0o644)
	isolate(t, root)
	code, out, errOut := run(t, "new", "zz-go-sample", "--kind", "cli", "--scope", "global", "--task", "rice-kb:issues/04", "--side-effects", "write-local")
	if code != 0 {
		t.Fatalf("new: exit %d\n%s%s", code, out, errOut)
	}
	tool, err := manifest.Load(filepath.Join(root, "tools", "zz-go-sample"))
	if err != nil {
		t.Fatal(err)
	}
	if tool.Language != "go" || tool.Test != "go test ." || tool.DryRun != "--dry-run" {
		t.Errorf("manifest %+v", tool)
	}
	code, out, _ = run(t, "check", "zz-go-sample")
	if got := statuses(parseCheck(t, out), "zz-go-sample"); code != 0 || got != "PASS PASS PASS PASS PASS PASS" {
		t.Fatalf("check: exit %d, statuses %q\n%s", code, got, out)
	}

	// Without the agent-dry module there is nothing to build a Go tool in.
	bare := emptyRoot(t)
	isolate(t, bare)
	if code, _, _ := run(t, "new", "zz-go-bare", "--kind", "cli", "--scope", "global", "--task", "rice-kb:issues/04"); code != 3 {
		t.Errorf("no go.mod: exit %d, want 3", code)
	}
}

func TestNewDuplicateGate(t *testing.T) {
	root := emptyRoot(t)
	isolate(t, root)
	base := []string{"zz-dup", "--kind", "script", "--scope", "global", "--task", "rice-kb:issues/04"}

	useIndex(t, nil, hit("pr-flow", "script"), hit("ci-family", "wanted"))
	code, out, errOut := run(t, "new", base...)
	if code != 1 || !strings.Contains(out, "pr-flow") || strings.Contains(out, "ci-family") || !strings.Contains(errOut, "G2 duplicate") {
		t.Fatalf("match: exit %d\n%s%s", code, out, errOut)
	}
	if exists(filepath.Join(root, "tools", "zz-dup")) {
		t.Fatal("refused scaffold wrote files")
	}

	// At authoring time an installed skill is a reuse candidate too.
	installed := hit("home-skill", "skill")
	installed.Source = "skill"
	useIndex(t, nil, installed)
	if code, out, _ := run(t, "new", base...); code != 1 || !strings.Contains(out, "home-skill") {
		t.Errorf("installed skill: exit %d\n%s", code, out)
	}
	useIndex(t, nil, hit("pr-flow", "script"), hit("ci-family", "wanted"))

	// Naming some other tool does not clear the match.
	if code, _, _ := run(t, "new", append(base, "--extends", "other-tool")...); code != 1 {
		t.Errorf("unrelated --extends: exit %d, want 1", code)
	}

	if code, out, errOut := run(t, "new", append(base, "--extends", "pr-flow")...); code != 0 {
		t.Fatalf("--extends: exit %d\n%s%s", code, out, errOut)
	}
	tool, err := manifest.Load(filepath.Join(root, "tools", "zz-dup"))
	if err != nil || tool.Extends != "pr-flow" {
		t.Fatalf("extends not recorded: %v %+v", err, tool)
	}

	reason := "watches one job, pr-flow drives the whole PR"
	code, out, errOut = run(t, "new", "zz-dup-two", "--kind", "script", "--scope", "global", "--task", "rice-kb:issues/04",
		"--distinct-from", "pr-flow", "--reason", reason)
	if code != 0 {
		t.Fatalf("--distinct-from: exit %d\n%s%s", code, out, errOut)
	}
	tool, err = manifest.Load(filepath.Join(root, "tools", "zz-dup-two"))
	if err != nil || len(tool.DistinctFrom) != 1 || tool.DistinctFrom[0].Tool != "pr-flow" || tool.DistinctFrom[0].Reason != reason {
		t.Fatalf("distinct_from not recorded: %v %+v", err, tool)
	}
}

func TestNewPathCollision(t *testing.T) {
	root := emptyRoot(t)
	isolate(t, root)
	useIndex(t, nil)
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "zz-taken"), "#!/bin/sh\nexit 0\n", 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	code, _, errOut := run(t, "new", "zz-taken", "--kind", "script", "--scope", "global", "--task", "rice-kb:issues/04")
	if code != 1 || !strings.Contains(errOut, "PATH") || exists(filepath.Join(root, "tools", "zz-taken")) {
		t.Errorf("collision: exit %d, stderr %q", code, errOut)
	}
}

func TestNewProjectScope(t *testing.T) {
	requireCommands(t, "git", "shellcheck")
	isolate(t, emptyRoot(t))
	useIndex(t, nil)
	repo := t.TempDir()
	gitInit(t, repo)
	writeFile(t, filepath.Join(repo, ProjectList), `[{"name":"existing","summary":"Use when testing merges.","intents":["a b c"],"entry":"scripts/existing","owner":"kept"}]`, 0o644)
	sub := filepath.Join(repo, "pkg", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)

	args := []string{"zz-proj", "--kind", "script", "--scope", "project", "--task", "rice-kb:issues/04", "--side-effects", "write-local",
		"--intent", "project sample", "--intent", "sample project script", "--intent", "zz project"}
	code, out, errOut := run(t, "new", args...)
	if code != 0 {
		t.Fatalf("new: exit %d\n%s%s", code, out, errOut)
	}
	entry := filepath.Join(repo, "scripts", "zz-proj")
	testFile := filepath.Join(repo, "scripts", "zz-proj.test.sh")
	if info, err := os.Stat(entry); err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("entry: %v", err)
	}
	if info, err := os.Stat(testFile); err != nil || info.Mode()&0o111 != 0 {
		t.Fatalf("test file must exist and not be executable (find lists executable scripts): %v", err)
	}
	cmd := exec.Command("bash", "scripts/zz-proj.test.sh")
	cmd.Dir, cmd.Env = repo, childEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("project test: %v\n%s", err, out)
	}
	sc := exec.Command("shellcheck", "scripts/zz-proj", "scripts/zz-proj.test.sh")
	sc.Dir = repo
	if out, err := sc.CombinedOutput(); err != nil {
		t.Fatalf("shellcheck: %v\n%s", err, out)
	}

	var list []map[string]any
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(repo, ProjectList))), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0]["owner"] != "kept" || list[1]["name"] != "zz-proj" || list[1]["entry"] != "scripts/zz-proj" || list[1]["kind"] != "script" {
		t.Fatalf("%s: %v", ProjectList, list)
	}

	before := readFile(t, filepath.Join(repo, ProjectList))
	if code, _, errOut := run(t, "new", args...); code != 1 || !strings.Contains(errOut, "refusing to overwrite") {
		t.Errorf("overwrite: exit %d, stderr %q", code, errOut)
	}
	if code, _, errOut := run(t, "new", "existing", "--kind", "script", "--scope", "project", "--task", "rice-kb:issues/04"); code != 1 || !strings.Contains(errOut, "already listed") {
		t.Errorf("listed name: exit %d, stderr %q", code, errOut)
	}
	if readFile(t, filepath.Join(repo, ProjectList)) != before || exists(filepath.Join(repo, "scripts", "existing")) {
		t.Error("refused scaffold changed the project")
	}

	t.Chdir(t.TempDir())
	if code, _, _ := run(t, "new", "zz-nogit", "--kind", "script", "--scope", "project", "--task", "rice-kb:issues/04"); code != 3 {
		t.Errorf("outside Git: exit %d, want 3", code)
	}
}

func TestNewProjectGo(t *testing.T) {
	requireCommands(t, "git", "go")
	isolate(t, emptyRoot(t))
	useIndex(t, nil)
	repo := t.TempDir()
	gitInit(t, repo)
	t.Chdir(repo)
	if code, _, _ := run(t, "new", "zz-proj-go", "--kind", "cli", "--scope", "project", "--task", "rice-kb:issues/04"); code != 2 {
		t.Errorf("no go.mod: exit %d, want 2", code)
	}
	writeFile(t, filepath.Join(repo, "go.mod"), "module example.com/proj\n\ngo 1.27\n", 0o644)
	if code, out, errOut := run(t, "new", "zz-proj-go", "--kind", "cli", "--scope", "project", "--task", "rice-kb:issues/04"); code != 0 {
		t.Fatalf("new: exit %d\n%s%s", code, out, errOut)
	}
	for _, args := range [][]string{{"vet", "./scripts/zz-proj-go"}, {"test", "./scripts/zz-proj-go"}} {
		cmd := exec.Command("go", args...)
		cmd.Dir, cmd.Env = repo, childEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go %v: %v\n%s", args, err, out)
		}
	}
	if !strings.Contains(readFile(t, filepath.Join(repo, ProjectList)), `"go run ./scripts/zz-proj-go"`) {
		t.Errorf("entry not listed")
	}
}

func TestNewSkill(t *testing.T) {
	requireCommands(t, "git")
	root := emptyRoot(t)
	isolate(t, root)
	useIndex(t, nil)
	fixedNow(t)
	summary := `Use when a review needs judgment: "is this ready"?`
	if code, out, errOut := run(t, "new", "zz-judge", "--kind", "skill", "--scope", "global", "--task", "rice-kb:issues/15", "--summary", summary); code != 0 {
		t.Fatalf("new: exit %d\n%s%s", code, out, errOut)
	}
	got := readFile(t, filepath.Join(root, "skills", "zz-judge", "SKILL.md"))
	want := "---\nname: zz-judge\ndescription: \"Use when a review needs judgment: \\\"is this ready\\\"?\"\nmetadata:\n  status: trial\n  origin-date: \"2026-10-17\"\n  builder: \"test-model\"\n  task: \"rice-kb:issues/15\"\n---\n"
	if !strings.HasPrefix(got, want) {
		t.Errorf("frontmatter:\n%s", got)
	}

	repo := t.TempDir()
	gitInit(t, repo)
	t.Chdir(repo)
	if code, _, _ := run(t, "new", "zz-judge", "--kind", "skill", "--scope", "project", "--task", "rice-kb:issues/15"); code != 0 {
		t.Fatalf("project skill: exit %d", code)
	}
	if !exists(filepath.Join(repo, ".agents", "skills", "zz-judge", "SKILL.md")) || exists(filepath.Join(repo, ProjectList)) {
		t.Error("project skill not in .agents/skills, or listed as a command")
	}
}

func TestNewUsage(t *testing.T) {
	isolate(t, emptyRoot(t))
	useIndex(t, nil)
	ok := []string{"--kind", "script", "--scope", "global", "--task", "rice-kb:issues/04"}
	cases := map[string][]string{
		"no name":             ok,
		"two names":           append([]string{"zz-a", "zz-b"}, ok...),
		"bad name":            append([]string{"Bad_Name"}, ok...),
		"no kind":             {"zz-a", "--scope", "global", "--task", "rice-kb:issues/04"},
		"bad scope":           {"zz-a", "--kind", "script", "--scope", "team", "--task", "rice-kb:issues/04"},
		"no task":             {"zz-a", "--kind", "script", "--scope", "global"},
		"summary not trigger": append([]string{"zz-a", "--summary", "Counts lines in files"}, ok...),
		"summary too long":    append([]string{"zz-a", "--summary", "Use when " + strings.Repeat("x", 80)}, ok...),
		"summary dash":        append([]string{"zz-a", "--summary", "Use when lines " + emDash + " count"}, ok...),
		"short intent":        append([]string{"zz-a", "--intent", "ab"}, ok...),
		"skill language":      {"zz-a", "--kind", "skill", "--scope", "global", "--task", "rice-kb:issues/04", "--language", "go"},
		"bad language":        append([]string{"zz-a", "--language", "python"}, ok...),
		"bad side effects":    append([]string{"zz-a", "--side-effects", "network"}, ok...),
		"reason alone":        append([]string{"zz-a", "--reason", "a long enough reason"}, ok...),
		"distinct alone":      append([]string{"zz-a", "--distinct-from", "pr-flow"}, ok...),
		"short reason":        append([]string{"zz-a", "--distinct-from", "pr-flow", "--reason", "short"}, ok...),
		"extends itself":      append([]string{"zz-a", "--extends", "zz-a"}, ok...),
		"unknown flag":        append([]string{"zz-a", "--bogus"}, ok...),
		"too many intents":    append([]string{"zz-a"}, append(manyIntents(13), ok...)...),
	}
	for name, args := range cases {
		if code, _, _ := run(t, "new", args...); code != 2 {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
	}
	code, out, _ := run(t, "new", "--help")
	if code != 0 || !strings.HasPrefix(out, "Usage: agent-dry new") {
		t.Errorf("--help: exit %d\n%s", code, out)
	}
}

func manyIntents(n int) []string {
	var out []string
	for i := 0; i < n; i++ {
		out = append(out, "--intent", "intent number "+string(rune('a'+i)))
	}
	return out
}

// Every template renders for both dry-run settings without a dash glyph.
func TestTemplatesRender(t *testing.T) {
	for _, dry := range []bool{false, true} {
		d := tmplData{Name: "zz-render", Summary: "Use when rendering.", TestCmd: "bash test.sh", Date: "2026-10-17", Builder: "b", Task: "task", DryRun: dry}
		if dry {
			d.DryRunFlag, d.DryRunName = "--dry-run", "dry-run"
		}
		d.GoUsage = `"usage"`
		for _, name := range []string{"bash-entry.tmpl", "bash-test.tmpl", "go-launcher.tmpl", "go-main.tmpl", "go-main-test.tmpl", "skill.tmpl", "usage"} {
			out := string(render(name, d))
			if hasDash(out) || strings.Contains(out, "<no value>") {
				t.Errorf("%s (dry run %v) renders a dash or a missing field", name, dry)
			}
			if dry != strings.Contains(out, "--dry-run") && (name == "bash-entry.tmpl" || name == "usage") {
				t.Errorf("%s (dry run %v): --dry-run presence wrong", name, dry)
			}
		}
	}
}
