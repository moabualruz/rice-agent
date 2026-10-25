package dist

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/moabualruz/agent-dry/internal/cli"
	"github.com/moabualruz/agent-dry/internal/commands"
	"github.com/moabualruz/agent-dry/internal/manifest"
	"github.com/moabualruz/agent-dry/internal/paths"
)

const installUsage = `Usage: agent-dry install [--dry-run] [--uninstall] [--prefix <dir>] [--remote <host>]

Link agent-dry and every trial or active global tool for this platform into the prefix as
<name> symlinks into the agent-dry root, then write a receipt to the state directory.
An existing file that is not a symlink into the root, or another command of the same name on
PATH, is a collision: every collision is reported and nothing changes.

Options:
  --dry-run        print the plan (or the remote plan) and change nothing
  --uninstall      remove only links recorded in the receipt that still point into the root
  --prefix <dir>   link directory (default ~/.local/bin)
  --remote <host>  build a linux-amd64 bundle and install it on hosts.<host> from the config;
                   the remote login shell only parses "bash -s"; the remote exit status is returned

Exit codes: 0 installed or nothing to do, 1 collision, 2 usage, 3 environment (no root, bad
manifest, missing entry, unknown host), remote: the remote command's status (255 is ssh failure).

Example: agent-dry install --dry-run
`

// ReceiptSchema identifies install receipts.
const ReceiptSchema = "install-receipt/v1"

// ReceiptFile is the receipt name inside the state directory.
const ReceiptFile = "install-receipt.json"

// Link is one symlink install created.
type Link struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Target string `json:"target"`
	SHA256 string `json:"sha256,omitempty"`
	Action string `json:"action,omitempty"` // created, updated, unchanged, removed, skipped
	Reason string `json:"reason,omitempty"`
}

// Receipt records what install linked; uninstall and doctor read it.
type Receipt struct {
	Schema      string `json:"schema"`
	Version     string `json:"version"`
	Root        string `json:"root"`
	Prefix      string `json:"prefix"`
	Platform    string `json:"platform"`
	InstalledAt string `json:"installed_at"`
	Links       []Link `json:"links"`
	// Output-only fields; never stored.
	DryRun  bool   `json:"dry_run,omitempty"`
	File    string `json:"receipt,omitempty"`
	Removed []Link `json:"removed,omitempty"`
}

// executable is the running binary; tests replace it.
var executable = os.Executable

var installReceiptWrite = writeFileAtomic
var installLinkRemove = os.Remove

func platform() string { return runtime.GOOS + "-" + runtime.GOARCH }

func resolvedPath(path, home string) (string, error) {
	abs, err := absPath(path, home)
	if err != nil {
		return "", err
	}
	current := abs
	var tail []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return abs, nil
		}
		tail = append(tail, filepath.Base(current))
		current = parent
	}
}

func distRoot(getenv func(string) string) (string, error) {
	root, err := paths.Root(getenv)
	if err != nil {
		return "", err
	}
	return resolvedPath(root, paths.Home(getenv))
}

func stateDirectory(getenv func(string) string) (string, error) {
	return resolvedPath(paths.StateDir(getenv), paths.Home(getenv))
}

func receiptPath(getenv func(string) string) string {
	state, err := stateDirectory(getenv)
	if err != nil {
		state, _ = filepath.Abs(paths.StateDir(getenv))
	}
	return filepath.Join(state, ReceiptFile)
}

func loadReceipt(path string) (*Receipt, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("receipt is not a regular file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Receipt
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if r.Schema != ReceiptSchema {
		return nil, errors.New("unsupported install receipt schema")
	}
	if !filepath.IsAbs(r.Root) || !filepath.IsAbs(r.Prefix) {
		return nil, errors.New("receipt roots must be absolute")
	}
	resolvedPrefix, err := resolvedPath(r.Prefix, "")
	if err != nil || resolvedPrefix != filepath.Clean(r.Prefix) {
		return nil, errors.New("receipt prefix traverses a symlink")
	}
	for _, link := range r.Links {
		if !filepath.IsAbs(link.Path) || !filepath.IsAbs(link.Target) || filepath.Dir(link.Path) != filepath.Clean(r.Prefix) || filepath.Base(link.Path) != link.Name {
			return nil, errors.New("receipt link is outside its recorded prefix")
		}
	}
	return &r, nil
}

func defaultPrefix(getenv func(string) string) string {
	return filepath.Join(paths.Home(getenv), ".local", "bin")
}

// want is one link install should create.
type want struct {
	name     string
	target   string
	requires []string
}

// desiredLinks lists the agent-dry binary and every installable tool under root.
func desiredLinks(root string, binaries map[string]string, dryRun bool) ([]want, error) {
	bin := filepath.Join(root, "bin", "agent-dry")
	if info, err := os.Stat(bin); err != nil || !info.Mode().IsRegular() {
		exe, err := executable()
		if err != nil {
			return nil, fmt.Errorf("locate the running executable: %w", err)
		}
		exe, err = filepath.Abs(exe)
		if err != nil {
			return nil, fmt.Errorf("resolve the running executable: %w", err)
		}
		bin = realOrSelf(exe)
	}
	out := []want{{name: "agent-dry", target: bin}}
	tools, err := manifest.LoadAll(filepath.Join(root, "tools"))
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{"agent-dry": true}
	for _, t := range tools {
		if !installable(t) {
			continue
		}
		if seen[t.Name] {
			return nil, fmt.Errorf("tool %q collides with another link name", t.Name)
		}
		seen[t.Name] = true
		target := t.EntryPath()
		if built := binaries[t.Name]; built != "" {
			target = built
		}
		// Go entries are validated in staging, not at their future stable paths.
		if binaries[t.Name] == "" {
			info, err := os.Stat(target)
			if err != nil {
				return nil, fmt.Errorf("tool %s: entry: %w", t.Name, err)
			}
			if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
				return nil, fmt.Errorf("tool %s: entry %s is not an executable file", t.Name, target)
			}
		}
		out = append(out, want{name: t.Name, target: target, requires: t.Requires})
	}
	return out, nil
}

// installable reports whether install links the tool on this platform.
func installable(t *manifest.Tool) bool {
	if !t.Listed() || t.Scope != "global" || t.Kind == "skill" {
		return false
	}
	for _, p := range t.Platforms {
		if p == platform() {
			return true
		}
	}
	return false
}

func runInstall(env *cli.Env) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "")
	uninstall := fs.Bool("uninstall", false, "")
	prefix := fs.String("prefix", "", "")
	remote := fs.String("remote", "", "")
	pos, code, ok := parseArgs(env, fs, installUsage)
	if !ok {
		return code
	}
	if len(pos) > 0 {
		return cli.Usagef(env, "install: unexpected argument %q", pos[0])
	}
	if *remote != "" {
		return runRemote(env, *remote, *dryRun, *uninstall, *prefix)
	}
	home := paths.Home(env.Getenv)
	p := *prefix
	if p == "" {
		p = defaultPrefix(env.Getenv)
	}
	p, err := resolvedPath(p, home)
	if err != nil {
		return cli.Usagef(env, "install: --prefix: %v", err)
	}
	if *uninstall {
		return uninstallLinks(env, *dryRun, p)
	}
	return installLinks(env, p, *dryRun)
}

// owned reports whether a link destination belongs to this installation.
func owned(dest, root string, prev map[string]Link, linkPath string) bool {
	if within(root, dest) {
		return true
	}
	if l, ok := prev[linkPath]; ok && l.Target == dest {
		return true
	}
	return false
}

// readLinkAbs returns the absolute destination of the symlink at p.
func readLinkAbs(p string) (string, error) {
	dest, err := os.Readlink(p)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(dest) {
		dest = filepath.Join(filepath.Dir(p), dest)
	}
	return filepath.Clean(dest), nil
}

// shadowing finds another command named name on PATH, outside prefix, that is not the target.
func shadowing(name, target, prefix, pathEnv string) string {
	realTarget := realOrSelf(target)
	realPrefix := realOrSelf(prefix)
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		if filepath.Clean(dir) == prefix || realOrSelf(dir) == realPrefix {
			continue
		}
		p := filepath.Join(dir, name)
		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
			continue
		}
		if realOrSelf(p) != realTarget {
			return p
		}
	}
	return ""
}

func installLinks(env *cli.Env, prefix string, dryRun bool) (code int) {
	home := paths.Home(env.Getenv)
	root, err := distRoot(env.Getenv)
	if err != nil {
		return cli.Envf(env, "install: %v", err)
	}
	stateDir, err := stateDirectory(env.Getenv)
	if err != nil {
		return cli.Envf(env, "install: %v", err)
	}
	prefix, err = resolvedPath(prefix, home)
	if err != nil {
		return cli.Envf(env, "install: %v", err)
	}
	if !dryRun {
		unlock, err := lockInstallPaths(stateDir, prefix)
		if err != nil {
			return cli.Envf(env, "install: %v", err)
		}
		defer unlock()
	}
	goToolRoot := filepath.Join(stateDir, "go-tools")
	binaries, err := goToolDestinations(root, goToolRoot, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return cli.Envf(env, "install: %v", err)
	}
	wants, err := desiredLinks(root, binaries, dryRun)
	if err != nil {
		return cli.Envf(env, "install: %v", err)
	}
	if !dryRun {
		if err := validateGoTools(root, goToolRoot, runtime.GOOS, runtime.GOARCH, env.Getenv("PATH")); err != nil {
			return cli.Envf(env, "install: %v", err)
		}
	}
	rpath := filepath.Join(stateDir, ReceiptFile)
	prev := map[string]Link{}
	if r, err := loadReceipt(rpath); err == nil {
		for _, l := range r.Links {
			prev[l.Path] = l
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return cli.Envf(env, "install: %v", err)
	}

	var links []Link
	var collisions []string
	wanted := map[string]bool{}
	for _, w := range wants {
		lp := filepath.Join(prefix, w.name)
		wanted[lp] = true
		l := Link{Name: w.name, Path: lp, Target: w.target}
		info, err := os.Lstat(lp)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			l.Action = "created"
		case err != nil:
			return cli.Envf(env, "install: %v", err)
		case info.Mode()&fs.ModeSymlink != 0:
			dest, err := readLinkAbs(lp)
			if err != nil {
				return cli.Envf(env, "install: %v", err)
			}
			switch {
			case dest == w.target:
				l.Action = "unchanged"
			case owned(dest, root, prev, lp):
				l.Action = "updated"
			default:
				collisions = append(collisions, fmt.Sprintf("%s: symlink to %s, outside the agent-dry root", lp, dest))
				continue
			}
		default:
			collisions = append(collisions, fmt.Sprintf("%s: exists and is not a symlink into the agent-dry root", lp))
			continue
		}
		if other := shadowing(w.name, w.target, prefix, env.Getenv("PATH")); other != "" {
			collisions = append(collisions, fmt.Sprintf("%s: another %s is on PATH at %s", lp, w.name, other))
			continue
		}
		if binaries[w.name] == "" {
			sum, err := sha256File(w.target)
			if err != nil {
				return cli.Envf(env, "install: %v", err)
			}
			l.SHA256 = sum
		}
		links = append(links, l)
	}
	if len(collisions) > 0 {
		for _, c := range collisions {
			fmt.Fprintf(env.Stderr, "agent-dry: install: collision: %s\n", c)
		}
		fmt.Fprintf(env.Stderr, "agent-dry: install: %d collision(s); nothing changed\n", len(collisions))
		return cli.ExitNegative
	}

	// Links recorded earlier that the current root no longer installs.
	var removed []Link
	stale := make([]string, 0, len(prev))
	for lp := range prev {
		if !wanted[lp] {
			stale = append(stale, lp)
		}
	}
	sort.Strings(stale)
	for _, lp := range stale {
		l := prev[lp]
		l.SHA256 = ""
		info, err := os.Lstat(lp)
		if err != nil || info.Mode()&fs.ModeSymlink == 0 {
			l.Action, l.Reason = "skipped", "no longer a symlink"
			removed = append(removed, l)
			continue
		}
		dest, err := readLinkAbs(lp)
		if err != nil || !owned(dest, root, prev, lp) {
			l.Action, l.Reason = "skipped", "points outside the agent-dry root"
			removed = append(removed, l)
			continue
		}
		l.Action = "removed"
		removed = append(removed, l)
	}

	receipt := Receipt{Schema: ReceiptSchema, Version: commands.Version, Root: root, Prefix: prefix,
		Platform: platform(), InstalledAt: time.Now().UTC().Format(time.RFC3339), Links: links}
	if dryRun {
		receipt.DryRun = true
		receipt.Removed = removed
		if err := writeJSON(env.Stdout, receipt); err != nil {
			return cli.Envf(env, "install: %v", err)
		}
		return cli.ExitOK
	}

	mutations := []string{rpath}
	for _, target := range binaries {
		mutations = append(mutations, target)
	}
	for _, link := range links {
		if link.Action != "unchanged" {
			mutations = append(mutations, link.Path)
		}
	}
	for _, link := range removed {
		if link.Action == "removed" {
			mutations = append(mutations, link.Path)
		}
	}
	tx, err := prepareInstall(mutations)
	if err != nil {
		return cli.Envf(env, "install: preflight: %v", err)
	}
	defer func() {
		if err := tx.finish(); err != nil {
			code = cli.Envf(env, "install: %v", err)
		}
	}()
	stagedGoTools, err := buildGoTools(root, goToolRoot, runtime.GOOS, runtime.GOARCH, env.Getenv("PATH"))
	if err != nil {
		return cli.Envf(env, "install: %v", err)
	}
	defer discardGoTools(stagedGoTools)
	stagedByName := map[string]string{}
	for _, tool := range stagedGoTools {
		stagedByName[tool.Name] = tool.Path
	}
	for i := range links {
		if staged := stagedByName[links[i].Name]; staged != "" {
			sum, err := sha256File(staged)
			if err != nil {
				return cli.Envf(env, "install: %v", err)
			}
			links[i].SHA256 = sum
		}
	}
	raw, err := marshalJSON(receipt)
	if err != nil {
		return cli.Envf(env, "install: %v", err)
	}
	if err := commitGoTools(stagedGoTools); err != nil {
		return cli.Envf(env, "install: commit Go tools: %v", err)
	}
	for _, l := range links {
		if l.Action == "unchanged" {
			continue
		}
		if err := replaceSymlink(l.Path, l.Target); err != nil {
			return cli.Envf(env, "install: %v", err)
		}
	}
	for _, l := range links {
		dest, err := readLinkAbs(l.Path)
		if err != nil || dest != l.Target {
			return cli.Envf(env, "install: verification failed for %s", l.Path)
		}
	}
	for _, l := range removed {
		if l.Action != "removed" {
			continue
		}
		if err := os.Remove(l.Path); err != nil {
			return cli.Envf(env, "install: remove %s: %v", l.Path, err)
		}
	}
	if err := installReceiptWrite(rpath, raw, 0o644); err != nil {
		return cli.Envf(env, "install: write receipt: %v", err)
	}
	tx.committed = true
	receipt.File = rpath
	receipt.Removed = removed
	if err := writeJSON(env.Stdout, receipt); err != nil {
		return cli.Envf(env, "install: %v", err)
	}
	if !onPath(prefix, env.Getenv("PATH")) {
		fmt.Fprintf(env.Stderr, "agent-dry: install: %s is not on PATH; add it to your shell profile\n", prefix)
	}
	return cli.ExitOK
}

// replaceSymlink points path at target atomically (a new link renamed over the old one).
func replaceSymlink(path, target string) error {
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".agent-dry-*")
	if err != nil {
		return err
	}
	tmp := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Remove(tmp); err != nil {
		return err
	}
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func onPath(dir, pathEnv string) bool {
	real := realOrSelf(dir)
	for _, d := range filepath.SplitList(pathEnv) {
		if d == "" {
			continue
		}
		if filepath.Clean(d) == dir || realOrSelf(d) == real {
			return true
		}
	}
	return false
}

// uninstallResult is printed by install --uninstall.
type uninstallResult struct {
	Schema  string `json:"schema"`
	DryRun  bool   `json:"dry_run,omitempty"`
	Receipt string `json:"receipt"`
	Removed []Link `json:"removed"`
	Skipped []Link `json:"skipped"`
}

func uninstallLinks(env *cli.Env, dryRun bool, prefix string) (code int) {
	home := paths.Home(env.Getenv)
	stateDir, err := stateDirectory(env.Getenv)
	if err != nil {
		return cli.Envf(env, "install: %v", err)
	}
	prefix, err = resolvedPath(prefix, home)
	if err != nil {
		return cli.Envf(env, "install: %v", err)
	}
	rpath := filepath.Join(stateDir, ReceiptFile)
	var unlock func()
	if !dryRun {
		unlock, err = lockInstallPaths(stateDir, prefix)
		if err != nil {
			return cli.Envf(env, "install: %v", err)
		}
		defer func() {
			if unlock != nil {
				unlock()
			}
		}()
	}
	r, err := loadReceipt(rpath)
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(env.Stderr, "agent-dry: install: no receipt at %s; nothing to uninstall\n", rpath)
		return cli.ExitOK
	}
	if err != nil {
		return cli.Envf(env, "install: %v", err)
	}
	if !dryRun {
		actualPrefix, err := resolvedPath(r.Prefix, home)
		if err != nil {
			return cli.Envf(env, "install: receipt prefix: %v", err)
		}
		if actualPrefix != prefix {
			expectedPrefix := actualPrefix
			// The first snapshot only routes to the recorded prefix. Discard it
			// and bind every authoritative field again under both final locks.
			unlock()
			unlock = nil
			unlock, err = lockInstallPaths(stateDir, actualPrefix)
			if err != nil {
				return cli.Envf(env, "install: %v", err)
			}
			r, err = loadReceipt(rpath)
			if err != nil {
				return cli.Envf(env, "install: %v", err)
			}
			actualPrefix, err = resolvedPath(r.Prefix, home)
			if err != nil || actualPrefix != expectedPrefix {
				return cli.Envf(env, "install: receipt prefix changed during uninstall")
			}
		}
	}
	roots := []string{r.Root}
	if cur, err := distRoot(env.Getenv); err == nil {
		if cur != r.Root {
			roots = append(roots, cur)
		}
	}
	res := uninstallResult{Schema: ReceiptSchema, DryRun: dryRun, Receipt: rpath, Removed: []Link{}, Skipped: []Link{}}
	var keep []Link
	for _, l := range r.Links {
		info, err := os.Lstat(l.Path)
		if err != nil || info.Mode()&fs.ModeSymlink == 0 {
			l.Action, l.Reason = "skipped", "missing or not a symlink"
			res.Skipped = append(res.Skipped, l)
			continue
		}
		dest, err := readLinkAbs(l.Path)
		ours := err == nil && dest == l.Target
		for _, root := range roots {
			ours = ours || (err == nil && within(root, dest))
		}
		if !ours {
			l.Action, l.Reason = "skipped", "points outside the agent-dry root"
			res.Skipped = append(res.Skipped, l)
			continue
		}
		l.Action = "removed"
		res.Removed = append(res.Removed, l)
	}
	if !dryRun {
		mutations := []string{rpath}
		for _, link := range res.Removed {
			mutations = append(mutations, link.Path)
		}
		tx, err := prepareInstall(mutations)
		if err != nil {
			return cli.Envf(env, "install: preflight: %v", err)
		}
		defer func() {
			if err := tx.finish(); err != nil {
				code = cli.Envf(env, "install: %v", err)
			}
		}()
		removed := res.Removed[:0]
		for _, link := range res.Removed {
			if err := installLinkRemove(link.Path); err != nil {
				link.Action, link.Reason = "skipped", err.Error()
				res.Skipped = append(res.Skipped, link)
				keep = append(keep, link)
				continue
			}
			removed = append(removed, link)
		}
		res.Removed = removed
		if len(keep) == 0 {
			if err := removeFileSync(rpath); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return cli.Envf(env, "install: %v", err)
			}
		} else {
			r.Links = keep
			raw, err := marshalJSON(r)
			if err != nil {
				return cli.Envf(env, "install: %v", err)
			}
			if err := installReceiptWrite(rpath, raw, 0o644); err != nil {
				return cli.Envf(env, "install: write receipt: %v", err)
			}
		}
		tx.committed = true
	}
	if err := writeJSON(env.Stdout, res); err != nil {
		return cli.Envf(env, "install: %v", err)
	}
	return cli.ExitOK
}
