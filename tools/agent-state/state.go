package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/moabualruz/agent-dry/internal/fslock"
)

const indexSchema = "agent-state/index-v1"
const evidenceSchema = "agent-evidence/v1"
const planSchema = "agent-state/plan-v1"

type pin struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
	Added  string `json:"added_at"`
}
type lease struct {
	ID          string `json:"id"`
	Project     string `json:"project"`
	Path        string `json:"path"`
	CreatedAt   string `json:"created_at"`
	RenewedAt   string `json:"renewed_at"`
	ExpiresAt   string `json:"expires_at"`
	ReleasedAt  string `json:"released_at,omitempty"`
	CollectedAt string `json:"collected_at,omitempty"`
}
type fileRecord struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Role   string `json:"role"`
}
type evidenceManifest struct {
	Schema     string       `json:"schema"`
	Kind       string       `json:"kind,omitempty"`
	ID         string       `json:"id"`
	Project    string       `json:"project"`
	GoalID     string       `json:"goal_id"`
	Criterion  string       `json:"criterion"`
	Status     string       `json:"status"`
	CreatedAt  string       `json:"created_at"`
	LastUsedAt string       `json:"last_used_at"`
	ClosedAt   string       `json:"closed_at,omitempty"`
	Result     string       `json:"result,omitempty"`
	Sources    []sourceRef  `json:"sources,omitempty"`
	Files      []fileRecord `json:"files,omitempty"`
	Blobs      []fileRecord `json:"blobs,omitempty"`
	Archive    *archiveRef  `json:"archive,omitempty"`
}
type sourceRef struct {
	Repo   string `json:"repo"`
	Commit string `json:"commit"`
}
type archiveRef struct {
	Snapshot       string `json:"snapshot"`
	RepoRef        string `json:"repo_ref"`
	VerifiedAt     string `json:"verified_at"`
	ManifestSHA256 string `json:"manifest_sha256"`
}
type registryIndex struct {
	Schema                string   `json:"schema"`
	Pins                  []pin    `json:"pins,omitempty"`
	Leases                []lease  `json:"leases,omitempty"`
	RemovedConfiguredPins []string `json:"removed_configured_pins,omitempty"`
}
type planEntry struct {
	Path           string             `json:"path"`
	Class          string             `json:"class"`
	Bytes          int64              `json:"bytes"`
	Action         string             `json:"action"`
	Reasons        []string           `json:"blocked_reasons,omitempty"`
	ManifestSHA256 string             `json:"manifest_sha256,omitempty"`
	TreeSHA256     string             `json:"tree_sha256,omitempty"`
	Migration      *migrationEvidence `json:"migration,omitempty"`
}
type gcPlan struct {
	Schema        string      `json:"schema"`
	ID            string      `json:"id"`
	Nonce         string      `json:"nonce"`
	Digest        string      `json:"digest"`
	Action        string      `json:"action,omitempty"`
	SnapshotIDs   []string    `json:"snapshot_ids,omitempty"`
	CreatedAt     string      `json:"created_at"`
	Root          string      `json:"root"`
	Policy        planPolicy  `json:"policy"`
	Candidates    []planEntry `json:"candidates"`
	Blocked       []planEntry `json:"blocked"`
	ScratchBytes  int64       `json:"scratch_expirable_bytes"`
	EvidenceBytes int64       `json:"archive_then_stub_bytes"`
	BlockedBytes  int64       `json:"blocked_bytes"`
}
type planPolicy struct {
	ScratchTTL             string `json:"scratch_ttl"`
	EvidenceGrace          string `json:"evidence_grace"`
	ArchiveRetentionMonths int    `json:"archive_retention_months"`
	ActiveLedgerDays       int    `json:"active_ledger_days"`
}

func digestPlan(plan gcPlan) (string, error) {
	plan.Digest = ""
	raw, err := json.Marshal(plan)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func sealPlan(plan *gcPlan) error {
	nonce, err := newID("nonce-")
	if err != nil {
		return err
	}
	plan.Nonce = nonce
	plan.Digest = ""
	plan.Digest, err = digestPlan(*plan)
	return err
}

func validatePlan(plan gcPlan) error {
	if !strings.HasPrefix(plan.Nonce, "nonce-") || len(plan.Digest) != sha256.Size*2 {
		return errors.New("plan integrity metadata is missing")
	}
	if plan.Action != "" && plan.Action != "archive_retention" && plan.Action != "migrate" {
		return errors.New("unsupported plan action")
	}
	if plan.Action == "archive_retention" {
		if plan.Policy.ArchiveRetentionMonths <= 0 || len(plan.Candidates) != 0 || len(plan.Blocked) != 0 {
			return errors.New("invalid archive retention plan")
		}
		seen := map[string]bool{}
		for _, id := range plan.SnapshotIDs {
			if !snapshotIDPattern.MatchString(id) || seen[id] {
				return errors.New("invalid archive retention snapshot list")
			}
			seen[id] = true
		}
	} else if len(plan.SnapshotIDs) != 0 {
		return errors.New("GC plan contains archive retention snapshots")
	}
	got, err := digestPlan(plan)
	if err != nil {
		return err
	}
	if got != plan.Digest {
		return errors.New("plan digest mismatch")
	}
	return nil
}

func registryPath(cfg config) string { return rootPath(cfg, "_registry") }
func indexPath(cfg config) string    { return filepath.Join(registryPath(cfg), "index.json") }
func plansPath(cfg config) string    { return filepath.Join(registryPath(cfg), "plans") }
func lockPath(cfg config) string     { return filepath.Join(registryPath(cfg), "locks", "index.lock") }

func validateExistingPath(root, target string) error {
	if !inside(root, target) {
		return errors.New("path is outside the state root")
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	var rootDevice uint64
	if info, statErr := os.Lstat(root); statErr == nil {
		if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("state root is not a regular directory")
		}
		rootDevice, err = fileDevice(info)
		if err != nil {
			return err
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	current := filepath.Clean(root)
	parts := strings.Split(rel, string(filepath.Separator))
	for i, part := range parts {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return errors.New("path traverses a symlink")
		}
		if device, err := fileDevice(info); err != nil || device != rootDevice {
			return errors.New("path crosses a filesystem boundary")
		}
		if i < len(parts)-1 && !info.IsDir() {
			return errors.New("path parent is not a directory")
		}
	}
	return nil
}

// mkdirState uses a directory handle so a concurrent parent swap cannot escape root.
func mkdirState(root, target string) error {
	if err := validateExistingPath(root, target); err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer dir.Close()
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	if err := dir.MkdirAll(rel, 0o700); err != nil {
		return err
	}
	return validateExistingPath(root, target)
}

func loadIndex(cfg config) (registryIndex, error) {
	idx := registryIndex{Schema: indexSchema}
	if _, err := safeRootDevice(cfg.Root); err != nil && !errors.Is(err, os.ErrNotExist) {
		return idx, err
	}
	if err := validateExistingPath(cfg.Root, indexPath(cfg)); err != nil {
		return idx, err
	}
	info, statErr := os.Lstat(indexPath(cfg))
	if statErr == nil && !info.Mode().IsRegular() {
		return idx, errors.New("registry index is not a regular file")
	}
	raw, err := readStateFile(indexPath(cfg))
	if errors.Is(statErr, os.ErrNotExist) {
		err = os.ErrNotExist
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return idx, err
	}
	if err == nil {
		if err := json.Unmarshal(raw, &idx); err != nil {
			return idx, fmt.Errorf("invalid registry index")
		}
		if idx.Schema != indexSchema {
			return idx, fmt.Errorf("unsupported registry schema %q", idx.Schema)
		}
	}
	seenLeases := map[string]bool{}
	for _, item := range idx.Leases {
		if !validateName(item.ID) || !strings.HasPrefix(item.ID, "lease-") || !validateName(item.Project) || seenLeases[item.ID] {
			return idx, errors.New("registry contains an invalid or duplicate scratch lease")
		}
		seenLeases[item.ID] = true
		expected := rootPath(cfg, item.Project, "scratch", item.ID)
		if item.Path != expected || !inside(cfg.Root, expected) {
			return idx, errors.New("registry scratch lease path does not match its project and id")
		}
	}
	for i := range idx.Pins {
		resolved, err := canonicalPath(idx.Pins[i].Path)
		if err != nil {
			return idx, err
		}
		idx.Pins[i].Path = resolved
	}
	for i := range idx.RemovedConfiguredPins {
		resolved, err := canonicalPath(idx.RemovedConfiguredPins[i])
		if err != nil {
			return idx, err
		}
		idx.RemovedConfiguredPins[i] = resolved
	}
	for _, p := range cfg.PinnedPaths {
		abs, err := canonicalPath(p)
		if err != nil {
			return idx, err
		}
		if !slices.Contains(idx.RemovedConfiguredPins, abs) && !hasPin(idx.Pins, abs) {
			idx.Pins = append(idx.Pins, pin{Path: abs, Reason: "configured pin"})
		}
	}
	return idx, nil
}

func hasPin(pins []pin, path string) bool {
	for _, p := range pins {
		if p.Path == path {
			return true
		}
	}
	return false
}

func indexLockAnchor(cfg config) string {
	anchor, _ := fslock.ExistingAnchor(cfg.Root)
	return anchor
}

func flockIndex(f *os.File) error {
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return err
		}
		if time.Now().After(deadline) {
			return errors.New("registry lock is busy")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func withIndexLock(cfg config, fn func(*registryIndex) int) int {
	if err := validateExistingPath(cfg.Root, lockPath(cfg)); err != nil {
		return exitBlocked
	}
	unlock, err := fslock.Lock([]string{cfg.Root}, 3*time.Second)
	if err != nil {
		return exitBlocked
	}
	defer unlock()
	// Honor an already-existing lock file without creating one. This also lets
	// an older writer finish before its registry is read.
	legacy, err := os.OpenFile(lockPath(cfg), os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err == nil {
		defer legacy.Close()
		if err := flockIndex(legacy); err != nil {
			return exitBlocked
		}
		defer syscall.Flock(int(legacy.Fd()), syscall.LOCK_UN)
	} else if !errors.Is(err, os.ErrNotExist) {
		return exitBlocked
	}
	idx, err := loadIndex(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agent-state: registry is invalid or unavailable")
		return exitEnv
	}
	return fn(&idx)
}

func saveIndex(cfg config, idx registryIndex) error {
	if err := validateExistingPath(cfg.Root, registryPath(cfg)); err != nil {
		return err
	}
	if err := mkdirState(cfg.Root, registryPath(cfg)); err != nil {
		return err
	}
	return writeJSONAtomic(indexPath(cfg), idx, 0o600)
}

func writeJSONAtomic(path string, value any, mode fs.FileMode) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeStateBytes(path, append(data, '\n'), mode)
}

var writeStateBytes = writeBytesAtomic

func writeBytesAtomic(path string, data []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".agent-state-*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func newID(prefix string) (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:]), nil
}

func readManifest(bundle string) (evidenceManifest, error) {
	var m evidenceManifest
	bundleInfo, err := os.Lstat(bundle)
	if err != nil {
		return m, err
	}
	if bundleInfo.Mode()&fs.ModeSymlink != 0 || !bundleInfo.IsDir() {
		return m, errors.New("evidence bundle is not a regular directory")
	}
	manifestPath := filepath.Join(bundle, "manifest.json")
	manifestInfo, err := os.Lstat(manifestPath)
	if err != nil {
		return m, err
	}
	if manifestInfo.Mode()&fs.ModeSymlink != 0 || !manifestInfo.Mode().IsRegular() {
		return m, errors.New("evidence manifest is not a regular file")
	}
	raw, err := readStateFile(manifestPath)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, errors.New("invalid evidence manifest")
	}
	if m.Schema != evidenceSchema || !validateName(m.ID) || !validateName(m.Project) {
		return m, errors.New("unsupported or incomplete evidence manifest")
	}
	for _, file := range m.Files {
		if !safeRelativePath(file.Path) || len(file.SHA256) != sha256.Size*2 || file.Bytes < 0 {
			return m, errors.New("invalid evidence file record")
		}
		if _, err := hex.DecodeString(file.SHA256); err != nil {
			return m, errors.New("invalid evidence file hash")
		}
	}
	for _, blob := range m.Blobs {
		if !safeRelativePath(blob.Path) || len(blob.SHA256) != sha256.Size*2 || blob.Bytes < 0 {
			return m, errors.New("invalid evidence blob record")
		}
		if _, err := hex.DecodeString(blob.SHA256); err != nil {
			return m, errors.New("invalid evidence blob hash")
		}
		expected := filepath.ToSlash(filepath.Join("_cas", "sha256", blob.SHA256[:2], blob.SHA256))
		if filepath.ToSlash(filepath.Clean(filepath.FromSlash(blob.Path))) != expected {
			return m, errors.New("evidence blob path does not match its hash")
		}
	}
	return m, nil
}

func safeRelativePath(path string) bool {
	if path == "" || strings.Contains(path, "\\") || filepath.IsAbs(path) {
		return false
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	return clean != "." && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator)) && filepath.ToSlash(clean) == path
}

func safeRootDevice(root string) (uint64, error) {
	clean := filepath.Clean(root)
	info, err := os.Lstat(clean)
	if err != nil {
		return 0, err
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return 0, errors.New("state root is not a regular directory")
	}
	return fileDevice(info)
}

func canonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := filepath.Clean(abs)
	var suffix []string
	for {
		if _, err := os.Lstat(current); err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", errors.New("cannot resolve path")
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func rejectSymlinkComponents(root, target string) error {
	if !inside(root, target) {
		return errors.New("candidate is outside the state root")
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	current := filepath.Clean(root)
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return errors.New("candidate traverses a symlink")
		}
	}
	return nil
}

func measureTree(root string) (int64, error) { return measureTreeWithin(root, root) }

func measureTreeWithin(root, filesystemRoot string) (int64, error) {
	rootDevice, err := safeRootDevice(filesystemRoot)
	if err != nil {
		return 0, err
	}
	if err := rejectSymlinkComponents(filesystemRoot, root); err != nil {
		return 0, err
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return 0, err
	}
	if rootInfo.Mode()&fs.ModeSymlink != 0 {
		return 0, errors.New("candidate root is a symlink")
	}
	candidateDevice, err := fileDevice(rootInfo)
	if err != nil {
		return 0, err
	}
	if candidateDevice != rootDevice {
		return 0, errors.New("candidate crosses a filesystem boundary")
	}
	var total int64
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		device, err := fileDevice(info)
		if err != nil {
			return err
		}
		if device != rootDevice {
			return errors.New("candidate crosses a filesystem boundary")
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return errors.New("candidate contains a symlink")
		}
		if d.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return errors.New("candidate contains a non-regular file")
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func fileDevice(info os.FileInfo) (uint64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("filesystem identity is unavailable")
	}
	return uint64(stat.Dev), nil
}

// Keep record reads and payload hashing injectable alongside mutation boundaries.
var readStateFile = os.ReadFile
var hashFile = hashFileContents

func hashFileContents(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

var treeFingerprint = fingerprintTree

func fingerprintTree(root string) (string, error) {
	rootDevice, err := safeRootDevice(root)
	if err != nil {
		return "", err
	}
	var entries []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return errors.New("scratch tree contains a symlink")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		device, err := fileDevice(info)
		if err != nil || device != rootDevice {
			return errors.New("scratch tree crosses a filesystem boundary")
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if entry.IsDir() {
			entries = append(entries, fmt.Sprintf("d:%o:%s", info.Mode().Perm(), name))
			return nil
		}
		if !info.Mode().IsRegular() {
			return errors.New("scratch tree contains a non-regular file")
		}
		sum, _, err := hashFile(path)
		if err != nil {
			return err
		}
		entries = append(entries, fmt.Sprintf("f:%o:%s:%s", info.Mode().Perm(), name, sum))
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(entries)
	h := sha256.New()
	for _, entry := range entries {
		if _, err := fmt.Fprintf(h, "%d:%s", len(entry), entry); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func walkFiles(root string, fn func(string, fs.DirEntry) error) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		return fn(path, d)
	})
}

func projectRoot(cfg config, project string) (string, error) {
	if !validateName(project) {
		return "", fmt.Errorf("invalid project name %q", project)
	}
	p := rootPath(cfg, project)
	if !inside(cfg.Root, p) {
		return "", errors.New("project path escapes root")
	}
	return p, nil
}

func bytesGiB(n int64) string { return fmt.Sprintf("%.2f", float64(n)/float64(gib)) }

func sortEntries(entries []planEntry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Class != entries[j].Class {
			return entries[i].Class < entries[j].Class
		}
		return entries[i].Path < entries[j].Path
	})
}

func readJSONFile(path string, value any) error {
	raw, err := readStateFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, value); err != nil {
		return fmt.Errorf("invalid JSON")
	}
	return nil
}

func setUsed(m *evidenceManifest) { m.LastUsedAt = time.Now().UTC().Format(time.RFC3339) }
