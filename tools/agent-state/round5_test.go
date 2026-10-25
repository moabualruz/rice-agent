package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Include directories and modes, not just payload hashes: rejected operations
// must not leave empty scaffolding behind.
func snapshotStateTree(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		value := info.Mode().String()
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value += string(data)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			value += target
		}
		result[rel] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestRound5MutationEntryPointsUseFullProtectionUnderLock(t *testing.T) {
	for _, entry := range []string{"gc-plan", "gc-apply", "archive", "retention-plan", "retention-apply", "restore", "evidence-new", "evidence-put", "evidence-blob", "evidence-close", "restore-cas", "scratch-new", "scratch-renew", "scratch-release"} {
		t.Run(entry, func(t *testing.T) {
			oldOpen := safetyOpenPaths
			safetyOpenPaths = func() ([]string, error) { return nil, nil }
			t.Cleanup(func() { safetyOpenPaths = oldOpen })
			cfg := testConfig(t.TempDir())
			var invoke func() int
			switch entry {
			case "gc-plan", "gc-apply":
				var candidate string
				cfg, candidate = expiredScratch(t)
				_ = candidate
				plan, err := buildPlan(cfg, "scratch")
				if err != nil {
					t.Fatal(err)
				}
				if err := persistPlan(plan, plansPath(cfg)); err != nil {
					t.Fatal(err)
				}
				if entry == "gc-plan" {
					invoke = func() int {
						_, err := buildPlan(cfg, "scratch")
						if err != nil {
							t.Fatal(err)
						}
						return exitOK
					}
				} else {
					invoke = func() int { return applyGC(cfg, plan.ID, plansPath(cfg), io.Discard, io.Discard) }
				}
			case "retention-plan", "retention-apply":
				var plan gcPlan
				cfg, plan, _, _ = round5RetentionFixture(t)
				if entry == "retention-plan" {
					invoke = func() int {
						if _, err := buildArchiveRetentionPlan(cfg); err != nil {
							t.Fatal(err)
						}
						return exitOK
					}
				} else {
					invoke = func() int {
						return archiveCommand(cfg, []string{"--policy", "--apply", "--plan", plan.ID}, io.Discard, io.Discard)
					}
				}
			case "archive", "restore":
				var bundle string
				var plan gcPlan
				cfg, bundle, plan = archiveFixture(t)
				if entry == "archive" {
					invoke = func() int {
						return archiveCommand(cfg, []string{bundle, "--apply", "--plan", plan.ID}, io.Discard, io.Discard)
					}
				} else {
					if code := archiveCommand(cfg, []string{bundle, "--apply", "--plan", plan.ID}, io.Discard, io.Discard); code != exitOK {
						t.Fatal(code)
					}
					invoke = func() int { return restoreCommand(cfg, []string{bundle}, io.Discard, io.Discard) }
				}
			case "restore-cas":
				blob := completionBlob(t, cfg)
				destination := rootPath(cfg, filepath.FromSlash(blob.Path))
				data, err := os.ReadFile(destination)
				if err != nil {
					t.Fatal(err)
				}
				restore := t.TempDir()
				putFile(t, filepath.Join(restore, strings.TrimLeft(destination, string(filepath.Separator))), string(data))
				if err := os.Remove(destination); err != nil {
					t.Fatal(err)
				}
				invoke = func() int {
					if err := restoreCASBlobs(cfg, restore, []fileRecord{blob}); err != nil {
						t.Error(err)
						return exitEnv
					}
					return exitOK
				}
			case "scratch-new":
				invoke = func() int { return scratchCommand(cfg, []string{"new", "alpha"}, io.Discard, io.Discard) }
			case "scratch-renew", "scratch-release":
				cfg, _ = expiredScratch(t)
				action := strings.TrimPrefix(entry, "scratch-")
				invoke = func() int { return scratchCommand(cfg, []string{action, "lease-expired"}, io.Discard, io.Discard) }
			default:
				args := []string{"new", "alpha", "--goal", "goal", "--criterion", "proof"}
				if entry != "evidence-new" {
					var out bytes.Buffer
					if code := evidenceCommand(cfg, args, &out, io.Discard); code != exitOK {
						t.Fatal(code)
					}
					bundle := strings.TrimSpace(out.String())
					if entry == "evidence-close" {
						args = []string{"close", bundle, "--result", "pass"}
					} else {
						input := filepath.Join(t.TempDir(), "input")
						putFile(t, input, "proof")
						role := "receipt"
						if entry == "evidence-blob" {
							role = "blob"
						}
						args = []string{"put", bundle, input, "--role", role}
					}
				}
				invoke = func() int { return evidenceCommand(cfg, args, io.Discard, io.Discard) }
			}
			base := protectionCheck
			calls := 0
			protectionCheck = func(c config, idx registryIndex, path string) []string {
				calls++
				f, err := os.Open(indexLockAnchor(c))
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
				if err == nil {
					syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
					t.Error("protection check ran outside registry lock")
				}
				return base(c, idx, path)
			}
			t.Cleanup(func() { protectionCheck = base })
			if code := invoke(); code != exitOK {
				t.Fatalf("%s code=%d", entry, code)
			}
			if calls == 0 {
				t.Fatalf("%s bypassed full protection check", entry)
			}
		})
	}
}

func TestRound5PinMutationsWriteUnderLock(t *testing.T) {
	for _, action := range []string{"add", "rm"} {
		t.Run(action, func(t *testing.T) {
			cfg := testConfig(t.TempDir())
			path := rootPath(cfg, "retain")
			if action == "rm" {
				if code := pinCommand(cfg, []string{"add", path, "--reason", "retain"}, io.Discard, io.Discard); code != exitOK {
					t.Fatal(code)
				}
			}
			base := writeStateBytes
			writes := 0
			writeStateBytes = func(path string, data []byte, mode fs.FileMode) error {
				writes++
				f, err := os.Open(indexLockAnchor(cfg))
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
					syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
					t.Error("pin write ran outside lock")
				}
				return base(path, data, mode)
			}
			t.Cleanup(func() { writeStateBytes = base })
			args := []string{action, path}
			if action == "add" {
				args = append(args, "--reason", "retain")
			}
			if code := pinCommand(cfg, args, io.Discard, io.Discard); code != exitOK {
				t.Fatal(code)
			}
			if writes != 1 {
				t.Fatalf("pin %s writes=%d", action, writes)
			}
		})
	}
}

func TestRound5ArchiveAndRestoreHoldLockUntilCommit(t *testing.T) {
	for _, entry := range []string{"archive", "restore"} {
		t.Run(entry, func(t *testing.T) {
			cfg, bundle, plan := archiveFixture(t)
			if entry == "restore" {
				if code := archiveCommand(cfg, []string{bundle, "--apply", "--plan", plan.ID}, io.Discard, io.Discard); code != exitOK {
					t.Fatal(code)
				}
			}
			base := archiveExec
			var done chan int
			archiveExec = func(name string, args []string, env []string) ([]byte, error) {
				out, err := base(name, args, env)
				if done == nil {
					done = make(chan int, 1)
					go func() { done <- pinCommand(cfg, []string{"add", bundle, "--reason", "retain"}, io.Discard, io.Discard) }()
					select {
					case code := <-done:
						t.Errorf("pin bypassed %s lock: code=%d", entry, code)
					case <-time.After(50 * time.Millisecond):
					}
				}
				return out, err
			}
			var code int
			if entry == "archive" {
				code = archiveCommand(cfg, []string{bundle, "--apply", "--plan", plan.ID}, io.Discard, io.Discard)
			} else {
				code = restoreCommand(cfg, []string{bundle}, io.Discard, io.Discard)
			}
			if code != exitOK {
				t.Fatalf("%s code=%d", entry, code)
			}
			if done == nil {
				t.Fatal("restic was not called")
			}
			select {
			case code := <-done:
				if code != exitOK {
					t.Fatal(code)
				}
			case <-time.After(4 * time.Second):
				t.Fatal("pin did not acquire released lock")
			}
		})
	}
}

func TestRound5EvidencePutRollbackRestoresTree(t *testing.T) {
	for _, role := range []string{"receipt", "blob"} {
		t.Run(role, func(t *testing.T) {
			oldOpen := safetyOpenPaths
			safetyOpenPaths = func() ([]string, error) { return nil, nil }
			t.Cleanup(func() { safetyOpenPaths = oldOpen })
			cfg := testConfig(t.TempDir())
			var out bytes.Buffer
			if code := evidenceCommand(cfg, []string{"new", "alpha", "--goal", "goal", "--criterion", "proof"}, &out, io.Discard); code != exitOK {
				t.Fatal(code)
			}
			bundle := strings.TrimSpace(out.String())
			input := filepath.Join(t.TempDir(), "proof")
			putFile(t, input, "payload")
			before := snapshotStateTree(t, cfg.Root)
			base := writeStateBytes
			writeStateBytes = func(path string, data []byte, mode fs.FileMode) error {
				if path == filepath.Join(bundle, "manifest.json") {
					return errors.New("injected manifest failure")
				}
				return base(path, data, mode)
			}
			t.Cleanup(func() { writeStateBytes = base })
			if code := evidenceCommand(cfg, []string{"put", bundle, input, "--role", role}, io.Discard, io.Discard); code == exitOK {
				t.Fatal("manifest failure ignored")
			}
			if !maps.Equal(before, snapshotStateTree(t, cfg.Root)) {
				t.Fatal("failed put left payload or scaffolding")
			}
		})
	}
}

func TestRound5MutationsFailClosedWithoutProcessScan(t *testing.T) {
	for _, entry := range []string{"evidence-new", "scratch-release"} {
		t.Run(entry, func(t *testing.T) {
			cfg := testConfig(t.TempDir())
			if entry == "scratch-release" {
				cfg, _ = expiredScratch(t)
			}
			before := snapshotStateTree(t, cfg.Root)
			saved := safetyOpenPaths
			safetyOpenPaths = func() ([]string, error) { return nil, errors.New("injected unavailable process scan") }
			t.Cleanup(func() { safetyOpenPaths = saved })
			var code int
			if entry == "evidence-new" {
				code = evidenceCommand(cfg, []string{"new", "alpha", "--goal", "goal", "--criterion", "proof"}, io.Discard, io.Discard)
			} else {
				code = scratchCommand(cfg, []string{"release", "lease-expired"}, io.Discard, io.Discard)
			}
			if code != exitBlocked || !maps.Equal(before, snapshotStateTree(t, cfg.Root)) {
				t.Fatalf("unavailable process scan did not fail closed: code=%d", code)
			}
		})
	}
}

func TestRound5BlockedCommandsLeaveAbsentRootAbsent(t *testing.T) {
	for _, entry := range []string{"evidence", "scratch", "pin", "gc"} {
		t.Run(entry, func(t *testing.T) {
			parent := t.TempDir()
			cfg := testConfig(filepath.Join(parent, "absent"))
			before := snapshotStateTree(t, parent)
			var code int
			switch entry {
			case "evidence":
				code = evidenceCommand(cfg, []string{"put", "invalid"}, io.Discard, io.Discard)
			case "scratch":
				code = scratchCommand(cfg, []string{"release", "lease-missing"}, io.Discard, io.Discard)
			case "pin":
				code = pinCommand(cfg, []string{"rm", filepath.Join(cfg.Root, "missing")}, io.Discard, io.Discard)
			case "gc":
				code = applyGC(cfg, "plan-missing", plansPath(cfg), io.Discard, io.Discard)
			}
			if code == exitOK {
				t.Fatal("invalid command accepted")
			}
			if !maps.Equal(before, snapshotStateTree(t, parent)) {
				t.Fatal("rejected command changed filesystem")
			}
		})
	}
}

func TestRound5GitHistoryProtectsUniqueTags(t *testing.T) {
	root := t.TempDir()
	gitFixtureCommand(t, root, "init", "-q", "-b", "main")
	putFile(t, filepath.Join(root, "data"), "seed")
	gitFixtureCommand(t, root, "add", "data")
	gitFixtureCommand(t, root, "commit", "-q", "-m", "seed")
	gitFixtureCommand(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
	gitFixtureCommand(t, root, "checkout", "-q", "--detach")
	putFile(t, filepath.Join(root, "data"), "unique tag payload")
	gitFixtureCommand(t, root, "add", "data")
	gitFixtureCommand(t, root, "commit", "-q", "-m", "tagged payload")
	gitFixtureCommand(t, root, "tag", "retain")
	gitFixtureCommand(t, root, "checkout", "-q", "main")
	reasons, err := gitHistoryReasons(root)
	if err != nil || !contains(reasons, "git_unique_commits") {
		t.Fatalf("unique tagged history ignored: %v %v", reasons, err)
	}
}

func TestRound5ConfigCanonicalizesLocalRoots(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(parent, "alias")); err != nil {
		t.Fatal(err)
	}
	putFile(t, filepath.Join(parent, "config.json"), `{"agent_state":{"root":"alias/state","pinned_paths":["alias/pin"],"archive":{"repo":"alias/archive"}}}`)
	t.Chdir(parent)
	cfg, err := loadConfig(func(key string) string {
		switch key {
		case "HOME":
			return "alias"
		case "AGENT_DRY_CONFIG":
			return "config.json"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Root != filepath.Join(resolved, "state") || cfg.Archive.Repo != filepath.Join(resolved, "archive") || len(cfg.PinnedPaths) != 1 || cfg.PinnedPaths[0] != filepath.Join(resolved, "pin") {
		t.Fatalf("roots not canonical: %+v", cfg)
	}
}

func TestRound5GCReceiptFailureRollsBackTree(t *testing.T) {
	cfg, _ := expiredScratch(t)
	plan, err := buildPlan(cfg, "scratch")
	if err != nil {
		t.Fatal(err)
	}
	if err := persistPlan(plan, plansPath(cfg)); err != nil {
		t.Fatal(err)
	}
	before := snapshotStateTree(t, cfg.Root)
	base := writeStateBytes
	writeStateBytes = func(path string, data []byte, mode fs.FileMode) error {
		if strings.Contains(path, string(filepath.Separator)+"receipts"+string(filepath.Separator)) {
			return errors.New("injected receipt failure")
		}
		return base(path, data, mode)
	}
	t.Cleanup(func() { writeStateBytes = base })
	if code := applyGC(cfg, plan.ID, plansPath(cfg), io.Discard, io.Discard); code != exitEnv {
		t.Fatal(code)
	}
	if !maps.Equal(before, snapshotStateTree(t, cfg.Root)) {
		t.Fatal("failed GC left moved scratch or metadata")
	}
}

func TestRound5ScratchIndexFailureRestoresAbsentAncestors(t *testing.T) {
	parent := t.TempDir()
	cfg := testConfig(filepath.Join(parent, "missing", "state"))
	before := snapshotStateTree(t, parent)
	oldOpen := safetyOpenPaths
	safetyOpenPaths = func() ([]string, error) { return nil, nil }
	t.Cleanup(func() { safetyOpenPaths = oldOpen })
	base := writeStateBytes
	writeStateBytes = func(path string, data []byte, mode fs.FileMode) error {
		if path == indexPath(cfg) {
			return errors.New("injected index failure")
		}
		return base(path, data, mode)
	}
	t.Cleanup(func() { writeStateBytes = base })
	if code := scratchCommand(cfg, []string{"new", "alpha"}, io.Discard, io.Discard); code != exitEnv {
		t.Fatal(code)
	}
	if !maps.Equal(before, snapshotStateTree(t, parent)) {
		t.Fatal("failed scratch creation left ancestors")
	}
}

func TestRound5GCRechecksGitHistoryAtEachMove(t *testing.T) {
	cfg, candidate := expiredScratch(t)
	putFile(t, filepath.Join(candidate, ".git", "HEAD"), "fixture")
	oldRepo, oldHistory := safetyRepoState, safetyGitHistory
	t.Cleanup(func() { safetyRepoState, safetyGitHistory = oldRepo, oldHistory })
	safetyRepoState = func(string) ([]repoStateRecord, error) {
		upstream, ahead := "origin/main", 0
		return []repoStateRecord{{Upstream: &upstream, Ahead: &ahead}}, nil
	}
	safetyGitHistory = func(string) ([]string, error) { return nil, nil }
	plan, err := buildPlan(cfg, "scratch")
	if err != nil || len(plan.Candidates) != 1 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	if err := persistPlan(plan, plansPath(cfg)); err != nil {
		t.Fatal(err)
	}
	scans := 0
	safetyGitHistory = func(string) ([]string, error) {
		scans++
		if scans > 1 {
			return []string{"git_unique_commits"}, nil
		}
		return nil, nil
	}
	if code := applyGC(cfg, plan.ID, plansPath(cfg), io.Discard, io.Discard); code != exitBlocked {
		t.Fatalf("late unique history ignored: code=%d scans=%d", code, scans)
	}
	if _, err := os.Stat(candidate); err != nil {
		t.Fatal(err)
	}
}

func TestRound5GCBlockedDestinationLeavesExactTree(t *testing.T) {
	cfg, _ := expiredScratch(t)
	idx, err := loadIndex(cfg)
	if err != nil {
		t.Fatal(err)
	}
	second := idx.Leases[0]
	second.ID = "lease-second"
	second.Project = "beta"
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
	rel, _ := filepath.Rel(cfg.Root, plan.Candidates[1].Path)
	putFile(t, rootPath(cfg, "_registry", "trash", plan.ID, rel, "existing"), "preserve")
	before := snapshotStateTree(t, cfg.Root)
	if code := applyGC(cfg, plan.ID, plansPath(cfg), io.Discard, io.Discard); code != exitBlocked {
		t.Fatal(code)
	}
	after := snapshotStateTree(t, cfg.Root)
	if !maps.Equal(before, after) {
		var added []string
		for p := range after {
			if _, ok := before[p]; !ok {
				added = append(added, p)
			}
		}
		t.Fatalf("blocked GC changed tree; added=%v", added)
	}
}

func TestRound5GCRechecksLockPIDAtEachMove(t *testing.T) {
	cfg, candidate := expiredScratch(t)
	lock := filepath.Join(candidate, "process.lock")
	putFile(t, lock, "999999999")
	plan, err := buildPlan(cfg, "scratch")
	if err != nil || len(plan.Candidates) != 1 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	if err := persistPlan(plan, plansPath(cfg)); err != nil {
		t.Fatal(err)
	}
	scans := 0
	safetyOpenPaths = func() ([]string, error) {
		scans++
		if scans == 2 {
			putFile(t, lock, fmt.Sprint(os.Getpid()))
		}
		return nil, nil
	}
	if code := applyGC(cfg, plan.ID, plansPath(cfg), io.Discard, io.Discard); code != exitBlocked {
		t.Fatalf("late PID lock ignored: code=%d", code)
	}
	if _, err := os.Stat(candidate); err != nil {
		t.Fatal(err)
	}
}
