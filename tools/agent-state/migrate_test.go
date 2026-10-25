package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func putMigrationLedger(t *testing.T, cfg config, goal string, refs []string, stamp string) {
	t.Helper()
	doc := map[string]any{"version": 1, "goal_id": goal, "attempts": []any{map[string]any{"evidence": refs}}, "turns": []any{map[string]any{"at": stamp}}}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	putFile(t, rootPath(cfg, "alpha", goal+".json"), string(raw))
}

func migrationFixture(t *testing.T, names ...string) (config, []string) {
	t.Helper()
	isolateOpenScan(t)
	root, err := canonicalPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(root)
	cfg.Archive.RetentionMonths = 12
	stamp := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	var bundles, refs []string
	for _, name := range names {
		bundle := rootPath(cfg, "alpha", "evidence", name)
		file := filepath.Join(bundle, "logs", "run.log")
		putFile(t, file, "legacy proof\n")
		if err := os.Chtimes(file, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		bundles = append(bundles, bundle)
		refs = append(refs, file)
	}
	putMigrationLedger(t, cfg, "legacy-goal", refs, "2020-01-03T00:00:00Z")
	return cfg, bundles
}

func reviewedMigrationPlan(t *testing.T, cfg config) (gcPlan, string, string) {
	t.Helper()
	directory, err := canonicalPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := migrateCommand(cfg, []string{"--plan-dir", directory}, &out, &errOut); code != exitOK {
		t.Fatalf("plan: code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	fields := strings.Fields(out.String())
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "plan_id=") {
		t.Fatalf("missing plan id: %q", out.String())
	}
	id := strings.TrimPrefix(fields[0], "plan_id=")
	plan, err := loadPlan(cfg, id, directory)
	if err != nil {
		t.Fatal(err)
	}
	return plan, directory, out.String()
}

func applyReviewedMigration(cfg config, plan gcPlan, directory string) int {
	return migrateCommand(cfg, []string{"--apply", "--plan", plan.ID, "--plan-dir", directory}, io.Discard, io.Discard)
}

func TestMigratePlanApplyAndIdempotence(t *testing.T) {
	cfg, bundles := migrationFixture(t, "bundle-a")
	before := snapshotStateTree(t, cfg.Root)
	plan, directory, _ := reviewedMigrationPlan(t, cfg)
	if !maps.Equal(before, snapshotStateTree(t, cfg.Root)) {
		t.Fatal("external dry run changed the state tree")
	}
	if plan.Action != "migrate" || len(plan.Candidates) != 1 || len(plan.Blocked) != 0 {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	entry := plan.Candidates[0]
	m := entry.Migration.Manifest
	if m.Schema != evidenceSchema || m.Kind != "evidence" || m.Project != "alpha" || m.GoalID != "legacy-goal" || m.ID != "bundle-a" || m.Status != "open" || m.ClosedAt != "" || m.Criterion != "" {
		t.Fatalf("unsafe inference: %+v", m)
	}
	if m.CreatedAt != "2020-01-01T00:00:00Z" || m.LastUsedAt != "2020-01-03T00:00:00Z" {
		t.Fatalf("timestamps: %+v", m)
	}
	if entry.Bytes != int64(len("legacy proof\n")) || len(m.Files) != 1 || m.Files[0].SHA256 != sha256Sum([]byte("legacy proof\n")) || m.Files[0].Bytes != entry.Bytes || m.Files[0].Role != "log" || m.Files[0].Path != "logs/run.log" || len(entry.TreeSHA256) != 64 {
		t.Fatalf("incomplete inventory: %+v", entry)
	}
	ledgerRaw, err := os.ReadFile(entry.Migration.LedgerPath)
	if err != nil || sha256Sum(ledgerRaw) != entry.Migration.LedgerSHA256 {
		t.Fatal("ledger input is not bound to the plan")
	}
	t.Setenv("AGENT_STATE_ROOT", cfg.Root)
	t.Setenv("AGENT_DRY_CONFIG", filepath.Join(t.TempDir(), "missing-config.json"))
	var out, errOut bytes.Buffer
	if code := run([]string{"migrate", "--apply", "--plan", plan.ID, "--plan-dir", directory}, &out, &errOut); code != exitOK {
		t.Fatalf("apply: code=%d err=%q", code, errOut.String())
	}
	actual, err := readManifest(bundles[0])
	if err != nil || !reflect.DeepEqual(actual, m) {
		t.Fatalf("manifest differs from plan: %+v err=%v", actual, err)
	}
	info, err := os.Stat(filepath.Join(bundles[0], "manifest.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("manifest is not private")
	}
	after := snapshotStateTree(t, cfg.Root)
	for path, value := range before {
		if after[path] != value {
			t.Fatalf("legacy data changed: %s", path)
		}
	}
	var receipt struct {
		Digest string      `json:"plan_digest"`
		Items  []planEntry `json:"items"`
	}
	if err := readJSONFile(rootPath(cfg, "_registry", "receipts", plan.ID+"-migrate.json"), &receipt); err != nil || receipt.Digest != plan.Digest || !reflect.DeepEqual(receipt.Items, plan.Candidates) {
		t.Fatalf("receipt: %+v err=%v", receipt, err)
	}
	next, nextDirectory, _ := reviewedMigrationPlan(t, cfg)
	if len(next.Candidates) != 0 || len(next.Blocked) != 0 || applyReviewedMigration(cfg, next, nextDirectory) != exitOK {
		t.Fatalf("repeat was not a no-op: %+v", next)
	}
	if !maps.Equal(after, snapshotStateTree(t, cfg.Root)) {
		t.Fatal("repeat rewrote manifests or created a receipt")
	}
}

func TestMigrateRejectsUsageWithoutSideEffects(t *testing.T) {
	for _, args := range [][]string{{"--dry-run", "--apply", "--plan", "plan-example"}, {"--apply"}, {"--plan", "plan-example"}, {"unexpected"}, {"--unknown"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			parent := t.TempDir()
			cfg := testConfig(filepath.Join(parent, "absent"))
			before := snapshotStateTree(t, parent)
			if code := migrateCommand(cfg, args, io.Discard, io.Discard); code != exitUsage {
				t.Fatalf("code=%d", code)
			}
			if !maps.Equal(before, snapshotStateTree(t, parent)) {
				t.Fatal("usage error created state")
			}
		})
	}
}

func TestMigrateRejectsChangedPlanInputs(t *testing.T) {
	for _, change := range []string{"payload", "timestamp", "ledger", "policy", "pin", "lock", "association", "scope", "manifest", "tamper", "wrong-action"} {
		t.Run(change, func(t *testing.T) {
			cfg, bundles := migrationFixture(t, "bundle-a")
			plan, directory, _ := reviewedMigrationPlan(t, cfg)
			switch change {
			case "payload":
				putFile(t, filepath.Join(bundles[0], "logs", "run.log"), "CHANGED DATA\n")
			case "timestamp":
				stamp := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
				if err := os.Chtimes(filepath.Join(bundles[0], "logs", "run.log"), stamp, stamp); err != nil {
					t.Fatal(err)
				}
			case "ledger":
				path := rootPath(cfg, "alpha", "legacy-goal.json")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				putFile(t, path, string(raw)+"\n")
			case "policy":
				cfg.ActiveLedgerDays++
			case "pin":
				cfg.PinnedPaths = []string{bundles[0]}
			case "lock":
				putFile(t, filepath.Join(bundles[0], "writer.lock"), fmt.Sprint(os.Getpid()))
			case "association":
				putMigrationLedger(t, cfg, "other-goal", []string{bundles[0]}, "2020-01-03T00:00:00Z")
			case "scope":
				putFile(t, rootPath(cfg, "alpha", "evidence", "another-bundle", "proof"), "new")
			case "manifest":
				putFile(t, filepath.Join(bundles[0], "manifest.json"), "{}")
			case "tamper":
				plan.Candidates[0].Migration.Manifest.GoalID = "unreviewed-goal"
				if err := writeJSONAtomic(filepath.Join(directory, plan.ID+".json"), plan, 0o600); err != nil {
					t.Fatal(err)
				}
			case "wrong-action":
				plan.Action = ""
				if err := sealPlan(&plan); err != nil {
					t.Fatal(err)
				}
				if err := persistPlan(plan, directory); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshotStateTree(t, cfg.Root)
			if code := applyReviewedMigration(cfg, plan, directory); code != exitBlocked {
				t.Fatalf("changed input accepted: code=%d", code)
			}
			if !maps.Equal(before, snapshotStateTree(t, cfg.Root)) {
				t.Fatal("failed preflight changed state")
			}
		})
	}
}

func TestMigrateSkipsProtectedAndUnreadableBundles(t *testing.T) {
	for _, gate := range []string{"pinned_path", "active_ledger_reference", "open_file_or_process_cwd", "live_process_lock", "lock_file_pid_unknown", "unreadable_content", "open_file_scan_unavailable", "git_dirty", "symlink"} {
		t.Run(gate, func(t *testing.T) {
			cfg, bundles := migrationFixture(t, "bundle-a")
			bundle := bundles[0]
			expected := gate
			switch gate {
			case "pinned_path":
				cfg.PinnedPaths = []string{filepath.Join(bundle, "logs")}
			case "active_ledger_reference":
				putMigrationLedger(t, cfg, "legacy-goal", []string{bundle}, time.Now().UTC().Format(time.RFC3339))
			case "open_file_or_process_cwd":
				safetyOpenPaths = func() ([]string, error) { return []string{filepath.Join(bundle, "logs", "run.log")}, nil }
			case "live_process_lock":
				putFile(t, filepath.Join(bundle, "postmaster.pid"), fmt.Sprint(os.Getpid()))
			case "lock_file_pid_unknown":
				putFile(t, filepath.Join(bundle, "writer.lock"), "unknown")
			case "unreadable_content":
				base := hashFile
				hashFile = func(path string) (string, int64, error) {
					if inside(bundle, path) {
						return "", 0, os.ErrPermission
					}
					return base(path)
				}
				t.Cleanup(func() { hashFile = base })
			case "open_file_scan_unavailable":
				safetyOpenPaths = func() ([]string, error) { return nil, errors.New("scan unavailable") }
			case "git_dirty":
				putFile(t, filepath.Join(bundle, ".git"), "gitdir: fixture")
				base := safetyRepoState
				safetyRepoState = func(string) ([]repoStateRecord, error) { return []repoStateRecord{{Untracked: 1}}, nil }
				t.Cleanup(func() { safetyRepoState = base })
			case "symlink":
				outside := filepath.Join(t.TempDir(), "outside")
				putFile(t, outside, "do not follow")
				if err := os.Symlink(outside, filepath.Join(bundle, "link")); err != nil {
					t.Fatal(err)
				}
				expected = "unreadable_content"
			}
			before := snapshotStateTree(t, cfg.Root)
			plan, _, output := reviewedMigrationPlan(t, cfg)
			if len(plan.Candidates) != 0 || len(plan.Blocked) != 1 || !contains(plan.Blocked[0].Reasons, expected) || !strings.Contains(output, expected) {
				t.Fatalf("missing safety skip: %+v output=%q", plan, output)
			}
			if !maps.Equal(before, snapshotStateTree(t, cfg.Root)) {
				t.Fatal("skip changed legacy data")
			}
		})
	}
}

func TestMigrateGoalInferenceNeverGuesses(t *testing.T) {
	for _, mode := range []string{"missing", "ambiguous", "exact", "uri", "wrong-project", "bad-ledger-id"} {
		t.Run(mode, func(t *testing.T) {
			name := "bundle-a"
			if mode == "exact" {
				name = "legacy-goal"
			}
			cfg, bundles := migrationFixture(t, name)
			expected := ""
			switch mode {
			case "missing", "exact":
				putMigrationLedger(t, cfg, "legacy-goal", nil, "2020-01-03T00:00:00Z")
				if mode == "missing" {
					expected = "goal_not_found"
				}
			case "ambiguous":
				putMigrationLedger(t, cfg, "other-goal", []string{bundles[0]}, "2020-01-03T00:00:00Z")
				expected = "ambiguous_goal"
			case "uri":
				putMigrationLedger(t, cfg, "legacy-goal", []string{"agent-state://alpha/bundle-a/logs/run.log"}, "2020-01-03T00:00:00Z")
			case "wrong-project":
				putMigrationLedger(t, cfg, "legacy-goal", []string{"agent-state://other/bundle-a/logs/run.log"}, "2020-01-03T00:00:00Z")
				expected = "goal_not_found"
			case "bad-ledger-id":
				putFile(t, rootPath(cfg, "alpha", "legacy-goal.json"), `{"goal_id":"different"}`)
				expected = "ledger_inference_unavailable"
			}
			plan, _, _ := reviewedMigrationPlan(t, cfg)
			if expected == "" {
				if len(plan.Candidates) != 1 || plan.Candidates[0].Migration.Manifest.GoalID != "legacy-goal" {
					t.Fatalf("unambiguous goal not inferred: %+v", plan)
				}
			} else if len(plan.Candidates) != 0 || len(plan.Blocked) != 1 || !contains(plan.Blocked[0].Reasons, expected) {
				t.Fatalf("ambiguous goal guessed: %+v", plan)
			}
		})
	}
}

func TestMigratePreservesExistingManifestsAndSkipsUnsafeDirectory(t *testing.T) {
	cfg, bundles := migrationFixture(t, "managed", "invalid", "legacy")
	m := evidenceManifest{Schema: evidenceSchema, ID: "managed", Project: "alpha", Status: "open"}
	if err := writeJSONAtomic(filepath.Join(bundles[0], "manifest.json"), m, 0o600); err != nil {
		t.Fatal(err)
	}
	putFile(t, filepath.Join(bundles[1], "manifest.json"), `{"foreign":"manifest"}`)
	outside := t.TempDir()
	if err := os.MkdirAll(rootPath(cfg, "beta"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, rootPath(cfg, "beta", "evidence")); err != nil {
		t.Fatal(err)
	}
	before := snapshotStateTree(t, cfg.Root)
	plan, directory, _ := reviewedMigrationPlan(t, cfg)
	if len(plan.Candidates) != 1 || len(plan.Blocked) != 2 || plan.Candidates[0].Path != bundles[2] {
		t.Fatalf("inventory: %+v", plan)
	}
	if code := applyReviewedMigration(cfg, plan, directory); code != exitOK {
		t.Fatalf("unrelated unsafe directory blocked the safe candidate: %d", code)
	}
	after := snapshotStateTree(t, cfg.Root)
	for path, value := range before {
		if after[path] != value {
			t.Fatalf("overwrote legacy content: %s", path)
		}
	}
}

func TestMigrateRollsBackEntireBatch(t *testing.T) {
	for _, failure := range []string{"second-manifest", "receipt", "late-protection"} {
		t.Run(failure, func(t *testing.T) {
			cfg, bundles := migrationFixture(t, "bundle-a", "bundle-b")
			plan, directory, _ := reviewedMigrationPlan(t, cfg)
			before := snapshotStateTree(t, cfg.Root)
			baseWrite, baseProtection := writeStateBytes, protectionCheck
			t.Cleanup(func() { writeStateBytes, protectionCheck = baseWrite, baseProtection })
			written := 0
			writeStateBytes = func(path string, data []byte, mode fs.FileMode) error {
				if err := baseWrite(path, data, mode); err != nil {
					return err
				}
				if filepath.Base(path) == "manifest.json" {
					written++
				}
				if failure == "second-manifest" && path == filepath.Join(bundles[1], "manifest.json") || failure == "receipt" && strings.HasSuffix(path, "-migrate.json") {
					return errors.New("injected failure after atomic write")
				}
				return nil
			}
			protectionCheck = func(cfg config, idx registryIndex, path string) []string {
				reasons := baseProtection(cfg, idx, path)
				if failure == "late-protection" && written != 0 && path == bundles[1] {
					return append(reasons, "late_protection")
				}
				return reasons
			}
			code := applyReviewedMigration(cfg, plan, directory)
			if code == exitOK || written == 0 {
				t.Fatalf("failure did not exercise rollback: code=%d written=%d", code, written)
			}
			if !maps.Equal(before, snapshotStateTree(t, cfg.Root)) {
				t.Fatal("failed batch left manifests, receipts or scaffolding")
			}
		})
	}
}

func TestMigratePreflightsReceiptBeforeWritingAnyManifest(t *testing.T) {
	cfg, _ := migrationFixture(t, "bundle-a", "bundle-b")
	plan, directory, _ := reviewedMigrationPlan(t, cfg)
	putFile(t, rootPath(cfg, "_registry", "receipts"), "not a directory")
	before := snapshotStateTree(t, cfg.Root)
	if code := applyReviewedMigration(cfg, plan, directory); code != exitBlocked {
		t.Fatalf("unsafe destination accepted: %d", code)
	}
	if !maps.Equal(before, snapshotStateTree(t, cfg.Root)) {
		t.Fatal("receipt preflight changed state")
	}
}

func TestMigrateReadsAndCommitsUnderRegistryLock(t *testing.T) {
	cfg, _ := migrationFixture(t, "bundle-a")
	putFile(t, lockPath(cfg), "")
	checkLock := func() {
		t.Helper()
		f, err := os.OpenFile(lockPath(cfg), os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			t.Fatal("migration read or write escaped registry lock")
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			t.Fatal(err)
		}
	}
	baseRead, baseWrite, baseProtection := readStateFile, writeStateBytes, protectionCheck
	t.Cleanup(func() { readStateFile, writeStateBytes, protectionCheck = baseRead, baseWrite, baseProtection })
	reads, writes, checks := 0, 0, 0
	readStateFile = func(path string) ([]byte, error) {
		checkLock()
		reads++
		return baseRead(path)
	}
	writeStateBytes = func(path string, data []byte, mode fs.FileMode) error {
		checkLock()
		writes++
		return baseWrite(path, data, mode)
	}
	protectionCheck = func(cfg config, idx registryIndex, path string) []string {
		checkLock()
		checks++
		return baseProtection(cfg, idx, path)
	}
	// Avoid the test helper's post-command loadPlan, which intentionally reads unlocked.
	directory := t.TempDir()
	var out bytes.Buffer
	if code := migrateCommand(cfg, []string{"--plan-dir", directory}, &out, io.Discard); code != exitOK {
		t.Fatal(code)
	}
	id := strings.TrimPrefix(strings.Fields(out.String())[0], "plan_id=")
	if code := migrateCommand(cfg, []string{"--apply", "--plan", id, "--plan-dir", directory}, io.Discard, io.Discard); code != exitOK {
		t.Fatal(code)
	}
	if reads == 0 || writes < 3 || checks == 0 {
		t.Fatalf("incomplete lock coverage: reads=%d writes=%d checks=%d", reads, writes, checks)
	}
}

func TestMigrateCanonicalizesConfiguredRoot(t *testing.T) {
	cfg, _ := migrationFixture(t, "bundle-a")
	alias := filepath.Join(t.TempDir(), "state-alias")
	if err := os.Symlink(cfg.Root, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_STATE_ROOT", alias)
	t.Setenv("AGENT_DRY_CONFIG", filepath.Join(t.TempDir(), "missing-config.json"))
	directory, err := canonicalPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"migrate", "--dry-run", "--plan-dir", directory}, &out, &errOut); code != exitOK {
		t.Fatalf("code=%d err=%q", code, errOut.String())
	}
	id := strings.TrimPrefix(strings.Fields(out.String())[0], "plan_id=")
	plan, err := loadPlan(cfg, id, directory)
	if err != nil || plan.Root != cfg.Root || len(plan.Candidates) != 1 {
		t.Fatalf("canonical plan: %+v err=%v", plan, err)
	}
	entry := plan.Candidates[0]
	if !filepath.IsAbs(entry.Path) || !strings.HasPrefix(entry.Path, cfg.Root+string(filepath.Separator)) || !filepath.IsAbs(entry.Migration.LedgerPath) {
		t.Fatalf("noncanonical paths: %+v", entry)
	}
}
