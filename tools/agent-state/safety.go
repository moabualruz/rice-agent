package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type repoStateRecord struct {
	Root      string  `json:"root"`
	Upstream  *string `json:"upstream"`
	Ahead     *int    `json:"ahead"`
	Staged    int     `json:"staged"`
	Unstaged  int     `json:"unstaged"`
	Untracked int     `json:"untracked"`
	Unmerged  int     `json:"unmerged"`
	Error     string  `json:"error"`
}

var safetyGitHistory = gitHistoryReasons

func gitHistoryReasons(path string) ([]string, error) {
	git, err := exec.LookPath("git")
	if err != nil {
		return nil, err
	}
	var env []string
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		switch key {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_CEILING_DIRECTORIES", "GIT_NAMESPACE":
			continue
		}
		env = append(env, item)
	}
	var reasons []string
	stash := exec.Command(git, "-C", path, "rev-parse", "--verify", "--quiet", "refs/stash")
	stash.Env = env
	stash.Stderr = io.Discard
	stashOut, err := stash.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			return nil, errors.New("git stash scan failed")
		}
	} else if len(bytes.TrimSpace(stashOut)) != 0 {
		reasons = append(reasons, "git_stashes_present")
	}
	unique := exec.Command(git, "-C", path, "rev-list", "HEAD", "--all", "--not", "--remotes", "-n", "1")
	unique.Env = env
	unique.Stderr = io.Discard
	uniqueOut, err := unique.Output()
	if err != nil {
		return nil, errors.New("git unique-commit scan failed")
	}
	if len(bytes.TrimSpace(uniqueOut)) != 0 {
		reasons = append(reasons, "git_unique_commits")
	}
	return reasons, nil
}

var safetyRepoState = func(path string) ([]repoStateRecord, error) {
	bin, err := exec.LookPath("repo-state")
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, path, "--json")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, errors.New("repo-state failed")
	}
	var records []repoStateRecord
	if err := json.Unmarshal(out, &records); err != nil {
		return nil, errors.New("repo-state returned invalid JSON")
	}
	return records, nil
}

// One process-wide lsof pass captures file handles and cwd entries for GC planning.
var safetyOpenPaths = func() ([]string, error) {
	bin, err := exec.LookPath("lsof")
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, "-Fn")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 && len(out) == 0 {
			return nil, nil
		}
		return nil, errors.New("lsof could not complete the open-file scan")
	}
	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n") && len(line) > 1 {
			paths = append(paths, strings.TrimPrefix(line, "n"))
		}
	}
	return paths, nil
}

func repoSafety(path string) []string {
	var markers []string
	walkErr := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			markers = append(markers, "unreadable")
			return filepath.SkipAll
		}
		if d.Name() == ".git" {
			markers = append(markers, p)
			if d.IsDir() {
				return filepath.SkipDir
			}
		}
		return nil
	})
	if walkErr != nil {
		markers = append(markers, "unreadable")
	}
	if len(markers) == 0 {
		return nil
	}
	var reasons []string
	for _, marker := range markers {
		if marker == "unreadable" {
			reasons = append(reasons, "unreadable_content")
			continue
		}
		repo := filepath.Dir(marker)
		records, err := safetyRepoState(repo)
		if err != nil {
			reasons = append(reasons, "git_state_unavailable")
			continue
		}
		if len(records) == 0 || records[0].Error != "" {
			reasons = append(reasons, "git_state_unavailable")
			continue
		}
		r := records[0]
		if r.Staged+r.Unstaged+r.Untracked+r.Unmerged == 0 && r.Upstream != nil && r.Ahead != nil && *r.Ahead == 0 {
			historyReasons, historyErr := safetyGitHistory(repo)
			if historyErr != nil {
				reasons = append(reasons, "git_history_state_unavailable")
			} else {
				reasons = append(reasons, historyReasons...)
			}
		}
		if r.Staged+r.Unstaged+r.Untracked+r.Unmerged > 0 {
			reasons = append(reasons, "git_dirty")
		}
		if r.Upstream == nil || r.Ahead == nil {
			reasons = append(reasons, "git_unique_or_unpushed_unknown")
		} else if *r.Ahead > 0 {
			reasons = append(reasons, "git_unpushed_commits")
		}
	}
	return unique(reasons)
}

func unique(input []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(input))
	for _, value := range input {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func pathInUse(candidate string, openPaths []string) bool {
	resolved, err := canonicalPath(candidate)
	if err != nil {
		return true
	}
	candidate = resolved
	for _, p := range openPaths {
		if !filepath.IsAbs(p) {
			continue
		}
		resolved, err := canonicalPath(p)
		if err != nil {
			return true
		}
		p = resolved
		if p == candidate || inside(candidate, p) {
			return true
		}
	}
	return false
}

func pinBlocks(path string, pins []pin) bool {
	resolved, err := canonicalPath(path)
	if err != nil {
		return true
	}
	path = resolved
	for _, p := range pins {
		pinPath, err := canonicalPath(p.Path)
		if err != nil {
			return true
		}
		if path == pinPath || inside(path, pinPath) || inside(pinPath, path) {
			return true
		}
	}
	return false
}

func livePID(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func processLockReasons(path string) ([]string, error) {
	var reasons []string
	err := walkFiles(path, func(p string, d fs.DirEntry) error {
		if d.IsDir() {
			return nil
		}
		name := strings.ToLower(d.Name())
		if name != "postmaster.pid" && !strings.HasSuffix(name, ".lock") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			reasons = append(reasons, "lock_file_unreadable")
			return nil
		}
		if !info.Mode().IsRegular() {
			reasons = append(reasons, "lock_file_pid_unknown")
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			reasons = append(reasons, "lock_file_unreadable")
			return nil
		}
		first := strings.Fields(string(b))
		if len(first) == 0 {
			reasons = append(reasons, "lock_file_pid_unknown")
			return nil
		}
		pid, err := strconv.Atoi(first[0])
		if err != nil {
			reasons = append(reasons, "lock_file_pid_unknown")
			return nil
		}
		if livePID(pid) {
			reasons = append(reasons, "live_process_lock")
		}
		return nil
	})
	return unique(reasons), err
}

var ledgerTimeKeys = map[string]bool{
	"turn_at": true, "last_turn_at": true, "updated_at": true, "last_updated_at": true,
	"last_activity_at": true, "updated": true, "timestamp": true, "at": true,
}

func collectActiveLedgerRefs(cfg config) (map[string]bool, error) {
	refs := map[string]bool{}
	rootDevice, err := safeRootDevice(cfg.Root)
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-time.Duration(cfg.ActiveLedgerDays) * 24 * time.Hour)
	futureLimit := time.Now().Add(24 * time.Hour)
	err = filepath.WalkDir(cfg.Root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errors.New("ledger scan encountered an unreadable path")
		}
		rel, err := filepath.Rel(cfg.Root, path)
		if err != nil {
			return errors.New("ledger scan encountered an invalid path")
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if d.Type()&fs.ModeSymlink != 0 {
			if len(parts) == 1 && parts[0] != "_registry" && parts[0] != "_cas" || len(parts) == 2 && filepath.Ext(path) == ".json" {
				return errors.New("ledger scan encountered a symlinked ledger path")
			}
			return nil
		}
		if d.IsDir() {
			// Reserved root storage and project subtrees cannot contain live ledgers.
			// Project names themselves may equal any of the subtree names.
			if len(parts) == 1 && (parts[0] == "_registry" || parts[0] == "_cas") || len(parts) == 2 && (parts[1] == "evidence" || parts[1] == "scratch" || parts[1] == "ledger-history") {
				return filepath.SkipDir
			}
			info, err := d.Info()
			if err != nil {
				return errors.New("ledger scan encountered an unreadable path")
			}
			device, err := fileDevice(info)
			if err != nil || device != rootDevice {
				return errors.New("ledger scan crossed a filesystem boundary")
			}
			return nil
		}
		if len(parts) != 2 || filepath.Ext(path) != ".json" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return errors.New("ledger scan encountered an unreadable file")
		}
		device, err := fileDevice(info)
		if err != nil || device != rootDevice {
			return errors.New("ledger scan crossed a filesystem boundary")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return errors.New("ledger scan encountered an unreadable ledger")
		}
		var doc any
		if json.Unmarshal(raw, &doc) != nil {
			return errors.New("ledger scan encountered invalid JSON")
		}
		active, values := ledgerFacts(doc, cutoff, futureLimit)
		if active {
			for _, value := range values {
				if strings.HasPrefix(value, "agent-state://") {
					refs[value] = true
					continue
				}
				if filepath.IsAbs(value) {
					refs[filepath.Clean(value)] = true
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return refs, nil
}

func ledgerFacts(doc any, cutoff, futureLimit time.Time) (bool, []string) {
	active := false
	var values []string
	var visit func(any, string)
	visit = func(value any, key string) {
		switch x := value.(type) {
		case map[string]any:
			for k, v := range x {
				visit(v, strings.ToLower(k))
			}
		case []any:
			for _, v := range x {
				visit(v, key)
			}
		case string:
			values = append(values, strings.TrimSpace(x))
			if ledgerTimeKeys[key] {
				if stamp, err := time.Parse(time.RFC3339, x); err == nil && !stamp.Before(cutoff) && !stamp.After(futureLimit) {
					active = true
				}
			}
		}
	}
	visit(doc, "")
	return active, values
}

func ledgerReferencesPath(refs map[string]bool, candidate string) bool {
	resolved, err := canonicalPath(candidate)
	if err != nil {
		return true
	}
	candidate = resolved
	for ref := range refs {
		if strings.HasPrefix(ref, "agent-state://") {
			refPath := strings.TrimPrefix(ref, "agent-state://")
			parts := strings.SplitN(refPath, "/", 3)
			if len(parts) >= 2 && filepath.Base(candidate) == parts[1] {
				return true
			}
			continue
		}
		resolved, err := canonicalPath(ref)
		if err != nil {
			return true
		}
		ref = resolved
		if ref == candidate || strings.HasPrefix(ref, candidate+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func activeLedgerReferences(cfg config, candidate string) bool {
	refs, err := collectActiveLedgerRefs(cfg)
	return err != nil || ledgerReferencesPath(refs, candidate)
}

// Every planner and mutation uses this seam. There is deliberately no cached
// or reduced final-check variant that could omit a protection class.
var protectionCheck = fullProtectionCheck

func fullProtectionCheck(cfg config, idx registryIndex, path string) []string {
	var reasons []string
	if err := validateExistingPath(cfg.Root, path); err != nil {
		return []string{"unsafe_path"}
	}
	resolved, err := canonicalPath(path)
	root, rootErr := canonicalPath(cfg.Root)
	if err != nil || rootErr != nil || !inside(root, resolved) {
		return []string{"unsafe_path"}
	}
	if pinBlocks(path, idx.Pins) {
		reasons = append(reasons, "pinned_path")
	}
	if _, err := os.Lstat(path); err == nil {
		reasons = append(reasons, repoSafety(path)...)
		if _, err := measureTreeWithin(path, cfg.Root); err != nil {
			reasons = append(reasons, "unreadable_content")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		reasons = append(reasons, "unreadable_content")
	}
	// Refresh fast-changing process and ledger observations after Git inspection.
	open, openErr := safetyOpenPaths()
	if openErr != nil {
		reasons = append(reasons, "open_file_scan_unavailable")
	} else if pathInUse(path, open) {
		reasons = append(reasons, "open_file_or_process_cwd")
	}
	refs, ledgerErr := collectActiveLedgerRefs(cfg)
	if ledgerErr != nil && !errors.Is(ledgerErr, os.ErrNotExist) {
		reasons = append(reasons, "ledger_scan_unavailable")
	} else if ledgerReferencesPath(refs, path) {
		reasons = append(reasons, "active_ledger_reference")
	}
	// A process can publish its PID lock while the slower scans are running.
	if _, err := os.Lstat(path); err == nil {
		locks, lockErr := processLockReasons(path)
		reasons = append(reasons, locks...)
		if lockErr != nil {
			reasons = append(reasons, "unreadable_content")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		reasons = append(reasons, "unreadable_content")
	}
	return unique(reasons)
}
