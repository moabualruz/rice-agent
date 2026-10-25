package dist

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

type installFixture struct {
	root, home, prefix, state string
	vars                      map[string]string
}

func newInstallFixture(t *testing.T) *installFixture {
	t.Helper()
	f := &installFixture{root: fixtureRoot(t), home: tempDir(t)}
	f.prefix = filepath.Join(f.home, ".local", "bin")
	f.state = filepath.Join(f.home, "state")
	writeFile(t, filepath.Join(f.root, "bin", "agent-dry"), "#!/usr/bin/env bash\necho fixture agent-dry\n", 0o755)
	f.vars = map[string]string{
		"HOME":             f.home,
		"AGENT_DRY_ROOT":   f.root,
		"AGENT_DRY_STATE":  f.state,
		"AGENT_DRY_CONFIG": filepath.Join(f.home, "config.json"),
		"PATH":             f.prefix + ":/usr/bin:/bin",
	}
	return f
}

func decodeReceipt(t *testing.T, raw string) Receipt {
	t.Helper()
	var r Receipt
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatalf("receipt %q: %v", raw, err)
	}
	return r
}

func linkNames(links []Link) []string {
	var n []string
	for _, l := range links {
		n = append(n, l.Name)
	}
	sort.Strings(n)
	return n
}

func wantNames() []string {
	n := []string{"agent-dry", "dfx-active", "dfx-hello"}
	if platform() == "linux-arm64" {
		n = append(n, "dfx-armonly")
	}
	sort.Strings(n)
	return n
}

func TestInstallGoToolDryRunTargetsFutureBinaryWithoutBuilding(t *testing.T) {
	f := newInstallFixture(t)
	addGoToolFixture(t, f.root, "dfx-go-dry")
	code, out, errb := run(t, f.vars, "install", "--dry-run")
	if code != 0 {
		t.Fatalf("dry run: %d %s", code, errb)
	}
	receipt := decodeReceipt(t, out)
	var target string
	for _, link := range receipt.Links {
		if link.Name == "dfx-go-dry" {
			target = link.Target
		}
	}
	want := filepath.Join(f.state, "go-tools", "dfx-go-dry", platform(), "dfx-go-dry")
	if target != want {
		t.Fatalf("Go tool target=%q want %q", target, want)
	}
	if exists(f.state) || exists(f.prefix) {
		t.Fatal("dry run created the Go binary or install links")
	}
}

func TestBuildGoToolsPrefersVerifiedPackagedBinaryWhenGoIsAvailable(t *testing.T) {
	f := newInstallFixture(t)
	name := "dfx-go-packaged"
	addGoToolFixture(t, f.root, name)
	packaged := filepath.Join(f.root, "tools", name, "bin", platform(), name)
	writeFile(t, packaged, "#!/bin/sh\necho packaged binary\n", 0o755)
	fakeGo := filepath.Join(f.home, "go", "go")
	writeFile(t, fakeGo, "#!/bin/sh\nexit 99\n", 0o755)

	tools, err := buildGoTools(f.root, filepath.Join(f.state, "go-tools"), runtime.GOOS, runtime.GOARCH, filepath.Dir(fakeGo))
	if err != nil {
		t.Fatalf("build Go tools: %v", err)
	}
	defer discardGoTools(tools)
	if len(tools) != 1 || tools[0].Name != name || tools[0].Target != filepath.Join(f.state, "go-tools", name, platform(), name) {
		t.Fatalf("Go tool result: %+v", tools)
	}
	if got := readFile(t, tools[0].Path); got != readFile(t, packaged) {
		t.Fatalf("staged binary %q differs from packaged binary", got)
	}
}

func TestInstallLinksReceiptAndUninstall(t *testing.T) {
	f := newInstallFixture(t)

	code, out, errb := run(t, f.vars, "install", "--dry-run")
	if code != 0 {
		t.Fatalf("dry run: %d %s", code, errb)
	}
	plan := decodeReceipt(t, out)
	if !plan.DryRun || strings.Join(linkNames(plan.Links), ",") != strings.Join(wantNames(), ",") {
		t.Fatalf("dry run plan: %+v", plan)
	}
	if exists(f.prefix) || exists(filepath.Join(f.state, ReceiptFile)) {
		t.Fatal("dry run changed the filesystem")
	}

	code, out, errb = run(t, f.vars, "install")
	if code != 0 {
		t.Fatalf("install: %d %s", code, errb)
	}
	r := decodeReceipt(t, out)
	if strings.Join(linkNames(r.Links), ",") != strings.Join(wantNames(), ",") {
		t.Fatalf("links: %v", linkNames(r.Links))
	}
	for _, l := range r.Links {
		if l.Action != "created" {
			t.Errorf("%s: action %s", l.Name, l.Action)
		}
		dest, err := os.Readlink(l.Path)
		if err != nil || dest != l.Target || !within(f.root, dest) {
			t.Errorf("%s: link %q -> %q (%v)", l.Name, l.Path, dest, err)
		}
		sum, _ := sha256File(l.Target)
		if l.SHA256 != sum {
			t.Errorf("%s: sha256 %s, want %s", l.Name, l.SHA256, sum)
		}
	}
	if got := readFile(t, filepath.Join(f.state, ReceiptFile)); !strings.Contains(got, `"schema": "install-receipt/v1"`) {
		t.Fatalf("stored receipt: %s", got)
	}
	stored, err := loadReceipt(filepath.Join(f.state, ReceiptFile))
	if err != nil || len(stored.Links) != len(r.Links) || stored.Root != f.root || stored.Prefix != f.prefix {
		t.Fatalf("stored receipt %+v %v", stored, err)
	}
	if outb, err := exec.Command(filepath.Join(f.prefix, "dfx-hello")).Output(); err != nil || string(outb) != "hello from dfx-hello world\n" {
		t.Fatalf("linked tool: %q %v", outb, err)
	}
	for _, skipped := range []string{"dfx-project", "dfx-deprecated"} {
		if exists(filepath.Join(f.prefix, skipped)) {
			t.Errorf("%s was linked", skipped)
		}
	}

	code, out, _ = run(t, f.vars, "install")
	if code != 0 {
		t.Fatalf("second install: %d", code)
	}
	for _, l := range decodeReceipt(t, out).Links {
		if l.Action != "unchanged" {
			t.Errorf("second install %s: %s", l.Name, l.Action)
		}
	}

	// A link the user repointed outside the root is no longer ours; uninstall leaves it.
	other := filepath.Join(f.home, "elsewhere")
	writeFile(t, other, "#!/bin/sh\n", 0o755)
	if err := os.Remove(filepath.Join(f.prefix, "dfx-active")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(f.prefix, "dfx-active")); err != nil {
		t.Fatal(err)
	}
	code, out, errb = run(t, f.vars, "install", "--uninstall", "--dry-run")
	if code != 0 || !exists(filepath.Join(f.prefix, "dfx-hello")) {
		t.Fatalf("uninstall dry run: %d %s", code, errb)
	}
	code, out, errb = run(t, f.vars, "install", "--uninstall")
	if code != 0 {
		t.Fatalf("uninstall: %d %s", code, errb)
	}
	var res uninstallResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Name != "dfx-active" || len(res.Removed) != len(wantNames())-1 {
		t.Fatalf("uninstall result: %+v", res)
	}
	if exists(filepath.Join(f.prefix, "dfx-hello")) || exists(filepath.Join(f.prefix, "agent-dry")) {
		t.Fatal("uninstall left an owned link")
	}
	if dest, _ := os.Readlink(filepath.Join(f.prefix, "dfx-active")); dest != other {
		t.Fatalf("uninstall touched a foreign link: %q", dest)
	}
	if exists(filepath.Join(f.state, ReceiptFile)) {
		t.Fatal("receipt kept after a full uninstall")
	}
	if code, _, _ := run(t, f.vars, "install", "--uninstall"); code != 0 {
		t.Fatalf("uninstall without receipt: %d", code)
	}
}

func TestInstallCollisionDoesNotReplacePackagedGoTool(t *testing.T) {
	f := newInstallFixture(t)
	name := "dfx-go-collision"
	addGoToolFixture(t, f.root, name)
	packaged := filepath.Join(f.root, "tools", name, "bin", platform(), name)
	writeFile(t, packaged, "#!/bin/sh\necho packaged\n", 0o755)
	target := filepath.Join(f.state, "go-tools", name, platform(), name)
	writeFile(t, target, "previous stable binary\n", 0o755)
	writeFile(t, filepath.Join(f.prefix, "dfx-hello"), "user file\n", 0o755)

	code, _, errb := run(t, f.vars, "install")
	if code != 1 || !strings.Contains(errb, "collision") {
		t.Fatalf("install collision: code %d, stderr %s", code, errb)
	}
	if got := readFile(t, target); got != "previous stable binary\n" {
		t.Fatalf("stable Go binary changed before preflight completed: %q", got)
	}
	if exists(filepath.Join(f.prefix, "agent-dry")) || exists(filepath.Join(f.state, ReceiptFile)) {
		t.Fatal("install changed links or receipt despite collision")
	}
}

func TestInstallBuildsAndLinksGoTools(t *testing.T) {
	f := newInstallFixture(t)
	requireCommands(t, "go")
	writeFile(t, filepath.Join(f.root, "go.mod"), "module github.com/moabualruz/agent-dry\n\ngo 1.21\n", 0o644)
	addGoToolFixture(t, f.root, "dfx-go")
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not available")
	}
	f.vars["PATH"] = f.prefix + ":" + filepath.Dir(goBin) + ":/usr/bin:/bin"
	code, out, errb := run(t, f.vars, "install")
	if code != 0 {
		t.Fatalf("install: %d %s", code, errb)
	}
	receipt := decodeReceipt(t, out)
	for _, link := range receipt.Links {
		if link.Name != "dfx-go" {
			continue
		}
		if !strings.Contains(link.Target, filepath.Join("go-tools", "dfx-go", platform())) {
			t.Fatalf("Go tool linked to %s", link.Target)
		}
		if _, err := exec.Command(link.Path).CombinedOutput(); err != nil {
			t.Fatalf("Go tool executable: %v", err)
		}
		return
	}
	t.Fatal("Go tool is missing from install receipt")
}

func TestInstallRefusesCollisionsWithoutChanges(t *testing.T) {
	f := newInstallFixture(t)
	writeFile(t, filepath.Join(f.prefix, "dfx-hello"), "user file\n", 0o755)
	outside := filepath.Join(f.home, "outside")
	writeFile(t, outside, "#!/bin/sh\n", 0o755)
	if err := os.Symlink(outside, filepath.Join(f.prefix, "dfx-active")); err != nil {
		t.Fatal(err)
	}
	code, out, errb := run(t, f.vars, "install")
	if code != 1 || out != "" {
		t.Fatalf("collision: code %d out %q", code, out)
	}
	for _, want := range []string{filepath.Join(f.prefix, "dfx-hello") + ": exists and is not a symlink", filepath.Join(f.prefix, "dfx-active") + ": symlink to " + outside, "2 collision(s); nothing changed"} {
		if !strings.Contains(errb, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errb)
		}
	}
	if got := readFile(t, filepath.Join(f.prefix, "dfx-hello")); got != "user file\n" {
		t.Fatalf("collision target changed: %q", got)
	}
	if exists(filepath.Join(f.prefix, "agent-dry")) || exists(filepath.Join(f.state, ReceiptFile)) {
		t.Fatal("install changed something despite collisions")
	}

	// Another command of the same name elsewhere on PATH is a collision too.
	g := newInstallFixture(t)
	shadow := filepath.Join(g.home, "other-bin")
	writeFile(t, filepath.Join(shadow, "dfx-hello"), "#!/bin/sh\n", 0o755)
	g.vars["PATH"] = g.prefix + ":" + shadow + ":/usr/bin:/bin"
	code, _, errb = run(t, g.vars, "install", "--dry-run")
	if code != 1 || !strings.Contains(errb, "another dfx-hello is on PATH at "+filepath.Join(shadow, "dfx-hello")) {
		t.Fatalf("shadow: code %d err %s", code, errb)
	}
}

func TestInstallPrunesLinksTheRootNoLongerInstalls(t *testing.T) {
	f := newInstallFixture(t)
	if code, _, errb := run(t, f.vars, "install"); code != 0 {
		t.Fatalf("install: %s", errb)
	}
	manifestPath := filepath.Join(f.root, "tools", "dfx-hello", "tool.json")
	writeFile(t, manifestPath, strings.Replace(readFile(t, manifestPath), `"status": "trial"`, `"status": "deprecated"`, 1), 0o644)
	code, out, errb := run(t, f.vars, "install")
	if code != 0 {
		t.Fatalf("reinstall: %s", errb)
	}
	r := decodeReceipt(t, out)
	if len(r.Removed) != 1 || r.Removed[0].Name != "dfx-hello" || r.Removed[0].Action != "removed" {
		t.Fatalf("removed: %+v", r.Removed)
	}
	if exists(filepath.Join(f.prefix, "dfx-hello")) {
		t.Fatal("deprecated tool still linked")
	}
}

func TestInstallFallsBackToRunningExecutable(t *testing.T) {
	f := newInstallFixture(t)
	if err := os.Remove(filepath.Join(f.root, "bin", "agent-dry")); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(f.home, "built", "agent-dry")
	writeFile(t, exe, "#!/bin/sh\n", 0o755)
	saved := executable
	executable = func() (string, error) { return exe, nil }
	t.Cleanup(func() { executable = saved })
	code, out, errb := run(t, f.vars, "install")
	if code != 0 {
		t.Fatalf("install: %s", errb)
	}
	for _, l := range decodeReceipt(t, out).Links {
		if l.Name == "agent-dry" && l.Target != exe {
			t.Fatalf("agent-dry target %s, want %s", l.Target, exe)
		}
	}
	// The recorded target is ours even though it is outside the root: reinstall and uninstall own it.
	if code, out, _ := run(t, f.vars, "install"); code != 0 || strings.Contains(out, `"created"`) {
		t.Fatalf("reinstall: %d %s", code, out)
	}
	if code, _, _ := run(t, f.vars, "install", "--uninstall"); code != 0 || exists(filepath.Join(f.prefix, "agent-dry")) {
		t.Fatal("uninstall kept the executable link")
	}
}

func TestInstallEnvironmentErrors(t *testing.T) {
	f := newInstallFixture(t)
	f.vars["AGENT_DRY_ROOT"] = f.home
	if code, _, _ := run(t, f.vars, "install", "--dry-run"); code != 3 {
		t.Fatalf("missing root: code %d, want 3", code)
	}
	g := newInstallFixture(t)
	writeFile(t, filepath.Join(g.root, "tools", "dfx-broken", "tool.json"), "{}", 0o644)
	if code, _, errb := run(t, g.vars, "install", "--dry-run"); code != 3 || !strings.Contains(errb, "dfx-broken") {
		t.Fatalf("broken manifest: code %d %s", code, errb)
	}
	if code, _, _ := run(t, g.vars, "install", "extra"); code != 2 {
		t.Fatalf("extra argument: code %d", code)
	}
}

func TestDoctor(t *testing.T) {
	f := newInstallFixture(t)
	writeFile(t, f.vars["AGENT_DRY_CONFIG"], `{"root": "`+f.root+`", "hosts": {"pc": {"ssh": "u@h", "shell": "fish"}}}`, 0o644)

	code, out, _ := run(t, f.vars, "doctor")
	if code != 1 || !strings.Contains(out, "FAIL install: no receipt") {
		t.Fatalf("doctor before install: %d\n%s", code, out)
	}
	if code, _, errb := run(t, f.vars, "install"); code != 0 {
		t.Fatalf("install: %s", errb)
	}
	code, out, _ = run(t, f.vars, "doctor")
	if code != 0 || !strings.Contains(out, "doctor: healthy") || !strings.Contains(out, "PASS links:") || !strings.Contains(out, "WARN index") {
		t.Fatalf("doctor after install: %d\n%s", code, out)
	}

	idx := filepath.Join(f.state, IndexFile)
	writeFile(t, idx, "{}", 0o644)
	if code, out, _ := run(t, f.vars, "doctor"); code != 0 || !strings.Contains(out, "PASS index") {
		t.Fatalf("fresh index: %d\n%s", code, out)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(idx, old, old); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := run(t, f.vars, "doctor"); code != 0 || !strings.Contains(out, "WARN index: "+idx+" is older than") {
		t.Fatalf("stale index: %d\n%s", code, out)
	}
	if err := os.Chmod(idx, 0); err != nil {
		t.Fatal(err)
	}
	if os.Getuid() != 0 {
		if code, out, _ := run(t, f.vars, "doctor"); code != 1 || !strings.Contains(out, "FAIL index: unreadable cache") {
			t.Fatalf("unreadable index: %d\n%s", code, out)
		}
	}
	if err := os.Chmod(idx, 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, idx, "{}", 0o644)

	f.vars["PATH"] = "/usr/bin:/bin"
	if code, out, _ := run(t, f.vars, "doctor"); code != 1 || !strings.Contains(out, "FAIL path") {
		t.Fatalf("prefix off PATH: %d\n%s", code, out)
	}
	f.vars["PATH"] = f.prefix + ":/usr/bin:/bin"

	if err := os.Remove(filepath.Join(f.prefix, "dfx-hello")); err != nil {
		t.Fatal(err)
	}
	code, out, _ = run(t, f.vars, "doctor", "--json")
	var rep doctorReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if code != 1 || rep.Healthy || rep.Problems != 2 {
		t.Fatalf("missing link: %d %+v", code, rep)
	}

	writeFile(t, f.vars["AGENT_DRY_CONFIG"], "{not json", 0o644)
	if code, out, _ := run(t, f.vars, "doctor"); code != 1 || !strings.Contains(out, "FAIL config") {
		t.Fatalf("bad config: %d\n%s", code, out)
	}
}

func TestDoctorRequiredCommands(t *testing.T) {
	f := newInstallFixture(t)
	writeFile(t, f.vars["AGENT_DRY_CONFIG"], `{"root": "`+f.root+`"}`, 0o644)
	manifestPath := filepath.Join(f.root, "tools", "dfx-active", "tool.json")
	writeFile(t, manifestPath, strings.Replace(readFile(t, manifestPath), `"test": "bash test.sh"`, `"requires": ["dfx-no-such-command >= 1.0"], "test": "bash test.sh"`, 1), 0o644)
	if code, _, errb := run(t, f.vars, "install"); code != 0 {
		t.Fatalf("install: %s", errb)
	}
	code, out, _ := run(t, f.vars, "doctor")
	if code != 1 || !strings.Contains(out, "FAIL requires: dfx-no-such-command (needed by dfx-active)") {
		t.Fatalf("requires: %d\n%s", code, out)
	}
}
