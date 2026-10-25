package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixtureDir(t *testing.T) string {
	t.Helper()
	p := t.TempDir()
	return p
}
func putFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
func testConfig(root string) config {
	return config{Root: root, ActiveLedgerDays: 7, ScratchTTL: "72h", EvidenceGrace: "336h", ProjectBudget: 2 * gib, WarnTotal: 8 * gib, FailTotal: 15 * gib}
}

func TestHelp(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"--help"}, &out, &errOut); code != 0 || !strings.HasPrefix(out.String(), "Usage: agent-state ") || !strings.Contains(out.String(), "--apply --plan") {
		t.Fatalf("help: code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
}

func TestParseOptionsAllowsFlagsAfterPositionals(t *testing.T) {
	o, err := parseOptions([]string{"bundle", "--apply", "--plan", "plan-1"}, []string{"apply"}, []string{"plan"})
	if err != nil || len(o.pos) != 1 || o.pos[0] != "bundle" || !o.bools["apply"] || o.values["plan"] != "plan-1" {
		t.Fatalf("parsed %+v, %v", o, err)
	}
}

func TestParseDurationSupportsDaysAndRejectsInvalidValues(t *testing.T) {
	if got := parseDuration("3d"); got != 72*time.Hour {
		t.Fatalf("3d parsed as %s", got)
	}
	for _, value := range []string{"0s", "-1h", "3.5d", "999999999999999999999d", "invalid"} {
		if got := parseDuration(value); got != 0 {
			t.Errorf("%q parsed as %s", value, got)
		}
	}
}

func TestInventoryCountsClassesAndIgnoresRegistry(t *testing.T) {
	root := fixtureDir(t)
	putFile(t, filepath.Join(root, "alpha", "evidence", "b", "x"), strings.Repeat("e", 1024))
	putFile(t, filepath.Join(root, "alpha", "scratch", "l", "x"), strings.Repeat("s", 512))
	putFile(t, filepath.Join(root, "alpha", "goal.json"), strings.Repeat("g", 256))
	putFile(t, filepath.Join(root, "_registry", "plans", "p.json"), strings.Repeat("r", 100))
	data, err := inventory(testConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	if data.TotalBytes != 1792 {
		t.Fatalf("total=%d", data.TotalBytes)
	}
	if data.ClassBytes["evidence"] != 1024 || data.ClassBytes["scratch"] != 512 || data.ClassBytes["ledger"] != 256 {
		t.Fatalf("classes=%+v", data.ClassBytes)
	}
	if data.ClassFiles["evidence"] != 1 || data.ClassFiles["scratch"] != 1 || data.ClassFiles["ledger"] != 1 {
		t.Fatalf("class files=%+v", data.ClassFiles)
	}
}

func TestLintFindsForbiddenDirectoriesAndSourceCopies(t *testing.T) {
	root := fixtureDir(t)
	putFile(t, filepath.Join(root, "bundle", "target", "debug", "app"), "output")
	putFile(t, filepath.Join(root, "bundle", "runtime", "source", "package.json"), "{}")
	putFile(t, filepath.Join(root, "bundle", "nested", ".git", "config"), "[core]")
	findings, err := lintEvidence(root)
	if err != nil {
		t.Fatal(err)
	}
	rules := map[string]bool{}
	for _, f := range findings {
		rules[f.Rule] = true
	}
	for _, rule := range []string{"forbidden_directory", "source_copy", "nested_git"} {
		if !rules[rule] {
			t.Errorf("missing finding %s: %+v", rule, findings)
		}
	}
}

func TestSafetyChecksBlockPinsOpenFilesActiveLedgersAndDirtyGit(t *testing.T) {
	root := fixtureDir(t)
	candidate := filepath.Join(root, "alpha", "scratch", "lease-a")
	putFile(t, filepath.Join(candidate, "data"), "data")
	ledger := filepath.Join(root, "alpha", "goal.json")
	now := time.Now().UTC().Format(time.RFC3339)
	b, _ := json.Marshal(map[string]any{"updated_at": now, "evidence": candidate})
	putFile(t, ledger, string(b))
	savedOpen, savedRepo := safetyOpenPaths, safetyRepoState
	t.Cleanup(func() { safetyOpenPaths, safetyRepoState = savedOpen, savedRepo })
	safetyOpenPaths = func() ([]string, error) { return []string{filepath.Join(candidate, "data")}, nil }
	safetyRepoState = func(string) ([]repoStateRecord, error) {
		return []repoStateRecord{{Staged: 1, Upstream: nil, Ahead: nil}}, nil
	}
	idx := registryIndex{Schema: indexSchema, Pins: []pin{{Path: candidate, Reason: "test"}}}
	reasons := safetyReasons(testConfig(root), idx, candidate, []string{filepath.Join(candidate, "data")}, nil)
	for _, want := range []string{"pinned_path", "open_file_or_process_cwd", "active_ledger_reference"} {
		if !contains(reasons, want) {
			t.Errorf("missing %s in %v", want, reasons)
		}
	}
	putFile(t, filepath.Join(candidate, ".git", "config"), "[core]")
	reasons = repoSafety(candidate)
	if !contains(reasons, "git_dirty") || !contains(reasons, "git_unique_or_unpushed_unknown") {
		t.Errorf("git safety reasons: %v", reasons)
	}
}

func gitFixtureCommand(t *testing.T, root string, args ...string) {
	t.Helper()
	base := []string{"-C", root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "-c", "core.hooksPath=/dev/null"}
	cmd := exec.Command("git", append(base, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestGitHistorySafetyFindsStashesAndUnpushedBranches(t *testing.T) {
	root := fixtureDir(t)
	gitFixtureCommand(t, root, "init", "-q")
	putFile(t, filepath.Join(root, "data"), "first")
	gitFixtureCommand(t, root, "add", "data")
	gitFixtureCommand(t, root, "commit", "-q", "-m", "fixture")
	putFile(t, filepath.Join(root, "data"), "changed")
	gitFixtureCommand(t, root, "stash", "push", "-m", "fixture")
	reasons, err := gitHistoryReasons(root)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(reasons, "git_stashes_present") || !contains(reasons, "git_unique_commits") {
		t.Fatalf("git history safety reasons=%v", reasons)
	}
}

func TestActiveLedgerScanFailsClosedOnSymlinkedLedger(t *testing.T) {
	root := fixtureDir(t)
	external := filepath.Join(fixtureDir(t), "active-ledger.json")
	putFile(t, external, `{"updated_at":"`+time.Now().UTC().Format(time.RFC3339)+`"}`)
	link := filepath.Join(root, "alpha", "goal.json")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, link); err != nil {
		t.Fatal(err)
	}
	if _, err := collectActiveLedgerRefs(testConfig(root)); err == nil {
		t.Fatal("symlinked ledger did not fail closed")
	}
}

func TestActiveLedgerScanFailsClosedOnMalformedLedger(t *testing.T) {
	root := fixtureDir(t)
	putFile(t, filepath.Join(root, "alpha", "goal.json"), "not-json")
	if _, err := collectActiveLedgerRefs(testConfig(root)); err == nil {
		t.Fatal("malformed ledger did not fail closed")
	}
}

func TestMeasureTreeRejectsSymlinkEntries(t *testing.T) {
	root := fixtureDir(t)
	candidate := filepath.Join(root, "alpha", "evidence", "bundle")
	putFile(t, filepath.Join(root, "outside"), "outside")
	if err := os.MkdirAll(candidate, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "outside"), filepath.Join(candidate, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := measureTreeWithin(candidate, root); err == nil {
		t.Fatal("symlink entry passed candidate measurement")
	}
}

func TestActiveLedgerWindowDoesNotBlockStaleReference(t *testing.T) {
	root := fixtureDir(t)
	candidate := filepath.Join(root, "alpha", "evidence", "bundle-a")
	putFile(t, filepath.Join(candidate, "file"), "x")
	old := time.Now().Add(-14 * 24 * time.Hour).UTC().Format(time.RFC3339)
	putFile(t, filepath.Join(root, "alpha", "goal.json"), `{"updated_at":"`+old+`","evidence":"`+candidate+`"}`)
	if activeLedgerReferences(testConfig(root), candidate) {
		t.Fatal("stale ledger reference was treated as active")
	}
}

func TestGCApplyRequiresReviewedPlanAndLeavesFilesUntouched(t *testing.T) {
	root := fixtureDir(t)
	path := filepath.Join(root, "alpha", "scratch", "lease-x")
	putFile(t, filepath.Join(path, "data"), "keep")
	cfg := testConfig(root)
	var out, errOut bytes.Buffer
	if code := gcCommand(cfg, []string{"--apply"}, &out, &errOut); code != exitUsage {
		t.Fatalf("apply without plan code=%d err=%s", code, errOut.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("candidate changed without a plan")
	}
	if code := gcCommand(cfg, []string{"--apply", "--plan", "plan-missing"}, &out, &errOut); code != exitBlocked {
		t.Fatalf("unknown plan code=%d err=%s", code, errOut.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("candidate changed with an unknown plan")
	}
}

func TestGCPlanOnlyListsExpiredManagedScratchAndWritesPlan(t *testing.T) {
	root := fixtureDir(t)
	cfg := testConfig(root)
	path := filepath.Join(root, "alpha", "scratch", "lease-expired")
	putFile(t, filepath.Join(path, "data"), "scratch")
	now := time.Now().UTC()
	idx := registryIndex{Schema: indexSchema, Leases: []lease{{ID: "lease-expired", Project: "alpha", Path: path, CreatedAt: now.Add(-5 * 24 * time.Hour).Format(time.RFC3339), RenewedAt: now.Add(-5 * 24 * time.Hour).Format(time.RFC3339), ExpiresAt: now.Add(-2 * 24 * time.Hour).Format(time.RFC3339)}}}
	if err := saveIndex(cfg, idx); err != nil {
		t.Fatal(err)
	}
	savedOpen := safetyOpenPaths
	t.Cleanup(func() { safetyOpenPaths = savedOpen })
	safetyOpenPaths = func() ([]string, error) { return nil, nil }
	plan, err := buildPlan(cfg, "scratch")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 1 || plan.Candidates[0].Path != path {
		t.Fatalf("plan candidates=%+v", plan.Candidates)
	}
	if err := persistPlan(plan, plansPath(cfg)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(plansPath(cfg), plan.ID+".json")); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPlan(cfg, plan.ID, plansPath(cfg)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("dry run changed scratch")
	}
}

func TestGCPlanIsBoundAndCanBeStoredOutsideStateRoot(t *testing.T) {
	base := fixtureDir(t)
	root := filepath.Join(base, "state")
	planDir := filepath.Join(base, "plans")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(root)
	path := filepath.Join(root, "alpha", "scratch", "lease-expired")
	putFile(t, filepath.Join(path, "data"), "scratch")
	now := time.Now().UTC()
	idx := registryIndex{Schema: indexSchema, Leases: []lease{{ID: "lease-expired", Project: "alpha", Path: path, CreatedAt: now.Add(-5 * 24 * time.Hour).Format(time.RFC3339), RenewedAt: now.Add(-5 * 24 * time.Hour).Format(time.RFC3339), ExpiresAt: now.Add(-2 * 24 * time.Hour).Format(time.RFC3339)}}}
	if err := saveIndex(cfg, idx); err != nil {
		t.Fatal(err)
	}
	savedOpen := safetyOpenPaths
	t.Cleanup(func() { safetyOpenPaths = savedOpen })
	safetyOpenPaths = func() ([]string, error) { return nil, nil }
	plan, err := buildPlan(cfg, "scratch")
	if err != nil {
		t.Fatal(err)
	}
	if err := persistPlan(plan, planDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(plansPath(cfg), plan.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("external plan was also written inside state root")
	}
	loaded, err := loadPlan(cfg, plan.ID, planDir)
	if err != nil {
		t.Fatal(err)
	}
	loaded.Candidates[0].Bytes++
	if err := validatePlan(loaded); err == nil {
		t.Fatal("tampered plan passed digest validation")
	}
}

func TestTreeFingerprintDetectsSameSizeScratchChanges(t *testing.T) {
	root := fixtureDir(t)
	file := filepath.Join(root, "scratch", "data")
	putFile(t, file, "pass")
	before, err := treeFingerprint(filepath.Join(root, "scratch"))
	if err != nil {
		t.Fatal(err)
	}
	putFile(t, file, "fail")
	after, err := treeFingerprint(filepath.Join(root, "scratch"))
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("same-size scratch change did not alter the plan fingerprint")
	}
	emptyDir := filepath.Join(root, "scratch", "empty")
	if err := os.Mkdir(emptyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	withEmptyDir, err := treeFingerprint(filepath.Join(root, "scratch"))
	if err != nil {
		t.Fatal(err)
	}
	if after == withEmptyDir {
		t.Fatal("added empty directory did not alter the plan fingerprint")
	}
}

func TestArchiveEvidenceReasonsFailClosed(t *testing.T) {
	lintFailure := errors.New("lint unavailable")
	reasons := archiveEvidenceReasons("bundle", func(string) ([]lintFinding, error) {
		return []lintFinding{{Rule: "forbidden_directory"}}, nil
	}, func(string) (bool, error) { return true, nil })
	for _, want := range []string{"evidence_lint_forbidden_directory", "secret_finding_redacted"} {
		if !contains(reasons, want) {
			t.Errorf("missing %s in %v", want, reasons)
		}
	}
	reasons = archiveEvidenceReasons("bundle", func(string) ([]lintFinding, error) {
		return nil, lintFailure
	}, func(string) (bool, error) { return false, lintFailure })
	for _, want := range []string{"evidence_lint_unavailable", "secret_scan_unavailable"} {
		if !contains(reasons, want) {
			t.Errorf("missing fail-closed reason %s in %v", want, reasons)
		}
	}
}

func TestGCPlanBlocksEvidenceLintAndSecretScanReasons(t *testing.T) {
	root := fixtureDir(t)
	cfg := testConfig(root)
	bundle := filepath.Join(root, "alpha", "evidence", "evidence-a")
	now := time.Now().UTC()
	manifest := evidenceManifest{
		Schema: evidenceSchema, ID: "evidence-a", Project: "alpha", Status: "closed",
		CreatedAt:  now.Add(-30 * 24 * time.Hour).Format(time.RFC3339),
		LastUsedAt: now.Add(-30 * 24 * time.Hour).Format(time.RFC3339),
		ClosedAt:   now.Add(-20 * 24 * time.Hour).Format(time.RFC3339),
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	putFile(t, filepath.Join(bundle, "manifest.json"), string(raw))
	savedOpen, savedEvidenceReasons := safetyOpenPaths, planEvidenceReasons
	t.Cleanup(func() { safetyOpenPaths, planEvidenceReasons = savedOpen, savedEvidenceReasons })
	safetyOpenPaths = func() ([]string, error) { return nil, nil }
	planEvidenceReasons = func(string) []string {
		return []string{"evidence_lint_forbidden_directory", "secret_finding_redacted"}
	}
	plan, err := buildPlan(cfg, "evidence")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range plan.Blocked {
		if entry.Path == bundle && contains(entry.Reasons, "evidence_lint_forbidden_directory") && contains(entry.Reasons, "secret_finding_redacted") {
			return
		}
	}
	t.Fatalf("evidence lint or secret scan reasons did not block bundle: %+v", plan)
}

func TestGCRejectsExternalPlanDirectorySymlinkIntoStateRoot(t *testing.T) {
	base := fixtureDir(t)
	root := filepath.Join(base, "state")
	insideDir := filepath.Join(root, "plans")
	link := filepath.Join(base, "external-plans")
	if err := os.MkdirAll(insideDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(insideDir, link); err != nil {
		t.Fatal(err)
	}
	if _, err := resolvePlanDirectory(testConfig(root), link); err == nil {
		t.Fatal("external plan directory symlink into state root was accepted")
	}
}

func TestLoadIndexRejectsLeaseOutsideItsScratchPath(t *testing.T) {
	root := fixtureDir(t)
	cfg := testConfig(root)
	ledger := filepath.Join(root, "alpha", "goal.json")
	putFile(t, ledger, "{}")
	idx := registryIndex{Schema: indexSchema, Leases: []lease{{ID: "lease-a", Project: "alpha", Path: ledger}}}
	if err := saveIndex(cfg, idx); err != nil {
		t.Fatal(err)
	}
	if _, err := loadIndex(cfg); err == nil {
		t.Fatal("registry lease targeting a ledger was accepted")
	}
}

func TestLoadIndexRejectsDuplicateScratchLeaseIDs(t *testing.T) {
	root := fixtureDir(t)
	cfg := testConfig(root)
	path := filepath.Join(root, "alpha", "scratch", "lease-a")
	idx := registryIndex{Schema: indexSchema, Leases: []lease{
		{ID: "lease-a", Project: "alpha", Path: path},
		{ID: "lease-a", Project: "alpha", Path: path},
	}}
	if err := saveIndex(cfg, idx); err != nil {
		t.Fatal(err)
	}
	if _, err := loadIndex(cfg); err == nil {
		t.Fatal("duplicate scratch lease IDs were accepted")
	}
}

func TestArchivePlanRejectsChangedManifestWithSameSize(t *testing.T) {
	bundle := fixtureDir(t)
	manifest := filepath.Join(bundle, "manifest.json")
	putFile(t, manifest, `{"result":"pass"}`)
	sum, size, err := hashFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	planned := planEntry{ManifestSHA256: sum, Bytes: size}
	if !plannedManifestMatches(bundle, planned) {
		t.Fatal("unchanged manifest failed its plan check")
	}
	putFile(t, manifest, `{"result":"fail"}`)
	updatedInfo, err := os.Stat(manifest)
	if err != nil || updatedInfo.Size() != size {
		t.Fatalf("changed fixture size: info=%v err=%v", updatedInfo, err)
	}
	if plannedManifestMatches(bundle, planned) {
		t.Fatal("same-size changed manifest passed its plan check")
	}
}

func TestArchiveRequiresConfiguredResticRepository(t *testing.T) {
	cfg := testConfig(fixtureDir(t))
	var out, errOut bytes.Buffer
	code := archiveCommand(cfg, []string{"bundle", "--apply", "--plan", "plan-a"}, &out, &errOut)
	if code != exitEnv || !strings.Contains(errOut.String(), "archive repository is not configured") {
		t.Fatalf("archive code=%d err=%q", code, errOut.String())
	}
	if code = verifyCommand(cfg, []string{"bundle"}, &out, &errOut); code != exitEnv {
		t.Fatalf("verify without repo code=%d", code)
	}
	if code = restoreCommand(cfg, []string{"bundle"}, &out, &errOut); code != exitEnv {
		t.Fatalf("restore without repo code=%d", code)
	}
}

func TestEvidenceRetentionCandidatesAreExactAndTagged(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	snapshots := []resticSnapshot{
		{ID: "aaaaaaaa", Time: "2025-10-24T12:00:00Z", Tags: []string{"class=evidence"}},
		{ID: "bbbbbbbb", Time: "2025-10-25T12:00:00Z", Tags: []string{"class=evidence"}},
		{ID: "cccccccc", Time: "2020-01-01T00:00:00Z", Tags: []string{"class=ledger"}},
	}
	eligible, err := eligibleRetentionSnapshots(snapshots, 12, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(eligible) != 1 || !eligible["aaaaaaaa"] {
		t.Fatalf("eligible snapshots=%v", eligible)
	}
}

func TestSnapshotParserUsesFinalSnapshotID(t *testing.T) {
	out := []byte("{\"message_type\":\"status\"}\n{\"message_type\":\"summary\",\"snapshot_id\":\"abcdef12\"}\n")
	id, err := snapshotIDFromOutput(out)
	if err != nil || id != "abcdef12" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	if _, err := snapshotIDFromOutput([]byte("{\"message_type\":\"summary\",\"snapshot_id\":\"--help\"}\n")); err == nil {
		t.Fatal("invalid restic snapshot id was accepted")
	}
}

func TestLedgerPath(t *testing.T) {
	cfg := testConfig(filepath.Join(string(filepath.Separator), "state"))
	var out, errOut bytes.Buffer
	if code := ledgerCommand(cfg, []string{"path", "alpha", "goal-1"}, &out, &errOut); code != 0 || strings.TrimSpace(out.String()) != filepath.Join(cfg.Root, "alpha", "goal-1.json") {
		t.Fatalf("ledger path code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	if code := ledgerCommand(cfg, []string{"path", "../escape", "goal"}, &out, &errOut); code != exitUsage {
		t.Fatalf("traversal code=%d", code)
	}
}

func TestSaveIndexRejectsCorruptIndex(t *testing.T) {
	cfg := testConfig(fixtureDir(t))
	putFile(t, indexPath(cfg), "not-json")
	if _, err := loadIndex(cfg); err == nil {
		t.Fatal("corrupt registry accepted")
	}
}

func TestUnknownOptionIsUsageError(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := reportCommand(testConfig(fixtureDir(t)), []string{"--typo"}, &out, &errOut); code != exitUsage {
		t.Fatalf("code=%d", code)
	}
}

func contains(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}
