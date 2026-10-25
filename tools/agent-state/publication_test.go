package main

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateAtomicDoesNotReplaceAtPublication(t *testing.T) {
	for _, kind := range []string{"file", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "manifest.json")
			outside := filepath.Join(t.TempDir(), "outside")
			putFile(t, outside, "outside content")
			base := linkStatePath
			t.Cleanup(func() { linkStatePath = base })
			var peer fs.FileInfo
			linkStatePath = func(source, target string) error {
				data, err := os.ReadFile(source)
				if err != nil || string(data) != "new manifest" {
					t.Fatalf("publication did not receive a complete file: %q %v", data, err)
				}
				switch kind {
				case "file":
					putFile(t, target, "peer manifest")
				case "symlink":
					err = os.Symlink(outside, target)
				case "directory":
					err = os.Mkdir(target, 0o700)
				}
				if err != nil {
					t.Fatal(err)
				}
				peer, err = os.Lstat(target)
				if err != nil {
					t.Fatal(err)
				}
				return base(source, target)
			}
			info, err := createBytesAtomic(path, []byte("new manifest"), 0o600)
			if !errors.Is(err, os.ErrExist) || info != nil || peer == nil {
				t.Fatalf("create replaced an existing destination: info=%v err=%v", info, err)
			}
			actual, err := os.Lstat(path)
			if err != nil || !os.SameFile(peer, actual) {
				t.Fatal("concurrent destination identity changed")
			}
			if kind == "file" {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != "peer manifest" {
					t.Fatal("concurrent file content changed")
				}
			}
			data, err := os.ReadFile(outside)
			if err != nil || string(data) != "outside content" {
				t.Fatal("followed a concurrent symlink")
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 || entries[0].Name() != "manifest.json" {
				t.Fatalf("failed publication leaked temporary files: %v %v", entries, err)
			}
		})
	}
}

func TestMigrateConcurrentManifestPublication(t *testing.T) {
	for _, failReceipt := range []bool{false, true} {
		name := "success"
		if failReceipt {
			name = "later-rollback"
		}
		t.Run(name, func(t *testing.T) {
			cfg, bundles := migrationFixture(t, "bundle-a", "bundle-b", "bundle-c")
			plan, directory, _ := reviewedMigrationPlan(t, cfg)
			before := snapshotStateTree(t, cfg.Root)
			peerPath := filepath.Join(bundles[1], "manifest.json")
			const content = "concurrent writer's manifest\n"
			var peer fs.FileInfo
			baseLink, baseCreate := linkStatePath, createStateBytes
			t.Cleanup(func() { linkStatePath, createStateBytes = baseLink, baseCreate })
			linkStatePath = func(source, target string) error {
				if target == peerPath {
					putFile(t, target, content)
					var err error
					peer, err = os.Lstat(target)
					if err != nil {
						t.Fatal(err)
					}
				}
				return baseLink(source, target)
			}
			createStateBytes = func(path string, data []byte, mode fs.FileMode) (fs.FileInfo, error) {
				info, err := baseCreate(path, data, mode)
				if err == nil && failReceipt && strings.HasSuffix(path, "-migrate.json") {
					return info, errors.New("injected failure after receipt publication")
				}
				return info, err
			}
			var out bytes.Buffer
			code := migrateCommand(cfg, []string{"--apply", "--plan", plan.ID, "--plan-dir", directory}, &out, io.Discard)
			want := exitOK
			if failReceipt {
				want = exitEnv
			}
			if code != want || peer == nil || !strings.Contains(out.String(), "reason=concurrent_manifest") {
				t.Fatalf("concurrent manifest not skipped: code=%d out=%q", code, out.String())
			}
			actual, err := os.Lstat(peerPath)
			data, readErr := os.ReadFile(peerPath)
			if err != nil || readErr != nil || !os.SameFile(peer, actual) || string(data) != content {
				t.Fatal("concurrent manifest was overwritten or removed")
			}
			if failReceipt {
				expected := maps.Clone(before)
				rel, _ := filepath.Rel(cfg.Root, peerPath)
				expected[rel] = peer.Mode().String() + content
				if !maps.Equal(expected, snapshotStateTree(t, cfg.Root)) {
					t.Fatal("rollback changed peer data or left owned manifests and scaffolding")
				}
				return
			}
			if !strings.Contains(out.String(), "migrated=2 skipped=1") {
				t.Fatalf("incorrect migration counts: %q", out.String())
			}
			var receipt struct {
				Items   []planEntry `json:"items"`
				Skipped []planEntry `json:"skipped"`
			}
			if err := readJSONFile(rootPath(cfg, "_registry", "receipts", plan.ID+"-migrate.json"), &receipt); err != nil {
				t.Fatal(err)
			}
			if len(receipt.Items) != 2 || len(receipt.Skipped) != 1 || receipt.Skipped[0].Path != bundles[1] || !contains(receipt.Skipped[0].Reasons, "concurrent_manifest") {
				t.Fatalf("receipt claimed a skipped manifest: %+v", receipt)
			}
			for _, bundle := range []string{bundles[0], bundles[2]} {
				if _, err := readManifest(bundle); err != nil {
					t.Fatalf("safe candidate was not migrated: %s %v", bundle, err)
				}
			}
		})
	}
}

func TestCreateJSONRollbackPreservesConcurrentReplacement(t *testing.T) {
	cfg := testConfig(filepath.Join(t.TempDir(), "state"))
	tx, err := beginStateMutation(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := rootPath(cfg, "alpha", "evidence", "bundle", "manifest.json")
	if err := tx.createJSON(path, map[string]string{"owner": "transaction"}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeBytesAtomic(path, []byte("peer replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	peer, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.rollback(); err != nil {
		t.Fatal(err)
	}
	actual, err := os.Lstat(path)
	data, readErr := os.ReadFile(path)
	if err != nil || readErr != nil || !os.SameFile(peer, actual) || string(data) != "peer replacement" {
		t.Fatal("rollback removed a replacement it did not publish")
	}
}

func TestCreateOnlyCallersPreserveConcurrentFiles(t *testing.T) {
	for _, command := range []string{"evidence-new", "migrate-receipt", "gc-receipt"} {
		t.Run(command, func(t *testing.T) {
			var cfg config
			var invoke func() int
			peerPath := ""
			switch command {
			case "evidence-new":
				cfg, _ = migrationFixture(t)
				invoke = func() int {
					return evidenceCommand(cfg, []string{"new", "alpha", "--goal", "legacy-goal", "--criterion", "proof"}, io.Discard, io.Discard)
				}
			case "migrate-receipt":
				cfg, _ = migrationFixture(t, "bundle-a")
				plan, directory, _ := reviewedMigrationPlan(t, cfg)
				peerPath = rootPath(cfg, "_registry", "receipts", plan.ID+"-migrate.json")
				invoke = func() int { return applyReviewedMigration(cfg, plan, directory) }
			case "gc-receipt":
				cfg, _ = expiredScratch(t)
				plan, err := buildPlan(cfg, "scratch")
				if err != nil {
					t.Fatal(err)
				}
				if err := persistPlan(plan, plansPath(cfg)); err != nil {
					t.Fatal(err)
				}
				peerPath = rootPath(cfg, "_registry", "receipts", plan.ID+"-gc.json")
				invoke = func() int { return applyGC(cfg, plan.ID, plansPath(cfg), io.Discard, io.Discard) }
			}
			before := snapshotStateTree(t, cfg.Root)
			base := linkStatePath
			t.Cleanup(func() { linkStatePath = base })
			var peer fs.FileInfo
			linkStatePath = func(source, target string) error {
				if target == peerPath || command == "evidence-new" && filepath.Base(target) == "manifest.json" {
					peerPath = target
					putFile(t, target, "peer content")
					var err error
					peer, err = os.Lstat(target)
					if err != nil {
						t.Fatal(err)
					}
				}
				return base(source, target)
			}
			if code := invoke(); code != exitBlocked || peer == nil {
				t.Fatalf("create-only conflict not blocked: code=%d peer=%v", code, peer)
			}
			actual, err := os.Lstat(peerPath)
			data, readErr := os.ReadFile(peerPath)
			if err != nil || readErr != nil || !os.SameFile(peer, actual) || string(data) != "peer content" {
				t.Fatal("concurrent file was replaced or removed on rollback")
			}
			after := snapshotStateTree(t, cfg.Root)
			for path, value := range before {
				if after[path] != value {
					t.Fatalf("rollback failed to restore original state: %s", path)
				}
			}
			for path := range after {
				if _, existed := before[path]; !existed && !inside(rootPath(cfg, path), peerPath) {
					t.Fatalf("rollback left owned state: %s", path)
				}
			}
		})
	}
}
