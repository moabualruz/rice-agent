package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLedgerStructuralNamesProtectGC(t *testing.T) {
	for _, project := range []string{"scratch", "evidence", "ledger-history"} {
		for _, phase := range []string{"plan", "apply"} {
			t.Run(project+"/"+phase, func(t *testing.T) {
				cfg, candidate := expiredScratch(t)
				plan, err := buildPlan(cfg, "scratch")
				if err != nil || len(plan.Candidates) != 1 {
					t.Fatalf("initial plan=%+v err=%v", plan, err)
				}
				if err := persistPlan(plan, plansPath(cfg)); err != nil {
					t.Fatal(err)
				}
				var ledger bytes.Buffer
				if code := ledgerCommand(cfg, []string{"path", project, "goal"}, &ledger, io.Discard); code != exitOK {
					t.Fatal(code)
				}
				if err := writeJSONAtomic(strings.TrimSpace(ledger.String()), map[string]string{"updated_at": time.Now().UTC().Format(time.RFC3339), "scratch": candidate}, 0o600); err != nil {
					t.Fatal(err)
				}
				if phase == "plan" {
					next, err := buildPlan(cfg, "scratch")
					if err != nil || len(next.Candidates) != 0 || len(next.Blocked) != 1 || !strings.Contains(strings.Join(next.Blocked[0].Reasons, ","), "active_ledger_reference") {
						t.Fatalf("active ledger ignored: plan=%+v err=%v", next, err)
					}
				} else if code := applyGC(cfg, plan.ID, plansPath(cfg), io.Discard, io.Discard); code != exitBlocked {
					t.Fatalf("active ledger ignored at apply: code=%d", code)
				}
				if _, err := os.Stat(candidate); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

// The command adapters are isolated from real restic, OpenBao and process scans.
func archiveFixture(t *testing.T) (config, string, gcPlan) {
	t.Helper()
	cfg := testConfig(t.TempDir())
	cfg.Archive.Repo = filepath.Join(t.TempDir(), "archive")
	cfg.Archive.PasswordRef = "fixture/password#key"
	bin := t.TempDir()
	for _, name := range []string{"restic", "bao-run"} {
		path := filepath.Join(bin, name)
		putFile(t, path, "#!/bin/sh\nexit 99\n")
		if err := os.Chmod(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	bundle := rootPath(cfg, "alpha", "evidence", "bundle")
	m := evidenceManifest{Schema: evidenceSchema, ID: "bundle", Project: "alpha", Status: "closed", ClosedAt: time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339)}
	if err := writeJSONAtomic(filepath.Join(bundle, "manifest.json"), m, 0o600); err != nil {
		t.Fatal(err)
	}
	oldExec, oldOpen, oldScan := archiveExec, safetyOpenPaths, scanSecrets
	t.Cleanup(func() { archiveExec, safetyOpenPaths, scanSecrets = oldExec, oldOpen, oldScan })
	safetyOpenPaths = func() ([]string, error) { return nil, nil }
	scanSecrets = func(string) (bool, error) { return false, nil }
	plan, err := buildPlan(cfg, "evidence")
	if err != nil || len(plan.Candidates) != 1 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	if err := persistPlan(plan, plansPath(cfg)); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(bundle, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	archiveExec = func(_ string, args []string, _ []string) ([]byte, error) {
		switch args[0] {
		case "backup":
			return []byte(`{"snapshot_id":"aaaaaaaa"}`), nil
		case "restore":
			var target string
			for i, arg := range args {
				if arg == "--target" {
					target = args[i+1]
				}
			}
			putFile(t, filepath.Join(target, strings.TrimLeft(bundle, string(filepath.Separator)), "manifest.json"), string(manifest))
		case "check":
		default:
			t.Fatalf("unexpected restic command %q", args[0])
		}
		return nil, nil
	}
	return cfg, bundle, plan
}

func TestArchiveRechecksProtectionBeforeStaging(t *testing.T) {
	for _, protection := range []string{"ledger", "open-file", "changed-manifest", "changed-content"} {
		t.Run(protection, func(t *testing.T) {
			cfg, bundle, plan := archiveFixture(t)
			baseExec := archiveExec
			archiveExec = func(name string, args []string, env []string) ([]byte, error) {
				out, err := baseExec(name, args, env)
				if args[0] == "check" {
					switch protection {
					case "pin":
						if code := pinCommand(cfg, []string{"add", bundle, "--reason", "retain"}, io.Discard, io.Discard); code != exitOK {
							t.Fatal(code)
						}
					case "ledger":
						if err := writeJSONAtomic(rootPath(cfg, "alpha", "goal.json"), map[string]string{"updated_at": time.Now().UTC().Format(time.RFC3339), "evidence": bundle}, 0o600); err != nil {
							t.Fatal(err)
						}
					case "open-file":
						safetyOpenPaths = func() ([]string, error) { return []string{filepath.Join(bundle, "manifest.json")}, nil }
					case "changed-manifest":
						m, err := readManifest(bundle)
						if err != nil {
							t.Fatal(err)
						}
						m.Status = "edited" // Same byte length as closed; size checks alone cannot detect this.
						if err := writeJSONAtomic(filepath.Join(bundle, "manifest.json"), m, 0o600); err != nil {
							t.Fatal(err)
						}
					case "changed-content":
						putFile(t, filepath.Join(bundle, "new-data"), "not backed up")
					}
				}
				return out, err
			}
			var stderr bytes.Buffer
			if code := archiveCommand(cfg, []string{bundle, "--apply", "--plan", plan.ID}, io.Discard, &stderr); code == exitOK {
				t.Fatalf("archive staged newly protected or changed bundle: %s", stderr.String())
			}
			m, err := readManifest(bundle)
			if err != nil || m.Archive != nil {
				t.Fatalf("hot bundle not preserved: %+v %v", m, err)
			}
			if _, err := os.Stat(rootPath(cfg, "_registry", "trash", "archive-bundle")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("bundle moved to trash: %v", err)
			}
		})
	}
}

func TestRestoreRechecksBeforeReplacingTarget(t *testing.T) {
	for _, change := range []string{"ledger", "open-file", "changed-stub", "new-target"} {
		t.Run(change, func(t *testing.T) {
			cfg, bundle, plan := archiveFixture(t)
			if code := archiveCommand(cfg, []string{bundle, "--apply", "--plan", plan.ID}, io.Discard, io.Discard); code != exitOK {
				t.Fatal(code)
			}
			target := bundle
			args := []string{bundle}
			if change == "new-target" {
				target = rootPath(cfg, "alpha", "restored")
				args = append(args, "--to", target)
			}
			baseExec := archiveExec
			archiveExec = func(name string, args []string, env []string) ([]byte, error) {
				out, err := baseExec(name, args, env)
				if args[0] == "restore" {
					switch change {
					case "pin":
						if code := pinCommand(cfg, []string{"add", bundle, "--reason", "retain"}, io.Discard, io.Discard); code != exitOK {
							t.Fatal(code)
						}
					case "ledger":
						if err := writeJSONAtomic(rootPath(cfg, "alpha", "goal.json"), map[string]string{"updated_at": time.Now().UTC().Format(time.RFC3339), "evidence": bundle}, 0o600); err != nil {
							t.Fatal(err)
						}
					case "open-file":
						safetyOpenPaths = func() ([]string, error) { return []string{filepath.Join(bundle, "manifest.json")}, nil }
					case "changed-stub":
						putFile(t, filepath.Join(bundle, "new-data"), "retain")
					case "new-target":
						// A second restore may win the destination while restic runs.
						if err := os.Mkdir(target, 0o700); err != nil {
							t.Fatal(err)
						}
					}
				}
				return out, err
			}
			if code := restoreCommand(cfg, args, io.Discard, io.Discard); code != exitBlocked {
				t.Fatalf("restore replaced changed or protected target: code=%d", code)
			}
			if change == "changed-stub" {
				if _, err := os.Stat(filepath.Join(bundle, "new-data")); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestEvidenceMutationsWaitForRegistryLock(t *testing.T) {
	isolateOpenScan(t)
	for _, action := range []string{"put", "close"} {
		t.Run(action, func(t *testing.T) {
			cfg := testConfig(t.TempDir())
			var out bytes.Buffer
			if code := evidenceCommand(cfg, []string{"new", "alpha", "--goal", "goal", "--criterion", "proof"}, &out, io.Discard); code != exitOK {
				t.Fatal(code)
			}
			bundle := strings.TrimSpace(out.String())
			input := filepath.Join(t.TempDir(), "receipt")
			putFile(t, input, "result")
			args := []string{"put", bundle, input, "--role", "receipt"}
			if action == "close" {
				args = []string{"close", bundle, "--result", "pass"}
			}
			if err := mkdirState(cfg.Root, filepath.Dir(lockPath(cfg))); err != nil {
				t.Fatal(err)
			}
			f, err := os.OpenFile(lockPath(cfg), os.O_CREATE|os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
				t.Fatal(err)
			}
			defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			done := make(chan int, 1)
			go func() { done <- evidenceCommand(cfg, args, io.Discard, io.Discard) }()
			select {
			case code := <-done:
				t.Fatalf("evidence %s bypassed registry lock: code=%d", action, code)
			case <-time.After(100 * time.Millisecond):
			}
			if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
				t.Fatal(err)
			}
			if code := <-done; code != exitOK {
				t.Fatal(code)
			}
		})
	}
}

func TestGCRechecksAfterSlowSafetyChecks(t *testing.T) {
	for _, change := range []string{"ledger", "open-file", "content"} {
		t.Run(change, func(t *testing.T) {
			cfg, candidate := expiredScratch(t)
			if err := os.Mkdir(filepath.Join(candidate, ".git"), 0o700); err != nil {
				t.Fatal(err)
			}
			oldRepo, oldHistory := safetyRepoState, safetyGitHistory
			t.Cleanup(func() { safetyRepoState, safetyGitHistory = oldRepo, oldHistory })
			cleanRepo := func(string) ([]repoStateRecord, error) {
				upstream, ahead := "origin/main", 0
				return []repoStateRecord{{Upstream: &upstream, Ahead: &ahead}}, nil
			}
			safetyRepoState = cleanRepo
			safetyGitHistory = func(string) ([]string, error) { return nil, nil }
			plan, err := buildPlan(cfg, "scratch")
			if err != nil || len(plan.Candidates) != 1 {
				t.Fatalf("plan=%+v err=%v", plan, err)
			}
			if err := persistPlan(plan, plansPath(cfg)); err != nil {
				t.Fatal(err)
			}
			safetyRepoState = func(path string) ([]repoStateRecord, error) {
				switch change {
				case "ledger":
					if err := writeJSONAtomic(rootPath(cfg, "alpha", "goal.json"), map[string]string{"updated_at": time.Now().UTC().Format(time.RFC3339), "scratch": candidate}, 0o600); err != nil {
						t.Fatal(err)
					}
				case "open-file":
					safetyOpenPaths = func() ([]string, error) { return []string{filepath.Join(candidate, "data")}, nil }
				case "content":
					putFile(t, filepath.Join(candidate, "data"), "edit")
				}
				return cleanRepo(path)
			}
			if code := applyGC(cfg, plan.ID, plansPath(cfg), io.Discard, io.Discard); code != exitBlocked {
				t.Fatalf("late %s change ignored: %d", change, code)
			}
			if _, err := os.Stat(filepath.Join(candidate, "data")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGCLateProtectionRollsBackEarlierMoves(t *testing.T) {
	cfg, first := expiredScratch(t)
	idx, err := loadIndex(cfg)
	if err != nil {
		t.Fatal(err)
	}
	second := idx.Leases[0]
	second.ID = "lease-second"
	second.Path = rootPath(cfg, second.Project, "scratch", second.ID)
	putFile(t, filepath.Join(second.Path, "data"), "second")
	idx.Leases = append(idx.Leases, second)
	if err := saveIndex(cfg, idx); err != nil {
		t.Fatal(err)
	}
	plan, err := buildPlan(cfg, "scratch")
	if err != nil || len(plan.Candidates) != 2 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	if err := persistPlan(plan, plansPath(cfg)); err != nil {
		t.Fatal(err)
	}
	scans := 0
	safetyOpenPaths = func() ([]string, error) {
		scans++
		if scans >= 3 {
			return []string{second.Path}, nil
		}
		return nil, nil
	}
	if code := applyGC(cfg, plan.ID, plansPath(cfg), io.Discard, io.Discard); code != exitBlocked {
		t.Fatal(code)
	}
	for _, path := range []string{first, second.Path} {
		if _, err := os.Stat(filepath.Join(path, "data")); err != nil {
			t.Fatal(err)
		}
	}
	current, err := loadIndex(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range current.Leases {
		if item.CollectedAt != "" {
			t.Fatal("rolled back lease marked collected")
		}
	}
}

func TestArchiveFinalSafetyScanHoldsRegistryLock(t *testing.T) {
	cfg, bundle, plan := archiveFixture(t)
	checked := false
	baseExec := archiveExec
	archiveExec = func(name string, args []string, env []string) ([]byte, error) {
		out, err := baseExec(name, args, env)
		if args[0] == "check" {
			checked = true
		}
		return out, err
	}
	finalScan := false
	safetyOpenPaths = func() ([]string, error) {
		if !checked {
			return nil, nil
		}
		finalScan = true
		f, err := os.Open(indexLockAnchor(cfg))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			t.Error("final safety check did not hold registry lock")
		}
		return nil, nil
	}
	if code := archiveCommand(cfg, []string{bundle, "--apply", "--plan", plan.ID}, io.Discard, io.Discard); code != exitOK {
		t.Fatal(code)
	}
	if !finalScan {
		t.Fatal("no final safety scan after restic verification")
	}
}
