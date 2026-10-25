package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func completionBlob(t *testing.T, cfg config) fileRecord {
	t.Helper()
	source := filepath.Join(t.TempDir(), "blob")
	putFile(t, source, "ordinary evidence payload")
	sum, n, err := hashFile(source)
	if err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join("_cas", "sha256", sum[:2], sum)
	putFile(t, rootPath(cfg, rel), "ordinary evidence payload")
	return fileRecord{Path: filepath.ToSlash(rel), SHA256: sum, Bytes: n, Role: "blob"}
}

func TestCompletionBlobScanBlocksPlanAndArchive(t *testing.T) {
	for _, scannerError := range []bool{false, true} {
		t.Run(map[bool]string{false: "finding", true: "scanner-error"}[scannerError], func(t *testing.T) {
			cfg := testConfig(t.TempDir())
			cfg.Archive.Repo = filepath.Join(t.TempDir(), "archive")
			cfg.Archive.PasswordRef = "fixture/password#key"
			stubDir := t.TempDir()
			for _, name := range []string{"restic", "bao-run"} {
				path := filepath.Join(stubDir, name)
				putFile(t, path, "#!/bin/sh\nexit 99\n")
				if err := os.Chmod(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			blob := completionBlob(t, cfg)
			bundle := rootPath(cfg, "alpha", "evidence", "bundle")
			m := evidenceManifest{Schema: evidenceSchema, ID: "bundle", Project: "alpha", Status: "closed", ClosedAt: time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339), Blobs: []fileRecord{blob}}
			if err := writeJSONAtomic(filepath.Join(bundle, "manifest.json"), m, 0o600); err != nil {
				t.Fatal(err)
			}
			oldScan, oldOpen, oldExec := scanSecrets, safetyOpenPaths, archiveExec
			t.Cleanup(func() { scanSecrets, safetyOpenPaths, archiveExec = oldScan, oldOpen, oldExec })
			scanned := 0
			scanSecrets = func(path string) (bool, error) {
				if path == rootPath(cfg, filepath.FromSlash(blob.Path)) {
					scanned++
					if scannerError {
						return false, errors.New("scanner unavailable")
					}
					return true, nil
				}
				return false, nil
			}
			safetyOpenPaths = func() ([]string, error) { return nil, nil }
			archiveExec = func(string, []string, []string) ([]byte, error) { t.Fatal("backup must not run"); return nil, nil }
			plan, err := buildPlan(cfg, "evidence")
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Candidates) != 0 || len(plan.Blocked) != 1 || scanned != 1 {
				t.Fatalf("plan=%+v scans=%d", plan, scanned)
			}
			// A previously authorized plan must also re-scan blobs at apply time.
			entry := plan.Blocked[0]
			entry.Reasons = nil
			plan.Blocked = nil
			plan.Candidates = []planEntry{entry}
			if err := sealPlan(&plan); err != nil {
				t.Fatal(err)
			}
			if err := persistPlan(plan, plansPath(cfg)); err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			if code := archiveCommand(cfg, []string{bundle, "--apply", "--plan", plan.ID}, io.Discard, &stderr); code != exitBlocked || scanned != 2 {
				t.Fatalf("code=%d scans=%d stderr=%s", code, scanned, stderr.String())
			}
		})
	}
}

func TestCompletionRestoreCASIdempotentAndRejectsUnsafeFiles(t *testing.T) {
	for _, kind := range []string{"matching", "different", "symlink", "parent-symlink", "missing"} {
		t.Run(kind, func(t *testing.T) {
			cfg := testConfig(t.TempDir())
			blob := completionBlob(t, cfg)
			dst := rootPath(cfg, filepath.FromSlash(blob.Path))
			restore := t.TempDir()
			src := filepath.Join(restore, strings.TrimLeft(dst, string(filepath.Separator)))
			putFile(t, src, "ordinary evidence payload")
			switch kind {
			case "different":
				putFile(t, dst, "corrupted")
			case "symlink":
				if err := os.Remove(dst); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(src, dst); err != nil {
					t.Fatal(err)
				}
			case "parent-symlink":
				if err := os.Remove(dst); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Dir(dst)); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Dir(src), filepath.Dir(dst)); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(dst); err != nil {
					t.Fatal(err)
				}
			}
			err := restoreCASBlobs(cfg, restore, []fileRecord{blob})
			if kind == "matching" || kind == "missing" {
				if err != nil {
					t.Fatal(err)
				}
				if err := restoreCASBlobs(cfg, restore, []fileRecord{blob}); err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("unsafe or mismatching blob accepted")
			}
		})
	}
}

func TestCompletionCreationRejectsSymlinkParents(t *testing.T) {
	for _, kind := range []string{"scratch", "evidence", "ledger", "blob", "receipt", "close"} {
		t.Run(kind, func(t *testing.T) {
			cfg := testConfig(t.TempDir())
			external := t.TempDir()
			project := rootPath(cfg, "alpha")
			if err := os.MkdirAll(project, 0o700); err != nil {
				t.Fatal(err)
			}
			var args []string
			var command func(config, []string, io.Writer, io.Writer) int
			switch kind {
			case "scratch":
				if err := os.Symlink(external, filepath.Join(project, "scratch")); err != nil {
					t.Fatal(err)
				}
				command, args = scratchCommand, []string{"new", "alpha"}
			case "evidence":
				if err := os.Symlink(external, filepath.Join(project, "evidence")); err != nil {
					t.Fatal(err)
				}
				command, args = evidenceCommand, []string{"new", "alpha", "--goal", "goal", "--criterion", "proof"}
			case "ledger":
				if err := os.Remove(project); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(external, project); err != nil {
					t.Fatal(err)
				}
				command, args = ledgerCommand, []string{"path", "alpha", "goal"}
			default:
				var out bytes.Buffer
				if code := evidenceCommand(cfg, []string{"new", "alpha", "--goal", "goal", "--criterion", "proof"}, &out, io.Discard); code != exitOK {
					t.Fatal(code)
				}
				bundle := strings.TrimSpace(out.String())
				src := filepath.Join(t.TempDir(), "input")
				putFile(t, src, "payload")
				if kind == "blob" {
					if err := os.Symlink(external, rootPath(cfg, "_cas")); err != nil {
						t.Fatal(err)
					}
					command, args = evidenceCommand, []string{"put", bundle, src, "--role", "blob"}
				} else if kind == "receipt" {
					if err := os.Symlink(external, filepath.Join(bundle, "receipts")); err != nil {
						t.Fatal(err)
					}
					command, args = evidenceCommand, []string{"put", bundle, src, "--role", "receipt"}
				} else {
					// readManifest checks the leaf, so a symlinked project parent must be checked too.
					moved := filepath.Join(external, "alpha")
					if err := os.Rename(project, moved); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(moved, project); err != nil {
						t.Fatal(err)
					}
					command, args = evidenceCommand, []string{"close", bundle, "--result", "pass"}
				}
			}
			before, err := treeFingerprint(external)
			if err != nil {
				t.Fatal(err)
			}
			if code := command(cfg, args, io.Discard, io.Discard); code == exitOK {
				t.Fatal("symlinked creation parent accepted")
			}
			after, err := treeFingerprint(external)
			if err != nil || before != after {
				t.Fatal("external content changed")
			}
		})
	}
}

func TestCompletionReleasedScratchCollectedAfterTTL(t *testing.T) {
	cfg, path := expiredScratch(t)
	idx, err := loadIndex(cfg)
	if err != nil {
		t.Fatal(err)
	}
	id := idx.Leases[0].ID
	if code := scratchCommand(cfg, []string{"release", id}, io.Discard, io.Discard); code != exitOK {
		t.Fatal(code)
	}
	plan, err := buildPlan(cfg, "scratch")
	if err != nil || len(plan.Candidates) != 1 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	if err := persistPlan(plan, plansPath(cfg)); err != nil {
		t.Fatal(err)
	}
	if code := applyGC(cfg, plan.ID, plansPath(cfg), io.Discard, io.Discard); code != exitOK {
		t.Fatal(code)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scratch remains: %v", err)
	}
	idx, err = loadIndex(cfg)
	if err != nil || idx.Leases[0].CollectedAt == "" {
		t.Fatalf("index=%+v err=%v", idx, err)
	}
	next, err := buildPlan(cfg, "scratch")
	if err != nil || len(next.Candidates) != 0 || len(next.Blocked) != 0 {
		t.Fatalf("collected lease reconsidered: %+v %v", next, err)
	}
}
