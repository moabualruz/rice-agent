package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/moabualruz/agent-dry/internal/shellquote"
)

type resticSnapshot struct {
	ID    string   `json:"id"`
	Time  string   `json:"time"`
	Tags  []string `json:"tags"`
	Paths []string `json:"paths"`
}
type resticMessage struct {
	SnapshotID  string `json:"snapshot_id"`
	MessageType string `json:"message_type"`
}

var archiveExec = func(name string, args []string, env []string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, errors.New("restic command failed")
	}
	return out, nil
}

var safeSecretRef = regexp.MustCompile(`^[A-Za-z0-9._/-]+#[A-Za-z0-9._-]+$`)

func resticEnvironment(cfg config) ([]string, error) {
	if cfg.Archive.Repo == "" {
		return nil, errors.New("archive repository is not configured")
	}
	if !safeSecretRef.MatchString(cfg.Archive.PasswordRef) {
		return nil, errors.New("archive password_ref is not configured")
	}
	parts := strings.SplitN(cfg.Archive.PasswordRef, "#", 2)
	bao, err := exec.LookPath("bao-run")
	if err != nil {
		return nil, errors.New("bao-run is unavailable")
	}
	if _, err := exec.LookPath("restic"); err != nil {
		return nil, errors.New("restic is unavailable")
	}
	passwordCommand := shellquote.POSIX(bao) + " -- bao kv get -field=" + shellquote.POSIX(parts[1]) + " " + shellquote.POSIX(parts[0])
	env := make([]string, 0, len(os.Environ())+2)
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "RESTIC_REPOSITORY=") && !strings.HasPrefix(item, "RESTIC_PASSWORD=") && !strings.HasPrefix(item, "RESTIC_PASSWORD_FILE=") && !strings.HasPrefix(item, "RESTIC_PASSWORD_COMMAND=") && !strings.HasPrefix(item, "RESTIC_REPOSITORY_FILE=") {
			env = append(env, item)
		}
	}
	env = append(env, "RESTIC_REPOSITORY="+cfg.Archive.Repo, "RESTIC_PASSWORD_COMMAND="+passwordCommand)
	return env, nil
}

func runRestic(cfg config, args ...string) ([]byte, error) {
	env, err := resticEnvironment(cfg)
	if err != nil {
		return nil, err
	}
	bin, err := exec.LookPath("restic")
	if err != nil {
		return nil, errors.New("restic is unavailable")
	}
	return archiveExec(bin, args, env)
}

func resolveBundle(cfg config, value string) (string, evidenceManifest, error) {
	var empty evidenceManifest
	p, err := cleanPath(value)
	if err == nil && inside(cfg.Root, p) {
		if _, safetyErr := measureTreeWithin(p, cfg.Root); safetyErr != nil {
			return "", empty, safetyErr
		}
		m, e := readManifest(p)
		return p, m, e
	}
	if !validateName(value) {
		return "", empty, errors.New("bundle must be a path or bundle id")
	}
	dirs, err := evidenceDirs(cfg, "")
	if err != nil {
		return "", empty, err
	}
	for _, dir := range dirs {
		children, _ := os.ReadDir(dir)
		for _, child := range children {
			if !child.IsDir() {
				continue
			}
			p := filepath.Join(dir, child.Name())
			m, e := readManifest(p)
			if e == nil && m.ID == value {
				return p, m, nil
			}
		}
	}
	return "", empty, errors.New("evidence bundle not found")
}

func findPlanEntry(plan gcPlan, path, action string) (planEntry, bool) {
	for _, entry := range plan.Candidates {
		if filepath.Clean(entry.Path) == filepath.Clean(path) && entry.Action == action {
			return entry, true
		}
	}
	return planEntry{}, false
}

func planContains(plan gcPlan, path, action string) bool {
	_, ok := findPlanEntry(plan, path, action)
	return ok
}

func plannedManifestMatches(bundle string, planned planEntry) bool {
	if planned.ManifestSHA256 == "" {
		return false
	}
	sum, _, err := hashFile(filepath.Join(bundle, "manifest.json"))
	return err == nil && sum == planned.ManifestSHA256
}

func archiveCommand(cfg config, args []string, stdout, stderr io.Writer) int {
	return withIndexLock(cfg, func(idx *registryIndex) int {
		return archiveCommandLocked(cfg, args, stdout, stderr, idx)
	})
}

func archiveCommandLocked(cfg config, args []string, stdout, stderr io.Writer, idx *registryIndex) int {
	o, err := parseOptions(args, []string{"apply", "policy", "dry-run"}, []string{"plan", "plan-dir"})
	if err != nil {
		return parseErr(stderr, err)
	}
	if code := checkPlanFlags(o, stderr); code != exitOK {
		return code
	}
	planDirectory, err := resolvePlanDirectory(cfg, o.values["plan-dir"])
	if err != nil {
		return usageError(stderr, "invalid plan directory")
	}
	if code := requireRepo(cfg, stderr); code != exitOK {
		return code
	}
	if _, err := resticEnvironment(cfg); err != nil {
		fmt.Fprintf(stderr, "%s: archive setup: %v\n", prog, err)
		return exitEnv
	}
	if o.bools["policy"] {
		if len(o.pos) != 0 {
			return usageError(stderr, "archive --policy [--apply --plan <id>] [--plan-dir DIR]")
		}
		if !o.bools["apply"] {
			plan, err := buildArchiveRetentionPlanLocked(cfg, *idx)
			if err != nil {
				fmt.Fprintf(stderr, "%s: archive policy plan failed\n", prog)
				if errors.Is(err, errArchiveBlocked) {
					return exitBlocked
				}
				return exitEnv
			}
			if err := persistPlan(plan, planDirectory); err != nil {
				return exitEnv
			}
			fmt.Fprintf(stdout, "plan_id=%s expired_evidence_snapshots=%d retention_months=%d path=%s\n", plan.ID, len(plan.SnapshotIDs), cfg.Archive.RetentionMonths, filepath.Join(planDirectory, plan.ID+".json"))
			return exitOK
		}
		plan, err := loadPlan(cfg, o.values["plan"], planDirectory)
		if err != nil || plan.Action != "archive_retention" || plan.Policy.ArchiveRetentionMonths != cfg.Archive.RetentionMonths {
			return exitBlocked
		}
		if len(plan.SnapshotIDs) == 0 {
			fmt.Fprintln(stdout, "no evidence snapshots exceeded the retention window")
			return exitOK
		}
		current, err := evidenceSnapshots(cfg)
		if err != nil {
			return exitNegative
		}
		eligible, err := eligibleRetentionSnapshots(current, cfg.Archive.RetentionMonths, time.Now())
		if err != nil {
			return exitNegative
		}
		if len(eligible) != len(plan.SnapshotIDs) {
			fmt.Fprintln(stderr, "agent-state: retention plan no longer matches eligible evidence snapshots")
			return exitBlocked
		}
		for _, planned := range plan.SnapshotIDs {
			if !eligible[planned] {
				fmt.Fprintln(stderr, "agent-state: retention plan no longer matches eligible evidence snapshots")
				return exitBlocked
			}
		}
		currentIndex, err := loadIndex(cfg)
		if err != nil {
			return exitEnv
		}
		if err := checkRetentionProtection(cfg, currentIndex, current, eligible); err != nil {
			return exitBlocked
		}
		args := append([]string{"forget", "--prune"}, plan.SnapshotIDs...)
		if _, err := runRestic(cfg, args...); err != nil {
			fmt.Fprintf(stderr, "%s: restic retention failed; no local data changed\n", prog)
			return exitNegative
		}
		fmt.Fprintf(stdout, "evidence retention applied: %d months, snapshots=%d\n", cfg.Archive.RetentionMonths, len(plan.SnapshotIDs))
		return exitOK
	}
	if len(o.pos) != 1 {
		return usageError(stderr, "archive <bundle> [--apply --plan <id>]")
	}
	if !o.bools["apply"] {
		if o.bools["dry-run"] {
			fmt.Fprintln(stdout, "no archive action taken; generate and review a matching gc plan first")
			return exitOK
		}
		fmt.Fprintln(stderr, "agent-state: archive and stub require --apply --plan <id>")
		return exitBlocked
	}
	bundle, m, err := resolveBundle(cfg, o.pos[0])
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", prog, err)
		return exitNegative
	}
	plan, err := loadPlan(cfg, o.values["plan"], planDirectory)
	planned, authorized := findPlanEntry(plan, bundle, "archive_then_stub")
	if err != nil || plan.Action != "" || !authorized {
		fmt.Fprintln(stderr, "agent-state: plan does not authorize this evidence bundle")
		return exitBlocked
	}
	currentBytes, err := measureTreeWithin(bundle, cfg.Root)
	if err != nil || currentBytes != planned.Bytes || !plannedManifestMatches(bundle, planned) {
		fmt.Fprintln(stderr, "agent-state: evidence changed since plan creation")
		return exitBlocked
	}
	if m.Status != "closed" || m.Archive != nil {
		return exitBlocked
	}
	if reasons := protectionCheck(cfg, *idx, bundle); len(reasons) > 0 {
		fmt.Fprintf(stderr, "agent-state: archive blocked by %s\n", strings.Join(reasons, ","))
		return exitBlocked
	}
	if findings, err := lintEvidence(bundle); err != nil {
		return exitEnv
	} else if len(findings) > 0 {
		fmt.Fprintln(stderr, "agent-state: archive blocked by evidence lint findings")
		return exitBlocked
	}
	secretFound, err := scanSecrets(bundle)
	if err != nil {
		return exitEnv
	}
	if secretFound {
		fmt.Fprintln(stderr, "agent-state: archive blocked by a redacted secret finding")
		return exitBlocked
	}
	if reasons := blobArchiveReasons(cfg, m.Blobs); len(reasons) > 0 {
		fmt.Fprintf(stderr, "agent-state: archive blocked by %s\n", strings.Join(reasons, ","))
		return exitBlocked
	}
	if err := validateArchiveStage(cfg, bundle, m); err != nil {
		return exitBlocked
	}
	if err := archiveBundle(cfg, bundle, &m, planned, idx); err != nil {
		fmt.Fprintf(stderr, "%s: archive failed; local bundle remains hot when the archive phase has not started\n", prog)
		if errors.Is(err, errArchiveBlocked) {
			return exitBlocked
		}
		return exitNegative
	}
	fmt.Fprintf(stdout, "archived and verified %s\n", m.ID)
	return exitOK
}

var errArchiveBlocked = errors.New("archive staging is now blocked")

func archiveBundle(cfg config, bundle string, m *evidenceManifest, planned planEntry, idx *registryIndex) error {
	if _, err := measureTreeWithin(bundle, cfg.Root); err != nil {
		return err
	}
	before, err := hashTree(bundle)
	if err != nil {
		return err
	}
	if before["manifest.json"] != planned.ManifestSHA256 {
		return errArchiveBlocked
	}
	if len(before) != len(m.Files)+1 || before["manifest.json"] == "" {
		return errors.New("evidence manifest does not cover the complete bundle")
	}
	for _, item := range m.Files {
		if before[item.Path] != item.SHA256 {
			return errors.New("evidence file differs from its manifest")
		}
	}
	args := []string{"backup", "--json", "--tag", "project=" + m.Project, "--tag", "bundle=" + m.ID, "--tag", "goal=" + m.GoalID, "--tag", "class=evidence", bundle}
	for _, blob := range m.Blobs {
		args = append(args, rootPath(cfg, filepath.FromSlash(blob.Path)))
	}
	if len(protectionCheck(cfg, *idx, bundle)) != 0 {
		return errArchiveBlocked
	}
	out, err := runRestic(cfg, args...)
	if err != nil {
		return err
	}
	snapshotID, err := snapshotIDFromOutput(out)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "agent-state-restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if _, err := runRestic(cfg, "restore", "--verify", "--target", tmp, snapshotID); err != nil {
		return err
	}
	if err := compareRestored(bundle, tmp, before); err != nil {
		return err
	}
	if err := compareRestoredBlobs(cfg, tmp, m.Blobs); err != nil {
		return err
	}
	if _, err := runRestic(cfg, "check"); err != nil {
		return err
	}
	// The command holds the registry lock across backup, verification and commit.
	currentIndex, err := loadIndex(cfg)
	if err != nil {
		return err
	}
	*idx = currentIndex
	if size, err := measureTreeWithin(bundle, cfg.Root); err != nil || size != planned.Bytes {
		return errArchiveBlocked
	}
	if reasons := protectionCheck(cfg, *idx, bundle); len(reasons) != 0 {
		return errArchiveBlocked
	}
	current, err := hashTree(bundle)
	if err != nil || !maps.Equal(current, before) {
		return errArchiveBlocked
	}
	m.Archive = &archiveRef{Snapshot: snapshotID, RepoRef: "config:agent_state.archive.repo", VerifiedAt: time.Now().UTC().Format(time.RFC3339), ManifestSHA256: before["manifest.json"]}
	return stageArchivedBundle(cfg, bundle, m, *idx)
}

func validateArchiveStage(cfg config, bundle string, m evidenceManifest) error {
	trash := rootPath(cfg, "_registry", "trash", "archive-"+m.ID)
	if err := validateExistingPath(cfg.Root, filepath.Dir(trash)); err != nil {
		return err
	}
	if err := validateExistingPath(cfg.Root, bundle); err != nil {
		return err
	}
	if _, err := os.Lstat(trash); err == nil {
		return errors.New("archive staging path already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return nil
}

func stageArchivedBundle(cfg config, bundle string, m *evidenceManifest, idx registryIndex) error {
	if err := validateArchiveStage(cfg, bundle, *m); err != nil {
		return err
	}
	expectedTree, err := treeFingerprint(bundle)
	if err != nil {
		return err
	}
	trash := rootPath(cfg, "_registry", "trash", "archive-"+m.ID)
	stage, err := os.MkdirTemp(filepath.Dir(bundle), ".agent-state-archive-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := writeJSONAtomic(filepath.Join(stage, "manifest.json"), m, 0o600); err != nil {
		return err
	}
	stub := fmt.Sprintf("# Archived evidence\n\nBundle `%s` is stored in the configured restic repository.\nRestore it with `agent-state restore %s`.\n", m.ID, m.ID)
	if err := writeStateBytes(filepath.Join(stage, "ARCHIVED.md"), []byte(stub), 0o600); err != nil {
		return err
	}

	tx, err := beginStateMutation(cfg)
	if err != nil {
		return err
	}
	rollback := func(cause error) error {
		return errors.Join(cause, tx.rollback())
	}
	currentTree, err := treeFingerprint(bundle)
	if err != nil || currentTree != expectedTree {
		return rollback(errArchiveBlocked)
	}
	if err := tx.mkdir(filepath.Dir(trash)); err != nil {
		return rollback(err)
	}
	if len(protectionCheck(cfg, idx, bundle)) != 0 {
		return rollback(errArchiveBlocked)
	}
	if err := renameStatePath(bundle, trash); err != nil {
		return rollback(err)
	}
	if err := renameStatePath(stage, bundle); err != nil {
		restoreErr := renameStatePath(trash, bundle)
		return rollback(errors.Join(err, restoreErr))
	}
	tx.commit()
	return nil
}

func hashTree(root string) (map[string]string, error) {
	out := map[string]string{}
	rootDevice, err := safeRootDevice(root)
	if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return errors.New("archive tree contains a symlink")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		device, err := fileDevice(info)
		if err != nil || device != rootDevice {
			return errors.New("archive tree crosses a filesystem boundary")
		}
		if entry.IsDir() || !info.Mode().IsRegular() {
			return nil
		}
		sum, _, err := hashFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = sum
		return nil
	})
	return out, err
}

func compareRestored(source, target string, before map[string]string) error {
	rootRel := strings.TrimLeft(filepath.ToSlash(strings.TrimPrefix(source, string(filepath.Separator))), "/")
	restored := filepath.Join(target, filepath.FromSlash(rootRel))
	if err := validateExistingPath(target, restored); err != nil {
		return err
	}
	actual, err := hashTree(restored)
	if err != nil {
		return err
	}
	if len(actual) != len(before) {
		return errors.New("restored file count mismatch")
	}
	for name, sum := range before {
		if actual[name] != sum {
			return fmt.Errorf("restored hash mismatch for %s", name)
		}
	}
	return nil
}

func uniqueCASRecords(blobs []fileRecord) ([]fileRecord, error) {
	byHash := make(map[string]fileRecord, len(blobs))
	byPath := make(map[string]fileRecord, len(blobs))
	unique := make([]fileRecord, 0, len(blobs))
	for _, item := range blobs {
		if previous, ok := byHash[item.SHA256]; ok {
			if previous.Path != item.Path || previous.Bytes != item.Bytes {
				return nil, errors.New("conflicting duplicate CAS blob")
			}
			continue
		}
		if previous, ok := byPath[item.Path]; ok && (previous.SHA256 != item.SHA256 || previous.Bytes != item.Bytes) {
			return nil, errors.New("conflicting duplicate CAS blob path")
		}
		byHash[item.SHA256] = item
		byPath[item.Path] = item
		unique = append(unique, item)
	}
	return unique, nil
}

func compareRestoredBlobs(cfg config, target string, blobs []fileRecord) error {
	unique, err := uniqueCASRecords(blobs)
	if err != nil {
		return err
	}
	for _, item := range unique {
		path := filepath.Join(target, filepath.FromSlash(strings.TrimLeft(filepath.ToSlash(strings.TrimPrefix(rootPath(cfg, filepath.FromSlash(item.Path)), string(filepath.Separator))), "/")))
		if err := validateExistingPath(target, path); err != nil {
			return err
		}
		sum, n, err := hashFile(path)
		if err != nil || sum != item.SHA256 || n != item.Bytes {
			return errors.New("restored CAS blob hash mismatch")
		}
	}
	return nil
}

func snapshotIDFromOutput(raw []byte) (string, error) {
	var id string
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var msg resticMessage
		if json.Unmarshal(line, &msg) == nil && msg.SnapshotID != "" {
			id = msg.SnapshotID
		}
	}
	if !snapshotIDPattern.MatchString(id) {
		return "", errors.New("restic did not return a valid snapshot id")
	}
	return id, nil
}

func sha256Sum(raw []byte) string { sum := sha256.Sum256(raw); return fmt.Sprintf("%x", sum) }

func snapshotsFor(cfg config, bundle string) ([]resticSnapshot, error) {
	out, err := runRestic(cfg, "snapshots", "--json", "--tag", "bundle="+bundle)
	if err != nil {
		return nil, err
	}
	var snaps []resticSnapshot
	if err := json.Unmarshal(out, &snaps); err != nil {
		return nil, errors.New("restic returned invalid snapshot metadata")
	}
	return snaps, nil
}

func evidenceSnapshots(cfg config) ([]resticSnapshot, error) {
	out, err := runRestic(cfg, "snapshots", "--json", "--tag", "class=evidence")
	if err != nil {
		return nil, err
	}
	var snapshots []resticSnapshot
	if err := json.Unmarshal(out, &snapshots); err != nil {
		return nil, errors.New("restic returned invalid snapshot metadata")
	}
	return snapshots, nil
}

var snapshotIDPattern = regexp.MustCompile(`^[0-9a-f]{8,64}$`)

func eligibleRetentionSnapshots(snapshots []resticSnapshot, months int, now time.Time) (map[string]bool, error) {
	eligible := map[string]bool{}
	cutoff := now.AddDate(0, -months, 0)
	for _, snapshot := range snapshots {
		tagged := false
		for _, tag := range snapshot.Tags {
			if tag == "class=evidence" {
				tagged = true
				break
			}
		}
		if !tagged {
			continue
		}
		if !snapshotIDPattern.MatchString(snapshot.ID) {
			return nil, errors.New("restic returned an invalid snapshot id")
		}
		created, err := time.Parse(time.RFC3339, snapshot.Time)
		if err != nil {
			return nil, errors.New("restic returned an invalid snapshot timestamp")
		}
		if created.Before(cutoff) {
			eligible[snapshot.ID] = true
		}
	}
	return eligible, nil
}

func checkRetentionProtection(cfg config, idx registryIndex, snapshots []resticSnapshot, selected map[string]bool) error {
	for _, snapshot := range snapshots {
		if !selected[snapshot.ID] {
			continue
		}
		if len(snapshot.Paths) == 0 {
			return errArchiveBlocked
		}
		for _, path := range snapshot.Paths {
			if len(protectionCheck(cfg, idx, path)) != 0 {
				return errArchiveBlocked
			}
		}
	}
	return nil
}

func buildArchiveRetentionPlan(cfg config) (gcPlan, error) {
	var plan gcPlan
	var planErr error
	code := withIndexLock(cfg, func(idx *registryIndex) int {
		plan, planErr = buildArchiveRetentionPlanLocked(cfg, *idx)
		return exitOK
	})
	if code != exitOK {
		return plan, errArchiveBlocked
	}
	return plan, planErr
}

func buildArchiveRetentionPlanLocked(cfg config, idx registryIndex) (gcPlan, error) {
	plan := gcPlan{Schema: planSchema, Action: "archive_retention", Root: cfg.Root, CreatedAt: time.Now().UTC().Format(time.RFC3339), Policy: planPolicy{ScratchTTL: cfg.ScratchTTL, EvidenceGrace: cfg.EvidenceGrace, ArchiveRetentionMonths: cfg.Archive.RetentionMonths, ActiveLedgerDays: cfg.ActiveLedgerDays}}
	id, err := newID("plan-")
	if err != nil {
		return plan, err
	}
	plan.ID = id
	snapshots, err := evidenceSnapshots(cfg)
	if err != nil {
		return plan, err
	}
	eligible, err := eligibleRetentionSnapshots(snapshots, cfg.Archive.RetentionMonths, time.Now())
	if err != nil {
		return plan, err
	}
	if err := checkRetentionProtection(cfg, idx, snapshots, eligible); err != nil {
		return plan, err
	}
	for snapshotID := range eligible {
		plan.SnapshotIDs = append(plan.SnapshotIDs, snapshotID)
	}
	sort.Strings(plan.SnapshotIDs)
	if err := sealPlan(&plan); err != nil {
		return plan, err
	}
	return plan, nil
}

func verifySnapshotFiles(cfg config, snapshot string, m evidenceManifest) error {
	tmp, err := os.MkdirTemp("", "agent-state-verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if _, err := runRestic(cfg, "restore", "--verify", "--target", tmp, snapshot); err != nil {
		return err
	}
	rootRel := strings.TrimLeft(filepath.ToSlash(strings.TrimPrefix(mustBundlePath(cfg, m), string(filepath.Separator))), "/")
	base := filepath.Join(tmp, filepath.FromSlash(rootRel))
	if err := validateExistingPath(tmp, base); err != nil {
		return err
	}
	actual, err := hashTree(base)
	if err != nil {
		return err
	}
	manifestHash := actual["manifest.json"]
	if m.Archive == nil || manifestHash == "" || manifestHash != m.Archive.ManifestSHA256 {
		return errors.New("archived manifest hash mismatch")
	}
	if len(actual) != len(m.Files)+1 {
		return errors.New("archived evidence tree has unlisted files")
	}
	for _, item := range m.Files {
		if actual[item.Path] != item.SHA256 {
			return errors.New("archived evidence file failed verification")
		}
	}
	for _, item := range m.Blobs {
		cas := rootPath(cfg, filepath.FromSlash(item.Path))
		casRel := strings.TrimLeft(filepath.ToSlash(strings.TrimPrefix(cas, string(filepath.Separator))), "/")
		path := filepath.Join(tmp, filepath.FromSlash(casRel))
		if err := validateExistingPath(tmp, path); err != nil {
			return err
		}
		sum, n, err := hashFile(path)
		if err != nil || sum != item.SHA256 || n != item.Bytes {
			return errors.New("archived CAS blob failed verification")
		}
	}
	return nil
}

func mustBundlePath(cfg config, m evidenceManifest) string {
	p, _, err := resolveBundle(cfg, m.ID)
	if err != nil {
		return filepath.Join(cfg.Root, m.Project, "evidence", m.ID)
	}
	return p
}

func verifyCommand(cfg config, args []string, stdout, stderr io.Writer) int {
	o, err := parseOptions(args, []string{"all"}, []string{"sample"})
	if err != nil {
		return parseErr(stderr, err)
	}
	if code := requireRepo(cfg, stderr); code != exitOK {
		return code
	}
	if _, err := resticEnvironment(cfg); err != nil {
		return exitEnv
	}
	if (o.bools["all"] && len(o.pos) != 0) || (!o.bools["all"] && len(o.pos) != 1) {
		return usageError(stderr, "verify <bundle>|--all [--sample 5%]")
	}
	sample := o.values["sample"]
	if sample == "" {
		sample = "5%"
	}
	if !regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?%$`).MatchString(sample) {
		return usageError(stderr, "--sample must be a percentage")
	}
	var bundles []struct {
		path string
		m    evidenceManifest
	}
	if o.bools["all"] {
		dirs, err := evidenceDirs(cfg, "")
		if err != nil {
			return exitEnv
		}
		for _, d := range dirs {
			children, err := os.ReadDir(d)
			if err != nil {
				return exitEnv
			}
			for _, c := range children {
				if c.Type()&fs.ModeSymlink != 0 {
					return exitBlocked
				}
				if c.IsDir() {
					p := filepath.Join(d, c.Name())
					m, e := readManifest(p)
					if e != nil {
						return exitBlocked
					}
					if m.Archive != nil {
						bundles = append(bundles, struct {
							path string
							m    evidenceManifest
						}{p, m})
					}
				}
			}
		}
	} else {
		p, m, e := resolveBundle(cfg, o.pos[0])
		if e != nil {
			return exitNegative
		}
		if m.Archive == nil || !snapshotIDPattern.MatchString(m.Archive.Snapshot) {
			return exitBlocked
		}
		bundles = append(bundles, struct {
			path string
			m    evidenceManifest
		}{p, m})
	}
	for _, item := range bundles {
		snapshots, e := snapshotsFor(cfg, item.m.ID)
		if e != nil {
			return exitNegative
		}
		matched := false
		for _, snapshot := range snapshots {
			if snapshot.ID == item.m.Archive.Snapshot {
				matched = true
				break
			}
		}
		if !matched {
			return exitBlocked
		}
		if _, e := runRestic(cfg, "check", "--tag", "bundle="+item.m.ID, "--read-data-subset", sample); e != nil {
			fmt.Fprintf(stderr, "agent-state: restic verification failed for %s\n", item.m.ID)
			return exitNegative
		}
		if e := verifySnapshotFiles(cfg, item.m.Archive.Snapshot, item.m); e != nil {
			fmt.Fprintf(stderr, "agent-state: archive content verification failed for %s\n", item.m.ID)
			return exitNegative
		}
		fmt.Fprintf(stdout, "verified %s %s\n", item.m.ID, item.m.Archive.Snapshot)
	}
	return exitOK
}

func restoreCommand(cfg config, args []string, stdout, stderr io.Writer) int {
	return withIndexLock(cfg, func(idx *registryIndex) int {
		return restoreCommandLocked(cfg, args, stdout, stderr, idx)
	})
}

func restoreCommandLocked(cfg config, args []string, stdout, stderr io.Writer, idx *registryIndex) int {
	o, err := parseOptions(args, nil, []string{"to"})
	if err != nil {
		return parseErr(stderr, err)
	}
	if len(o.pos) != 1 {
		return usageError(stderr, "restore <bundle> [--to <path>]")
	}
	if code := requireRepo(cfg, stderr); code != exitOK {
		return code
	}
	if _, err := resticEnvironment(cfg); err != nil {
		return exitEnv
	}
	bundle, m, err := resolveBundle(cfg, o.pos[0])
	if err != nil {
		return exitNegative
	}
	if m.Archive == nil || !snapshotIDPattern.MatchString(m.Archive.Snapshot) {
		return exitBlocked
	}
	originalTree, err := treeFingerprint(bundle)
	if err != nil {
		return exitBlocked
	}
	target := o.values["to"]
	if target == "" {
		target = bundle
	}
	target, err = cleanPath(target)
	if err != nil {
		return exitUsage
	}
	if err := validateRestoreTarget(cfg.Root, target, target == bundle); err != nil {
		return usageError(stderr, "restore target is unsafe")
	}
	if reasons := protectionCheck(cfg, *idx, target); len(reasons) != 0 {
		return exitBlocked
	}
	if err := preflightCASDestinations(cfg, *idx, m.Blobs); err != nil {
		return exitBlocked
	}
	trash := ""
	if target == bundle {
		trash = rootPath(cfg, "_registry", "trash", "restore-"+m.ID+"-"+time.Now().UTC().Format("20060102T150405Z"))
		if err := validateExistingPath(cfg.Root, trash); err != nil {
			return exitBlocked
		}
		if _, err := os.Lstat(trash); !errors.Is(err, os.ErrNotExist) {
			return exitBlocked
		}
	}
	parentInfo, err := os.Stat(filepath.Dir(target))
	if err != nil || !parentInfo.IsDir() {
		return usageError(stderr, "restore target parent must exist")
	}
	tmp, err := os.MkdirTemp(filepath.Dir(target), ".agent-state-restore-")
	if err != nil {
		return exitEnv
	}
	defer os.RemoveAll(tmp)
	if _, err := runRestic(cfg, "restore", "--verify", "--target", tmp, m.Archive.Snapshot); err != nil {
		fmt.Fprintln(stderr, "agent-state: restic restore failed")
		return exitNegative
	}
	rootRel := strings.TrimLeft(filepath.ToSlash(strings.TrimPrefix(bundle, string(filepath.Separator))), "/")
	restored := filepath.Join(tmp, filepath.FromSlash(rootRel))
	if !inside(tmp, restored) {
		return exitBlocked
	}
	if err := verifyRestoredManifest(cfg, restored, tmp, m); err != nil {
		fmt.Fprintln(stderr, "agent-state: restored archive failed manifest checks")
		return exitNegative
	}
	restoredManifest, err := readManifest(restored)
	if err != nil {
		return exitNegative
	}
	restoredManifest.Archive = m.Archive
	restoredManifest.Status = "closed"
	if err := writeJSONAtomic(filepath.Join(restored, "manifest.json"), restoredManifest, 0o600); err != nil {
		return exitEnv
	}

	currentIndex, err := loadIndex(cfg)
	if err != nil {
		return exitEnv
	}
	*idx = currentIndex
	if err := validateRestoreTarget(cfg.Root, target, target == bundle); err != nil {
		return exitBlocked
	}
	if reasons := protectionCheck(cfg, *idx, target); len(reasons) != 0 {
		return exitBlocked
	}
	if err := preflightCASDestinations(cfg, *idx, m.Blobs); err != nil {
		return exitBlocked
	}
	if target == bundle {
		current, err := treeFingerprint(bundle)
		if err != nil || current != originalTree {
			return exitBlocked
		}
	}

	tx, err := beginStateMutation(cfg)
	if err != nil {
		return exitEnv
	}
	rollbackCode := func(code int) int {
		if rollbackErr := tx.rollback(); rollbackErr != nil {
			fmt.Fprintln(stderr, "agent-state: restore rollback failed")
			return exitNegative
		}
		return code
	}
	if err := restoreCASBlobs(cfg, tmp, m.Blobs, tx); err != nil {
		fmt.Fprintln(stderr, "agent-state: CAS blob restore failed")
		return rollbackCode(exitNegative)
	}
	if target == bundle {
		if err := validateExistingPath(cfg.Root, filepath.Dir(trash)); err != nil {
			return rollbackCode(exitBlocked)
		}
		if _, err := os.Lstat(trash); err == nil {
			return rollbackCode(exitBlocked)
		} else if !errors.Is(err, os.ErrNotExist) {
			return rollbackCode(exitEnv)
		}
		current, err := treeFingerprint(bundle)
		if err != nil || current != originalTree {
			return rollbackCode(exitBlocked)
		}
		if err := tx.mkdir(filepath.Dir(trash)); err != nil {
			return rollbackCode(exitEnv)
		}
		if reasons := protectionCheck(cfg, *idx, bundle); len(reasons) != 0 {
			return rollbackCode(exitBlocked)
		}
		if err := renameStatePath(bundle, trash); err != nil {
			return rollbackCode(exitEnv)
		}
		if err := renameStatePath(restored, bundle); err != nil {
			restoreErr := renameStatePath(trash, bundle)
			if restoreErr != nil {
				fmt.Fprintln(stderr, "agent-state: original archive stub could not be restored")
			}
			return rollbackCode(exitEnv)
		}
	} else {
		if err := validateRestoreTarget(cfg.Root, target, false); err != nil {
			return rollbackCode(exitBlocked)
		}
		if len(protectionCheck(cfg, *idx, target)) != 0 {
			return rollbackCode(exitBlocked)
		}
		if err := renameStatePath(restored, target); err != nil {
			return rollbackCode(exitEnv)
		}
	}
	tx.commit()
	fmt.Fprintf(stdout, "restored %s to %s\n", m.ID, target)
	return exitOK
}

func validateRestoreTarget(root, target string, allowExisting bool) error {
	rootDevice, err := safeRootDevice(root)
	if err != nil {
		return err
	}
	if !inside(root, target) || filepath.Clean(root) == filepath.Clean(target) {
		return errors.New("target is outside the state root")
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	current := filepath.Clean(root)
	parts := strings.Split(rel, string(filepath.Separator))
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return errors.New("target traverses a symlink")
		}
		device, err := fileDevice(info)
		if err != nil || device != rootDevice {
			return errors.New("target crosses a filesystem boundary")
		}
		last := i == len(parts)-1
		if !last && !info.IsDir() {
			return errors.New("target parent is not a directory")
		}
		if last && !allowExisting {
			return errors.New("target already exists")
		}
		if last && allowExisting && !info.IsDir() {
			return errors.New("existing target is not a directory")
		}
	}
	return nil
}

func verifyRestoredManifest(cfg config, bundlePath, restoreTarget string, m evidenceManifest) error {
	if err := validateExistingPath(restoreTarget, bundlePath); err != nil {
		return err
	}
	restored, err := readManifest(bundlePath)
	if err != nil || restored.ID != m.ID {
		return errors.New("restored manifest is invalid")
	}
	actual, err := hashTree(bundlePath)
	if err != nil {
		return err
	}
	if len(actual) != len(m.Files)+1 {
		return errors.New("restored tree has unlisted files")
	}
	if m.Archive == nil || actual["manifest.json"] != m.Archive.ManifestSHA256 {
		return errors.New("archived manifest hash mismatch")
	}
	for _, item := range m.Files {
		if actual[item.Path] != item.SHA256 {
			return errors.New("archived file mismatch")
		}
	}
	for _, item := range m.Blobs {
		path := filepath.Join(restoreTarget, filepath.FromSlash(strings.TrimLeft(filepath.ToSlash(strings.TrimPrefix(rootPath(cfg, filepath.FromSlash(item.Path)), string(filepath.Separator))), "/")))
		if err := validateExistingPath(restoreTarget, path); err != nil {
			return err
		}
		if sum, n, err := hashFile(path); err != nil || sum != item.SHA256 || n != item.Bytes {
			return errors.New("archived CAS blob mismatch")
		}
	}
	return nil
}

func validateCASDestinations(cfg config, blobs []fileRecord) error {
	unique, err := uniqueCASRecords(blobs)
	if err != nil {
		return err
	}
	for _, item := range unique {
		if !safeRelativePath(item.Path) {
			return errors.New("CAS blob path is unsafe")
		}
		destination := rootPath(cfg, filepath.FromSlash(item.Path))
		if err := validateRestoreTarget(cfg.Root, filepath.Dir(destination), true); err != nil {
			return err
		}
		info, err := os.Lstat(destination)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("existing CAS blob is not a regular file")
		}
		if err := validateExistingPath(cfg.Root, destination); err != nil {
			return err
		}
		sha, size, err := hashFile(destination)
		if err != nil || sha != item.SHA256 || size != item.Bytes {
			return errors.New("existing CAS blob does not match its manifest")
		}
	}
	return nil
}

func preflightCASDestinations(cfg config, idx registryIndex, blobs []fileRecord) error {
	if err := validateCASDestinations(cfg, blobs); err != nil {
		return err
	}
	for _, item := range blobs {
		destination := rootPath(cfg, filepath.FromSlash(item.Path))
		if reasons := protectionCheck(cfg, idx, destination); len(reasons) != 0 {
			return errors.New("CAS blob destination is protected")
		}
	}
	return nil
}

func restoreCASBlobs(cfg config, restoreRoot string, blobs []fileRecord, transactions ...*stateTransaction) error {
	if len(transactions) > 1 {
		return errors.New("restore accepts only one transaction")
	}
	if len(transactions) == 0 {
		var restoreErr error
		code := withIndexTransaction(cfg, io.Discard, func(_ *registryIndex, tx *stateTransaction) int {
			restoreErr = restoreCASBlobs(cfg, restoreRoot, blobs, tx)
			if restoreErr != nil {
				return exitNegative
			}
			return exitOK
		})
		if code != exitOK && restoreErr == nil {
			return errors.New("CAS restore lock or transaction failed")
		}
		return restoreErr
	}
	tx := transactions[0]
	if tx == nil {
		return errors.New("restore transaction is nil")
	}
	unique, err := uniqueCASRecords(blobs)
	if err != nil {
		return err
	}
	blobs = unique
	fail := func(cause error) error {
		return errors.Join(cause, tx.rollback())
	}
	idx, err := loadIndex(cfg)
	if err != nil {
		return fail(err)
	}
	if err := preflightCASDestinations(cfg, idx, blobs); err != nil {
		return fail(err)
	}
	sources := make([]string, len(blobs))
	for i, item := range blobs {
		destination := rootPath(cfg, filepath.FromSlash(item.Path))
		if _, err := os.Lstat(destination); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return fail(err)
		}
		source := filepath.Join(restoreRoot, filepath.FromSlash(strings.TrimLeft(filepath.ToSlash(strings.TrimPrefix(destination, string(filepath.Separator))), "/")))
		if err := validateExistingPath(restoreRoot, source); err != nil {
			return fail(err)
		}
		if info, err := os.Lstat(source); err != nil || !info.Mode().IsRegular() {
			return fail(errors.New("restored CAS blob is not a regular file"))
		}
		sha, size, err := hashFile(source)
		if err != nil || sha != item.SHA256 || size != item.Bytes {
			return fail(errors.New("restored CAS blob does not match its manifest"))
		}
		sources[i] = source
	}
	for i, item := range blobs {
		destination := rootPath(cfg, filepath.FromSlash(item.Path))
		if _, err := os.Lstat(destination); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return fail(err)
		}
		if err := tx.mkdir(filepath.Dir(destination)); err != nil {
			return fail(err)
		}
		if err := tx.track(destination); err != nil {
			return fail(err)
		}
		if len(protectionCheck(cfg, idx, destination)) != 0 {
			return fail(errors.New("CAS destination became protected"))
		}
		if err := copyFile(destination, sources[i], 0o600); err != nil {
			return fail(err)
		}
		if sum, n, err := hashFile(destination); err != nil || sum != item.SHA256 || n != item.Bytes {
			return fail(errors.New("CAS source changed while copying"))
		}
	}
	return nil
}
