package main

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

type archiveTreeEntry struct {
	Mode fs.FileMode
	Data []byte
}

func archiveTreeState(root string) (map[string]archiveTreeEntry, error) {
	state := make(map[string]archiveTreeEntry)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		item := archiveTreeEntry{Mode: info.Mode()}
		if info.Mode().IsRegular() {
			item.Data, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		state[rel] = item
		return nil
	})
	return state, err
}

func round5RetentionFixture(t *testing.T) (config, gcPlan, string, *int) {
	t.Helper()
	cfg, bundle, _ := archiveFixture(t)
	cfg.Archive.RetentionMonths = 12
	snapshots := []resticSnapshot{{ID: "aaaaaaaa", Time: time.Now().AddDate(-2, 0, 0).UTC().Format(time.RFC3339), Tags: []string{"class=evidence"}, Paths: []string{bundle}}}
	raw, err := json.Marshal(snapshots)
	if err != nil {
		t.Fatal(err)
	}
	forgotten := new(int)
	archiveExec = func(_ string, args []string, _ []string) ([]byte, error) {
		switch args[0] {
		case "snapshots":
			return raw, nil
		case "forget":
			*forgotten++
			return nil, nil
		default:
			t.Fatalf("unexpected retention command %q", args[0])
			return nil, errors.New("unexpected retention command")
		}
	}
	plan, err := buildArchiveRetentionPlan(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistPlan(plan, plansPath(cfg)); err != nil {
		t.Fatal(err)
	}
	return cfg, plan, bundle, forgotten
}

func TestRound5RetentionRechecksFullProtection(t *testing.T) {
	for _, phase := range []string{"plan", "apply"} {
		t.Run(phase, func(t *testing.T) {
			cfg, plan, bundle, forgotten := round5RetentionFixture(t)
			if code := pinCommand(cfg, []string{"add", bundle, "--reason", "retain archive"}, io.Discard, io.Discard); code != exitOK {
				t.Fatal(code)
			}
			before, err := archiveTreeState(cfg.Root)
			if err != nil {
				t.Fatal(err)
			}
			if phase == "plan" {
				if _, err := buildArchiveRetentionPlan(cfg); !errors.Is(err, errArchiveBlocked) {
					t.Fatalf("pinned retention plan accepted: %v", err)
				}
			} else if code := archiveCommand(cfg, []string{"--policy", "--apply", "--plan", plan.ID}, io.Discard, io.Discard); code != exitBlocked {
				t.Fatalf("pinned retention apply code=%d", code)
			}
			if *forgotten != 0 {
				t.Fatal("protected snapshot was forgotten")
			}
			after, err := archiveTreeState(cfg.Root)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("blocked retention changed local tree")
			}
		})
	}
}

func TestRound5ArchiveAndRestoreUseFullProtectionChecker(t *testing.T) {
	cfg, bundle, plan := archiveFixture(t)
	oldCheck := protectionCheck
	calls := make(map[string]int)
	protectionCheck = func(cfg config, idx registryIndex, path string) []string {
		calls[path]++
		return oldCheck(cfg, idx, path)
	}
	t.Cleanup(func() { protectionCheck = oldCheck })

	if code := archiveCommand(cfg, []string{bundle, "--apply", "--plan", plan.ID}, io.Discard, io.Discard); code != exitOK {
		t.Fatalf("archive code = %d", code)
	}
	if calls[bundle] < 2 {
		t.Fatalf("archive protection checks = %d, want preflight and final checks", calls[bundle])
	}
	if code := restoreCommand(cfg, []string{bundle}, io.Discard, io.Discard); code != exitOK {
		t.Fatalf("restore code = %d", code)
	}
	if calls[bundle] < 4 {
		t.Fatalf("archive and restore protection checks = %d, want preflight and final checks for both", calls[bundle])
	}
}

func TestRound5ArchiveStubWriteFailureRestoresExactTree(t *testing.T) {
	cfg := testConfig(t.TempDir())
	bundle := rootPath(cfg, "alpha", "evidence", "bundle")
	manifest := evidenceManifest{Schema: evidenceSchema, ID: "bundle", Project: "alpha", Status: "closed"}
	if err := writeJSONAtomic(filepath.Join(bundle, "manifest.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	putFile(t, filepath.Join(bundle, "receipt.json"), `{"result":"pass"}`)
	before, err := archiveTreeState(cfg.Root)
	if err != nil {
		t.Fatal(err)
	}

	writeErr := errors.New("injected partial ARCHIVED.md write")
	oldWrite := writeStateBytes
	writeStateBytes = func(path string, data []byte, mode fs.FileMode) error {
		if filepath.Base(path) == "ARCHIVED.md" {
			if err := os.WriteFile(path, []byte("# partial stub"), mode); err != nil {
				return err
			}
			return writeErr
		}
		return oldWrite(path, data, mode)
	}
	t.Cleanup(func() { writeStateBytes = oldWrite })

	manifest.Archive = &archiveRef{Snapshot: "aaaaaaaa", ManifestSHA256: "manifest-hash"}
	err = stageArchivedBundle(cfg, bundle, &manifest, registryIndex{Schema: indexSchema})
	if !errors.Is(err, writeErr) {
		t.Fatalf("stage error = %v, want injected write failure", err)
	}
	after, err := archiveTreeState(cfg.Root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("archive rollback changed the prior tree\nbefore: %#v\nafter:  %#v", before, after)
	}
}
