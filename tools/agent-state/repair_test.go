package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func expiredScratch(t *testing.T) (config, string) {
	t.Helper()
	cfg := testConfig(fixtureDir(t))
	path := rootPath(cfg, "alpha", "scratch", "lease-expired")
	putFile(t, filepath.Join(path, "data"), "keep")
	stamp := time.Now().Add(-5 * 24 * time.Hour).UTC().Format(time.RFC3339)
	idx := registryIndex{Schema: indexSchema, Leases: []lease{{ID: "lease-expired", Project: "alpha", Path: path, CreatedAt: stamp, RenewedAt: stamp, ExpiresAt: stamp}}}
	if err := saveIndex(cfg, idx); err != nil {
		t.Fatal(err)
	}
	saved := safetyOpenPaths
	safetyOpenPaths = func() ([]string, error) { return nil, nil }
	t.Cleanup(func() { safetyOpenPaths = saved })
	return cfg, path
}

func TestRepairRejectConflictingFlags(t *testing.T) {
	for _, command := range []string{"gc", "archive-policy", "archive-bundle"} {
		t.Run(command, func(t *testing.T) {
			cfg, path := expiredScratch(t)
			plan, err := buildPlan(cfg, "scratch")
			if err != nil {
				t.Fatal(err)
			}
			if err := persistPlan(plan, plansPath(cfg)); err != nil {
				t.Fatal(err)
			}
			calls := 0
			if command == "archive-policy" {
				cfg.Archive.Repo = filepath.Join(fixtureDir(t), "archive")
				cfg.Archive.PasswordRef = "fixture/password#key"
				cfg.Archive.RetentionMonths = 12
				for _, name := range []string{"restic", "bao-run"} {
					stub := filepath.Join(fixtureDir(t), name)
					putFile(t, stub, "#!/bin/sh\nexit 99\n")
					if err := os.Chmod(stub, 0o700); err != nil {
						t.Fatal(err)
					}
					t.Setenv("PATH", filepath.Dir(stub)+string(os.PathListSeparator)+os.Getenv("PATH"))
				}
				savedExec := archiveExec
				t.Cleanup(func() { archiveExec = savedExec })
				archiveExec = func(_ string, args []string, _ []string) ([]byte, error) {
					calls++
					if args[0] == "snapshots" {
						return []byte(`[{"id":"aaaaaaaa","time":"2020-01-01T00:00:00Z","tags":["class=evidence"]}]`), nil
					}
					return nil, nil
				}
				plan.Action = "archive_retention"
				plan.Candidates = nil
				plan.Blocked = nil
				plan.SnapshotIDs = []string{"aaaaaaaa"}
				plan.Policy.ArchiveRetentionMonths = 12
				if err := sealPlan(&plan); err != nil {
					t.Fatal(err)
				}
				if err := persistPlan(plan, plansPath(cfg)); err != nil {
					t.Fatal(err)
				}
			}
			var out, errOut bytes.Buffer
			args := []string{"--dry-run", "--apply", "--plan", plan.ID}
			var code int
			switch command {
			case "gc":
				code = gcCommand(cfg, args, &out, &errOut)
			case "archive-policy":
				code = archiveCommand(cfg, append([]string{"--policy"}, args...), &out, &errOut)
			default:
				code = archiveCommand(cfg, append([]string{"bundle"}, args...), &out, &errOut)
			}
			if calls != 0 {
				t.Errorf("conflicting flags invoked restic %d times", calls)
			}
			if code != exitUsage || !strings.Contains(errOut.String(), "--dry-run") || !strings.Contains(errOut.String(), "--apply") {
				t.Errorf("code=%d err=%q", code, errOut.String())
			}
			if _, err := os.Stat(filepath.Join(path, "data")); err != nil {
				t.Errorf("conflicting flags changed scratch: %v", err)
			}
		})
	}
}

func TestRepairConfiguredPinBlocksArchive(t *testing.T) {
	cfg := testConfig(fixtureDir(t))
	cfg.Archive.Repo = filepath.Join(fixtureDir(t), "archive")
	cfg.Archive.PasswordRef = "fixture/password#key"
	for _, name := range []string{"restic", "bao-run"} {
		stub := filepath.Join(fixtureDir(t), name)
		putFile(t, stub, "#!/bin/sh\nexit 99\n")
		if err := os.Chmod(stub, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", filepath.Dir(stub)+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	bundle := rootPath(cfg, "alpha", "evidence", "bundle")
	m := evidenceManifest{Schema: evidenceSchema, ID: "bundle", Project: "alpha", Status: "closed", ClosedAt: time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339)}
	if err := writeJSONAtomic(filepath.Join(bundle, "manifest.json"), m, 0o600); err != nil {
		t.Fatal(err)
	}
	sum, size, err := hashFile(filepath.Join(bundle, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	plan := gcPlan{Schema: planSchema, ID: "plan-pinned", Root: cfg.Root, Candidates: []planEntry{{Path: bundle, Class: "evidence", Action: "archive_then_stub", Bytes: size, ManifestSHA256: sum}}}
	if err := sealPlan(&plan); err != nil {
		t.Fatal(err)
	}
	if err := persistPlan(plan, plansPath(cfg)); err != nil {
		t.Fatal(err)
	}
	cfg.PinnedPaths = []string{bundle}
	saved := safetyOpenPaths
	safetyOpenPaths = func() ([]string, error) { return nil, nil }
	t.Cleanup(func() { safetyOpenPaths = saved })
	var out, errOut bytes.Buffer
	if code := archiveCommand(cfg, []string{bundle, "--apply", "--plan", plan.ID}, &out, &errOut); code != exitBlocked || !strings.Contains(errOut.String(), "pinned_path") {
		t.Fatalf("archive=%d %s", code, errOut.String())
	}
	if _, err := readManifest(bundle); err != nil {
		t.Fatal(err)
	}
}

func TestRepairGCTrashReceipt(t *testing.T) {
	cfg, path := expiredScratch(t)
	plan, err := buildPlan(cfg, "scratch")
	if err != nil {
		t.Fatal(err)
	}
	if err := persistPlan(plan, plansPath(cfg)); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := gcCommand(cfg, []string{"--apply", "--plan", plan.ID}, &out, &errOut); code != exitOK {
		t.Fatalf("gc=%d %s", code, errOut.String())
	}
	rel, _ := filepath.Rel(cfg.Root, path)
	if _, err := os.Stat(rootPath(cfg, "_registry", "trash", plan.ID, rel, "data")); err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		PlanID string      `json:"plan_id"`
		Items  []planEntry `json:"items"`
	}
	if err := readJSONFile(rootPath(cfg, "_registry", "receipts", plan.ID+"-gc.json"), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.PlanID != plan.ID || len(receipt.Items) != 1 || receipt.Items[0].Path != path {
		t.Fatalf("receipt=%+v", receipt)
	}
}

func TestRepairApplyRechecksSafety(t *testing.T) {
	for _, kind := range []string{"pin", "open-file", "cwd", "ledger", "dirty", "unpushed"} {
		t.Run(kind, func(t *testing.T) {
			cfg, path := expiredScratch(t)
			plan, err := buildPlan(cfg, "scratch")
			if err != nil {
				t.Fatal(err)
			}
			if err := persistPlan(plan, plansPath(cfg)); err != nil {
				t.Fatal(err)
			}
			savedRepo := safetyRepoState
			t.Cleanup(func() { safetyRepoState = savedRepo })
			switch kind {
			case "pin":
				cfg.PinnedPaths = []string{path}
			case "open-file":
				safetyOpenPaths = func() ([]string, error) { return []string{filepath.Join(path, "data")}, nil }
			case "cwd":
				safetyOpenPaths = func() ([]string, error) { return []string{path}, nil }
			case "ledger":
				putFile(t, rootPath(cfg, "alpha", "goal.json"), `{"updated_at":"`+time.Now().Add(-6*24*time.Hour).UTC().Format(time.RFC3339)+`","evidence":"`+path+`"}`)
			default:
				putFile(t, filepath.Join(path, ".git"), "gitdir: fixture")
				plan, err = buildPlan(cfg, "scratch")
				if err != nil {
					t.Fatal(err)
				}
				// A repository can change status without changing bytes inside a linked worktree.
				// Build a clean reviewed plan before changing the repo-state response.
				upstream := "origin/main"
				ahead := 0
				safetyRepoState = func(string) ([]repoStateRecord, error) {
					return []repoStateRecord{{Upstream: &upstream, Ahead: &ahead}}, nil
				}
				savedHistory := safetyGitHistory
				safetyGitHistory = func(string) ([]string, error) { return nil, nil }
				t.Cleanup(func() { safetyGitHistory = savedHistory })
				plan, err = buildPlan(cfg, "scratch")
				if err != nil {
					t.Fatal(err)
				}
				if err := persistPlan(plan, plansPath(cfg)); err != nil {
					t.Fatal(err)
				}
				safetyRepoState = func(string) ([]repoStateRecord, error) {
					if kind == "dirty" {
						return []repoStateRecord{{Staged: 1, Upstream: &upstream, Ahead: &ahead}}, nil
					}
					ahead = 1
					return []repoStateRecord{{Upstream: &upstream, Ahead: &ahead}}, nil
				}
			}
			var out, errOut bytes.Buffer
			if code := gcCommand(cfg, []string{"--apply", "--plan", plan.ID}, &out, &errOut); code != exitBlocked {
				t.Fatalf("apply=%d %s", code, errOut.String())
			}
			if _, err := os.Stat(filepath.Join(path, "data")); err != nil {
				t.Fatal("protected path moved")
			}
		})
	}
}

func TestRepairConfiguredPinRemovalPersistsAndAddRestores(t *testing.T) {
	cfg, path := expiredScratch(t)
	cfg.PinnedPaths = []string{path}
	var out, errOut bytes.Buffer
	if code := pinCommand(cfg, []string{"rm", path}, &out, &errOut); code != exitOK {
		t.Fatalf("rm=%d %s", code, errOut.String())
	}
	idx, err := loadIndex(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if pinBlocks(path, idx.Pins) {
		t.Fatal("explicit removal of configured pin did not persist")
	}
	if code := pinCommand(cfg, []string{"add", path, "--reason", "protect"}, &out, &errOut); code != exitOK {
		t.Fatal(code)
	}
	idx, err = loadIndex(cfg)
	if err != nil || !pinBlocks(path, idx.Pins) {
		t.Fatalf("add did not restore protection: %+v %v", idx, err)
	}
}

func TestRepairConfiguredPinAliasBlocksGCAtApply(t *testing.T) {
	cfg, path := expiredScratch(t)
	plan, err := buildPlan(cfg, "scratch")
	if err != nil {
		t.Fatal(err)
	}
	if err := persistPlan(plan, plansPath(cfg)); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(fixtureDir(t), "state-link")
	if err := os.Symlink(cfg.Root, alias); err != nil {
		t.Fatal(err)
	}
	rel, _ := filepath.Rel(cfg.Root, path)
	cfg.PinnedPaths = []string{filepath.Join(alias, rel)}
	current, err := buildPlan(cfg, "scratch")
	if err != nil {
		t.Fatal(err)
	}
	if len(current.Candidates) != 0 || len(current.Blocked) != 1 || !contains(current.Blocked[0].Reasons, "pinned_path") {
		t.Errorf("configured alias pin bypassed: %+v", current)
	}
	var out, errOut bytes.Buffer
	if code := gcCommand(cfg, []string{"--apply", "--plan", plan.ID}, &out, &errOut); code != exitBlocked {
		t.Errorf("apply=%d err=%s", code, errOut.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("pinned scratch moved: %v", err)
	}
}

func TestRepairDoctorReportsSymlinkRootAndGCStaysInside(t *testing.T) {
	cfg, path := expiredScratch(t)
	alias := filepath.Join(fixtureDir(t), "state-link")
	if err := os.Symlink(cfg.Root, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_STATE_ROOT", alias)
	t.Setenv("AGENT_DRY_CONFIG", filepath.Join(fixtureDir(t), "absent.json"))
	var out, errOut bytes.Buffer
	if code := run([]string{"doctor", "--json"}, &out, &errOut); code != exitOK {
		t.Fatalf("doctor=%d %s", code, errOut.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	real, err := canonicalPath(cfg.Root)
	if err != nil {
		t.Fatal(err)
	}
	if doc["root_path"] != alias || doc["resolved_root_path"] != real {
		t.Errorf("missing root identities: %s", out.String())
	}
	loaded, err := loadConfig(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := buildPlan(loaded, "scratch")
	if err != nil {
		t.Fatalf("resolved root GC: %v", err)
	}
	if len(plan.Candidates) != 1 {
		t.Fatalf("candidates=%+v", plan)
	}
	external := filepath.Join(fixtureDir(t), "outside")
	putFile(t, filepath.Join(external, "keep"), "outside")
	if err := os.Symlink(external, filepath.Join(path, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := persistPlan(plan, plansPath(loaded)); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := gcCommand(loaded, []string{"--apply", "--plan", plan.ID}, &out, &errOut); code != exitBlocked {
		t.Fatalf("escape apply=%d %s", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(external, "keep")); err != nil {
		t.Fatal("GC escaped root")
	}
}
