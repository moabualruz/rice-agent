package main

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReferenceProtectionIsBundleScoped(t *testing.T) {
	cfg, bundles := migrationFixture(t, "bundle-a")
	bundle := bundles[0]
	outside := t.TempDir()
	alias := filepath.Join(outside, "alias")
	if err := os.Symlink(bundle, alias); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(outside, "dangling")
	if err := os.Symlink(filepath.Join(outside, "missing"), dangling); err != nil {
		t.Fatal(err)
	}
	targetDangling := filepath.Join(outside, "target-dangling")
	if err := os.Symlink(filepath.Join(bundle, "missing"), targetDangling); err != nil {
		t.Fatal(err)
	}
	relativeDangling := filepath.Join(outside, "relative-dangling")
	relativeTarget, err := filepath.Rel(outside, filepath.Join(bundle, "missing"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(relativeTarget, relativeDangling); err != nil {
		t.Fatal(err)
	}
	cycle := filepath.Join(outside, "cycle")
	if err := os.Symlink("cycle", cycle); err != nil {
		t.Fatal(err)
	}
	insideDangling := filepath.Join(bundle, "dangling")
	if err := os.Symlink(filepath.Join(outside, "missing"), insideDangling); err != nil {
		t.Fatal(err)
	}
	lockedOutside := filepath.Join(outside, "locked")
	lockedInside := filepath.Join(bundle, "locked")
	for _, dir := range []string{lockedOutside, lockedInside} {
		if err := os.Mkdir(dir, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	}
	lockedAlias := filepath.Join(outside, "locked-alias")
	if err := os.Symlink(lockedInside, lockedAlias); err != nil {
		t.Fatal(err)
	}
	longName := strings.Repeat("x", 300)
	cases := []struct {
		name, reference string
		want            bool
	}{
		{"root-cwd", cfg.Root, false},
		{"project-cwd", filepath.Dir(filepath.Dir(bundle)), false},
		{"evidence-cwd", filepath.Dir(bundle), false},
		{"bundle-cwd", bundle, true},
		{"open-file", filepath.Join(bundle, "logs", "run.log"), true},
		{"sibling-prefix", bundle + "-other/log", false},
		{"unrelated-long-path", filepath.Join(outside, longName), false},
		{"unrelated-inaccessible-child", filepath.Join(lockedOutside, "child"), false},
		{"inaccessible-alias-child", filepath.Join(lockedAlias, "child"), true},
		{"unrelated-dangling-link", dangling, false},
		{"unrelated-symlink-cycle", cycle, false},
		{"dangling-alias-into-bundle", targetDangling, true},
		{"dangling-alias-into-bundle-child", filepath.Join(targetDangling, "child"), true},
		{"relative-dangling-alias-into-bundle", relativeDangling, true},
		{"matching-long-path", filepath.Join(bundle, longName), true},
		{"matching-dangling-link", insideDangling, true},
		{"symlink-alias", filepath.Join(alias, "logs", "run.log"), true},
		{"unresolvable-alias-child", filepath.Join(alias, longName), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pathInUse(bundle, []string{tc.reference}); got != tc.want {
				t.Fatalf("open path match=%v, want %v", got, tc.want)
			}
			if got := ledgerReferencesPath(map[string]bool{tc.reference: true}, bundle); got != tc.want {
				t.Fatalf("ledger path match=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestLedgerURIProtectionIncludesProject(t *testing.T) {
	cfg, bundles := migrationFixture(t, "bundle-a")
	for _, tc := range []struct {
		reference string
		want      bool
	}{
		{"agent-state://alpha/bundle-a", true},
		{"agent-state://alpha/bundle-a/logs/run.log", true},
		{"agent-state://beta/bundle-a/logs/run.log", false},
		{"agent-state://alpha/bundle-a-other", false},
	} {
		if got := ledgerReferencesPath(map[string]bool{tc.reference: true}, bundles[0]); got != tc.want {
			t.Fatalf("URI %q match=%v, want %v", tc.reference, got, tc.want)
		}
	}
	if ledgerReferencesPath(map[string]bool{"agent-state://alpha/bundle-a": true}, rootPath(cfg, "alpha", "scratch", "bundle-a")) {
		t.Fatal("evidence URI protected an unrelated scratch lease")
	}
}

func TestLedgerProtectionHonorsGoalLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name       string
		fields     map[string]any
		wantActive bool
	}{
		{"closed-status", map[string]any{"status": "closed"}, false},
		{"completed-status", map[string]any{"status": "completed"}, false},
		{"archived-status", map[string]any{"status": "archived"}, false},
		{"closed-criteria", map[string]any{"criteria": []any{map[string]any{"status": "closed"}, map[string]any{"status": "deferred"}}}, false},
		{"open-criterion", map[string]any{"criteria": []any{map[string]any{"status": "closed"}, map[string]any{"status": "ready"}}}, true},
		{"running-attempt", map[string]any{"criteria": []any{map[string]any{"status": "closed"}}, "attempts": []any{map[string]any{"result": "running"}}}, true},
		{"unknown-status", map[string]any{"status": "unknown"}, true},
		{"legacy-no-status", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, bundles := migrationFixture(t, "bundle-a")
			doc := map[string]any{"goal_id": "legacy-goal", "updated_at": time.Now().UTC().Format(time.RFC3339), "evidence": bundles[0]}
			for key, value := range tc.fields {
				doc[key] = value
			}
			if err := writeJSONAtomic(rootPath(cfg, "alpha", "legacy-goal.json"), doc, 0o600); err != nil {
				t.Fatal(err)
			}
			if got := activeLedgerReferences(cfg, bundles[0]); got != tc.wantActive {
				t.Fatalf("active=%v, want %v", got, tc.wantActive)
			}
		})
	}
}

func TestMigrateScopesUnresolvableReferences(t *testing.T) {
	cfg, bundles := migrationFixture(t, "bundle-a", "bundle-b")
	unrelated := filepath.Join(t.TempDir(), strings.Repeat("x", 300))
	// A real legacy ledger contains a path followed by prose with this error class.
	putMigrationLedger(t, cfg, "legacy-goal", []string{bundles[0], bundles[1], unrelated}, "2020-01-03T00:00:00Z")
	if err := writeJSONAtomic(rootPath(cfg, "alpha", "recent-goal.json"), map[string]any{"updated_at": time.Now().UTC().Format(time.RFC3339), "evidence": unrelated, "cwd": cfg.Root}, 0o600); err != nil {
		t.Fatal(err)
	}
	safetyOpenPaths = func() ([]string, error) {
		return []string{cfg.Root, filepath.Dir(filepath.Dir(bundles[0])), unrelated}, nil
	}
	before := snapshotStateTree(t, cfg.Root)
	plan, _, _ := reviewedMigrationPlan(t, cfg)
	if len(plan.Candidates) != 2 || len(plan.Blocked) != 0 {
		t.Fatalf("unrelated resolution failure blocked migration: %+v", plan)
	}
	if !maps.Equal(before, snapshotStateTree(t, cfg.Root)) {
		t.Fatal("dry run changed legacy state")
	}
}
