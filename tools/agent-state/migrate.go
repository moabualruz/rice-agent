package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

// Keep both the inferred manifest and its ledger input inside the reviewed digest.
type migrationEvidence struct {
	Manifest     evidenceManifest `json:"manifest"`
	LedgerPath   string           `json:"ledger_path"`
	LedgerSHA256 string           `json:"ledger_sha256"`
}

type migrationLedger struct {
	GoalID string
	Path   string
	SHA256 string
	Values []string
}

func migrateCommand(cfg config, args []string, stdout, stderr io.Writer) int {
	o, err := parseOptions(args, []string{"dry-run", "apply"}, []string{"plan", "plan-dir"})
	if err != nil {
		return parseErr(stderr, err)
	}
	if len(o.pos) != 0 {
		return usageError(stderr, "migrate accepts options only")
	}
	if code := checkPlanFlags(o, stderr); code != exitOK {
		return code
	}
	directory, err := resolvePlanDirectory(cfg, o.values["plan-dir"])
	if err != nil {
		return usageError(stderr, "unsafe plan directory")
	}
	return withIndexLock(cfg, func(idx *registryIndex) int {
		if o.bools["apply"] {
			return applyMigrationLocked(cfg, *idx, o.values["plan"], directory, stdout, stderr)
		}
		plan, err := buildMigrationPlanLocked(cfg, *idx)
		if err != nil {
			fmt.Fprintln(stderr, "agent-state: cannot inventory legacy evidence")
			return exitEnv
		}
		if err := persistPlan(plan, directory); err != nil {
			fmt.Fprintln(stderr, "agent-state: cannot save migration plan")
			return exitEnv
		}
		fmt.Fprintf(stdout, "plan_id=%s migrate=%d blocked=%d path=%s\n", plan.ID, len(plan.Candidates), len(plan.Blocked), filepath.Join(directory, plan.ID+".json"))
		for _, entry := range plan.Blocked {
			fmt.Fprintf(stdout, "blocked path=%s reasons=%s\n", entry.Path, strings.Join(entry.Reasons, ","))
		}
		return exitOK
	})
}

func buildMigrationPlanLocked(cfg config, idx registryIndex) (gcPlan, error) {
	plan := gcPlan{Schema: planSchema, Action: "migrate", Root: cfg.Root, CreatedAt: time.Now().UTC().Format(time.RFC3339), Policy: planPolicy{ScratchTTL: cfg.ScratchTTL, EvidenceGrace: cfg.EvidenceGrace, ArchiveRetentionMonths: cfg.Archive.RetentionMonths, ActiveLedgerDays: cfg.ActiveLedgerDays}}
	id, err := newID("plan-")
	if err != nil {
		return plan, err
	}
	plan.ID = id
	add := func(entry planEntry) {
		if len(entry.Reasons) == 0 {
			plan.Candidates = append(plan.Candidates, entry)
		} else {
			plan.Blocked = append(plan.Blocked, entry)
			plan.BlockedBytes += entry.Bytes
		}
	}
	err = visitEvidenceDirs(cfg, "", func(dir string, scanErr error) error {
		if scanErr != nil {
			add(planEntry{Path: dir, Class: "evidence", Action: "write_manifest", Reasons: []string{"unsafe_or_unreadable_directory"}})
			return nil
		}
		children, err := os.ReadDir(dir)
		if err != nil {
			add(planEntry{Path: dir, Class: "evidence", Action: "write_manifest", Reasons: []string{"unreadable_content"}})
			return nil
		}
		ledgers, ledgerErr := migrationLedgers(cfg, filepath.Dir(dir))
		for _, child := range children {
			if !child.IsDir() && child.Type()&fs.ModeSymlink == 0 {
				continue
			}
			bundle := filepath.Join(dir, child.Name())
			if err := validateExistingPath(cfg.Root, bundle); err == nil {
				if _, err := os.Lstat(filepath.Join(bundle, "manifest.json")); err == nil {
					// An existing manifest, including an invalid legacy one, is never overwritten.
					if _, err := readManifest(bundle); err != nil {
						add(planEntry{Path: bundle, Class: "evidence", Action: "write_manifest", Reasons: []string{"invalid_existing_manifest"}})
					}
					continue
				}
			}
			add(migrationEntry(cfg, idx, bundle, ledgers, ledgerErr))
		}
		return nil
	})
	if err != nil {
		return plan, err
	}
	sortEntries(plan.Candidates)
	sortEntries(plan.Blocked)
	if err := sealPlan(&plan); err != nil {
		return plan, err
	}
	return plan, nil
}

func migrationLedgers(cfg config, project string) ([]migrationLedger, error) {
	if err := validateExistingPath(cfg.Root, project); err != nil {
		return nil, err
	}
	children, err := os.ReadDir(project)
	if err != nil {
		return nil, err
	}
	var ledgers []migrationLedger
	for _, child := range children {
		if !strings.HasSuffix(child.Name(), ".json") || strings.HasSuffix(child.Name(), ".checkpoint.json") || child.IsDir() {
			continue
		}
		path := filepath.Join(project, child.Name())
		if err := validateExistingPath(cfg.Root, path); err != nil {
			return nil, err
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			return nil, errors.New("ledger is not a readable regular file")
		}
		raw, err := readStateFile(path)
		if err != nil {
			return nil, err
		}
		var doc map[string]any
		if json.Unmarshal(raw, &doc) != nil {
			return nil, errors.New("invalid legacy ledger")
		}
		goal, ok := doc["goal_id"].(string)
		if !ok {
			continue // Loose JSON is not enough evidence of a goal's identity.
		}
		if !validateName(goal) || goal+".json" != child.Name() {
			return nil, errors.New("legacy ledger identity does not match its path")
		}
		_, values := ledgerFacts(doc, time.Time{}, time.Time{})
		ledgers = append(ledgers, migrationLedger{GoalID: goal, Path: path, SHA256: sha256Sum(raw), Values: values})
	}
	return ledgers, nil
}

func migrationGoal(bundle string, ledgers []migrationLedger) (migrationLedger, string) {
	var matches []migrationLedger
	project := filepath.Base(filepath.Dir(filepath.Dir(bundle)))
	for _, ledger := range ledgers {
		refs := map[string]bool{}
		for _, value := range ledger.Values {
			if strings.HasPrefix(value, "agent-state://") {
				parts := strings.Split(strings.TrimPrefix(value, "agent-state://"), "/")
				if len(parts) >= 2 && parts[0] == project && parts[1] == filepath.Base(bundle) {
					refs[value] = true
				}
			} else if filepath.IsAbs(value) {
				resolved, err := canonicalPath(value)
				if err != nil {
					return migrationLedger{}, "ledger_inference_unavailable"
				}
				refs[resolved] = true
			}
		}
		if ledger.GoalID == filepath.Base(bundle) || ledgerReferencesPath(refs, bundle) {
			matches = append(matches, ledger)
		}
	}
	if len(matches) == 0 {
		return migrationLedger{}, "goal_not_found"
	}
	if len(matches) != 1 {
		return migrationLedger{}, "ambiguous_goal"
	}
	return matches[0], ""
}

func migrationEntry(cfg config, idx registryIndex, bundle string, ledgers []migrationLedger, ledgerErr error) planEntry {
	entry := planEntry{Path: bundle, Class: "evidence", Action: "write_manifest"}
	entry.Reasons = protectionCheck(cfg, idx, bundle)
	project := filepath.Base(filepath.Dir(filepath.Dir(bundle)))
	if !validateName(project) || !validateName(filepath.Base(bundle)) {
		entry.Reasons = append(entry.Reasons, "ambiguous_layout")
	}
	if ledgerErr != nil {
		entry.Reasons = append(entry.Reasons, "ledger_inference_unavailable")
	}
	if len(entry.Reasons) != 0 {
		sort.Strings(entry.Reasons)
		return entry
	}
	if _, err := os.Lstat(filepath.Join(bundle, "manifest.json")); !errors.Is(err, os.ErrNotExist) {
		entry.Reasons = []string{"manifest_exists_or_unreadable"}
		return entry
	}
	ledger, reason := migrationGoal(bundle, ledgers)
	if reason != "" {
		entry.Reasons = []string{reason}
		return entry
	}
	manifest, err := migrationManifest(bundle, project, ledger)
	if err != nil {
		entry.Reasons = []string{"unreadable_content"}
		return entry
	}
	for _, file := range manifest.Files {
		entry.Bytes += file.Bytes
	}
	entry.TreeSHA256, err = treeFingerprint(bundle)
	if err != nil {
		entry.Reasons = append(entry.Reasons, "unreadable_content")
	}
	entry.Reasons = append(entry.Reasons, protectionCheck(cfg, idx, bundle)...)
	entry.Reasons = unique(entry.Reasons)
	sort.Strings(entry.Reasons)
	if len(entry.Reasons) == 0 {
		entry.Reasons = nil
	}
	entry.Migration = &migrationEvidence{Manifest: manifest, LedgerPath: ledger.Path, LedgerSHA256: ledger.SHA256}
	return entry
}

func migrationManifest(bundle, project string, ledger migrationLedger) (evidenceManifest, error) {
	// File age is inventory metadata, never proof that a legacy bundle is closed.
	m := evidenceManifest{Schema: evidenceSchema, Kind: "evidence", ID: filepath.Base(bundle), Project: project, GoalID: ledger.GoalID, Status: "open"}
	var earliest, latest time.Time
	err := walkFiles(bundle, func(path string, entry fs.DirEntry) error {
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		stamp := info.ModTime().UTC()
		if earliest.IsZero() || stamp.Before(earliest) {
			earliest = stamp
		}
		if latest.IsZero() || stamp.After(latest) {
			latest = stamp
		}
		rel, err := filepath.Rel(bundle, path)
		if err != nil || !safeRelativePath(filepath.ToSlash(rel)) {
			return errors.New("unsafe legacy file path")
		}
		role := map[string]string{"receipts": "receipt", "logs": "log", "screenshots": "screenshot", "patches": "patch", "git-bundles": "git-bundle"}[strings.Split(filepath.ToSlash(rel), "/")[0]]
		if role == "" {
			role = "legacy"
		}
		m.Files = append(m.Files, fileRecord{Path: filepath.ToSlash(rel), Role: role})
		return nil
	})
	if err != nil {
		return m, err
	}
	if earliest.IsZero() {
		info, err := os.Stat(bundle)
		if err != nil {
			return m, err
		}
		earliest, latest = info.ModTime().UTC(), info.ModTime().UTC()
	}
	for _, value := range ledger.Values {
		if stamp, err := time.Parse(time.RFC3339Nano, value); err == nil && stamp.After(latest) {
			latest = stamp
		}
	}
	m.CreatedAt, m.LastUsedAt = earliest.Format(time.RFC3339Nano), latest.Format(time.RFC3339Nano)
	if err := sealEvidenceFiles(bundle, &m); err != nil {
		return m, err
	}
	return m, nil
}

func applyMigrationLocked(cfg config, idx registryIndex, id, directory string, stdout, stderr io.Writer) int {
	plan, err := loadPlan(cfg, id, directory)
	if err != nil || plan.Action != "migrate" {
		fmt.Fprintln(stderr, "agent-state: invalid migration plan")
		return exitBlocked
	}
	current, err := buildMigrationPlanLocked(cfg, idx)
	if err != nil || plan.Policy != current.Policy || !reflect.DeepEqual(plan.Candidates, current.Candidates) || !reflect.DeepEqual(plan.Blocked, current.Blocked) {
		fmt.Fprintln(stderr, "agent-state: migration plan no longer matches current state; review a new plan")
		return exitBlocked
	}
	if len(plan.Candidates) == 0 {
		fmt.Fprintln(stdout, "migrated=0")
		return exitOK
	}
	receiptPath := rootPath(cfg, "_registry", "receipts", id+"-migrate.json")
	if err := validateExistingPath(cfg.Root, receiptPath); err != nil {
		return exitBlocked
	}
	if _, err := os.Lstat(receiptPath); !errors.Is(err, os.ErrNotExist) || len(protectionCheck(cfg, idx, receiptPath)) != 0 {
		return exitBlocked
	}
	// All destinations and inputs above are preflighted without creating scaffolding.
	return withStateTransaction(cfg, stderr, func(tx *stateTransaction) int {
		var migrated, skipped []planEntry
		for _, entry := range plan.Candidates {
			ledgers, ledgerErr := migrationLedgers(cfg, filepath.Dir(filepath.Dir(entry.Path)))
			if !reflect.DeepEqual(entry, migrationEntry(cfg, idx, entry.Path, ledgers, ledgerErr)) {
				fmt.Fprintln(stderr, "agent-state: legacy evidence changed during migration")
				return exitBlocked
			}
			if err := tx.createJSON(filepath.Join(entry.Path, "manifest.json"), entry.Migration.Manifest, 0o600); err != nil {
				if errors.Is(err, os.ErrExist) {
					entry.Reasons = []string{"concurrent_manifest"}
					skipped = append(skipped, entry)
					fmt.Fprintf(stdout, "skipped path=%s reason=concurrent_manifest\n", entry.Path)
					continue
				}
				return exitEnv
			}
			migrated = append(migrated, entry)
		}
		if len(migrated) == 0 {
			fmt.Fprintf(stdout, "migrated=0 skipped=%d\n", len(skipped))
			return exitOK
		}
		for _, entry := range migrated {
			if len(protectionCheck(cfg, idx, entry.Path)) != 0 {
				return exitBlocked
			}
		}
		if len(protectionCheck(cfg, idx, receiptPath)) != 0 {
			return exitBlocked
		}
		if err := tx.mkdir(filepath.Dir(receiptPath)); err != nil {
			return exitEnv
		}
		receipt := map[string]any{"plan_id": id, "plan_digest": plan.Digest, "action": "migrate", "completed_at": time.Now().UTC().Format(time.RFC3339), "items": migrated, "skipped": skipped}
		if err := tx.createJSON(receiptPath, receipt, 0o600); err != nil {
			if errors.Is(err, os.ErrExist) {
				fmt.Fprintln(stderr, "agent-state: a concurrent migration receipt appeared; nothing overwritten")
				return exitBlocked
			}
			return exitEnv
		}
		fmt.Fprintf(stdout, "migrated=%d skipped=%d\n", len(migrated), len(skipped))
		return exitOK
	})
}
