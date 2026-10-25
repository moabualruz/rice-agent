package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCompletionRestoredBlobParentSymlinkRejected(t *testing.T) {
	cfg := testConfig(t.TempDir())
	blob := completionBlob(t, cfg)
	restore := t.TempDir()
	src := filepath.Join(restore, strings.TrimLeft(rootPath(cfg, filepath.FromSlash(blob.Path)), string(filepath.Separator)))
	external := t.TempDir()
	putFile(t, filepath.Join(external, filepath.Base(src)), "ordinary evidence payload")
	if err := os.MkdirAll(filepath.Dir(filepath.Dir(src)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Dir(src)); err != nil {
		t.Fatal(err)
	}
	if err := compareRestoredBlobs(cfg, restore, []fileRecord{blob}); err == nil {
		t.Fatal("restored CAS parent symlink accepted")
	}
	if err := os.Remove(rootPath(cfg, filepath.FromSlash(blob.Path))); err != nil {
		t.Fatal(err)
	}
	if err := restoreCASBlobs(cfg, restore, []fileRecord{blob}); err == nil {
		t.Fatal("restored CAS source symlink accepted")
	}
}

func TestCompletionReleasedScratchRetainsTTLProtection(t *testing.T) {
	cfg, _ := expiredScratch(t)
	idx, err := loadIndex(cfg)
	if err != nil {
		t.Fatal(err)
	}
	idx.Leases[0].ExpiresAt = time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
	if err := saveIndex(cfg, idx); err != nil {
		t.Fatal(err)
	}
	if code := scratchCommand(cfg, []string{"release", idx.Leases[0].ID}, io.Discard, io.Discard); code != exitOK {
		t.Fatal(code)
	}
	plan, err := buildPlan(cfg, "scratch")
	if err != nil || len(plan.Candidates) != 0 {
		t.Fatalf("unexpired scratch offered for deletion: %+v %v", plan, err)
	}
}

func TestCompletionGCPreflightsEveryTrashDestination(t *testing.T) {
	cfg, first := expiredScratch(t)
	idx, err := loadIndex(cfg)
	if err != nil {
		t.Fatal(err)
	}
	secondLease := idx.Leases[0]
	secondLease.ID = "lease-second"
	secondLease.Path = rootPath(cfg, secondLease.Project, "scratch", secondLease.ID)
	putFile(t, filepath.Join(secondLease.Path, "data"), "second")
	idx.Leases = append(idx.Leases, secondLease)
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
	rel, err := filepath.Rel(cfg.Root, plan.Candidates[1].Path)
	if err != nil {
		t.Fatal(err)
	}
	putFile(t, rootPath(cfg, "_registry", "trash", plan.ID, rel, "existing"), "preserve")
	if code := applyGC(cfg, plan.ID, plansPath(cfg), io.Discard, io.Discard); code != exitBlocked {
		t.Fatal(code)
	}
	for _, path := range []string{first, secondLease.Path} {
		if _, err := os.Stat(filepath.Join(path, "data")); err != nil {
			t.Fatalf("preflight failure moved scratch: %v", err)
		}
	}
}
