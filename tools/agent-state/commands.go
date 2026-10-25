package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type classSize struct {
	Bytes int64 `json:"bytes"`
	Files int   `json:"files"`
}
type projectSize struct {
	Classes map[string]classSize `json:"classes"`
	Total   int64                `json:"total_bytes"`
}
type reportData struct {
	Root               string                 `json:"root"`
	TotalBytes         int64                  `json:"total_bytes"`
	Projects           map[string]projectSize `json:"projects"`
	ClassBytes         map[string]int64       `json:"class_bytes"`
	ClassFiles         map[string]int         `json:"class_files"`
	ProjectBudgetBytes int64                  `json:"project_budget_bytes"`
	WarnTotalBytes     int64                  `json:"warn_total_bytes"`
	FailTotalBytes     int64                  `json:"fail_total_bytes"`
	Status             string                 `json:"status"`
}

func reportCommand(cfg config, args []string, stdout, stderr io.Writer) int {
	o, err := parseOptions(args, []string{"brief", "json"}, []string{"project"})
	if err != nil {
		return parseErr(stderr, err)
	}
	if len(o.pos) != 0 {
		return usageError(stderr, "report accepts options only")
	}
	if _, err := safeRootDevice(cfg.Root); err != nil {
		fmt.Fprintf(stderr, "%s: state root is unavailable or unsafe\n", prog)
		return exitEnv
	}
	data, err := inventory(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "%s: inventory: %v\n", prog, err)
		return exitEnv
	}
	if p := o.values["project"]; p != "" {
		if _, ok := data.Projects[p]; !ok {
			fmt.Fprintf(stderr, "%s: project not found\n", prog)
			return exitNegative
		}
		selected := data.Projects[p]
		data.TotalBytes = selected.Total
		data.ClassBytes = map[string]int64{}
		data.ClassFiles = map[string]int{}
		for class, size := range selected.Classes {
			data.ClassBytes[class] = size.Bytes
			data.ClassFiles[class] = size.Files
		}
		data.Projects = map[string]projectSize{p: selected}
	}
	projectOver := false
	for _, project := range data.Projects {
		if project.Total > cfg.ProjectBudget {
			projectOver = true
		}
	}
	if data.TotalBytes > cfg.FailTotal {
		data.Status = "fail"
	} else if data.TotalBytes > cfg.WarnTotal || projectOver {
		data.Status = "warn"
	} else {
		data.Status = "ok"
	}
	if o.bools["json"] {
		if err := emitJSON(stdout, data); err != nil {
			return exitEnv
		}
		if data.Status == "fail" {
			return exitNegative
		}
		return exitOK
	}
	if o.bools["brief"] {
		fmt.Fprintf(stdout, "%s total=%s GiB projects=%d project-budget=%s GiB warn=%s GiB fail=%s GiB\n", data.Status, bytesGiB(data.TotalBytes), len(data.Projects), bytesGiB(cfg.ProjectBudget), bytesGiB(cfg.WarnTotal), bytesGiB(cfg.FailTotal))
		if data.Status == "fail" {
			return exitNegative
		}
		return exitOK
	}
	fmt.Fprintf(stdout, "agent-execution: %s (%s GiB total, %s GiB warn, %s GiB fail)\n", data.Root, bytesGiB(data.TotalBytes), bytesGiB(cfg.WarnTotal), bytesGiB(cfg.FailTotal))
	projects := make([]string, 0, len(data.Projects))
	for name := range data.Projects {
		projects = append(projects, name)
	}
	sort.Strings(projects)
	fmt.Fprintf(stdout, "%-32s %10s %10s %s\n", "PROJECT", "TOTAL_GiB", "BUDGET_GiB", "CLASS_GiB")
	for _, name := range projects {
		p := data.Projects[name]
		classes := make([]string, 0, len(p.Classes))
		for c := range p.Classes {
			classes = append(classes, c)
		}
		sort.Strings(classes)
		var parts []string
		for _, c := range classes {
			parts = append(parts, c+"="+bytesGiB(p.Classes[c].Bytes))
		}
		warning := ""
		if p.Total > cfg.ProjectBudget {
			warning = " OVER_BUDGET"
		}
		fmt.Fprintf(stdout, "%-32s %10s %10s %s%s\n", name, bytesGiB(p.Total), bytesGiB(cfg.ProjectBudget), strings.Join(parts, " "), warning)
	}
	if o.values["project"] == "" {
		classes := make([]string, 0, len(data.ClassBytes))
		for class := range data.ClassBytes {
			classes = append(classes, class)
		}
		sort.Strings(classes)
		fmt.Fprintln(stdout, "class totals:")
		for _, class := range classes {
			fmt.Fprintf(stdout, "  %s files=%d size=%s GiB\n", class, data.ClassFiles[class], bytesGiB(data.ClassBytes[class]))
		}
	}
	if data.Status == "fail" {
		return exitNegative
	}
	if data.Status == "warn" {
		fmt.Fprintf(stdout, "warning: total exceeds the soft limit\n")
	}
	return exitOK
}

func inventory(cfg config) (reportData, error) {
	rootDevice, err := safeRootDevice(cfg.Root)
	if err != nil {
		return reportData{}, err
	}
	out := reportData{Root: cfg.Root, Projects: map[string]projectSize{}, ClassBytes: map[string]int64{}, ClassFiles: map[string]int{}, ProjectBudgetBytes: cfg.ProjectBudget, WarnTotalBytes: cfg.WarnTotal, FailTotalBytes: cfg.FailTotal, Status: "ok"}
	err = filepath.WalkDir(cfg.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == cfg.Root {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
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
			return errors.New("inventory crosses a filesystem boundary")
		}
		rel, err := filepath.Rel(cfg.Root, path)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) > 0 && parts[0] == "_registry" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		project, class := parts[0], "unmanaged"
		if project == "_cas" {
			project, class = "_cas", "cas"
		} else if len(parts) >= 2 {
			switch parts[1] {
			case "evidence":
				class = "evidence"
			case "scratch":
				class = "scratch"
			case "ledger-history":
				class = "ledger"
			default:
				if strings.HasSuffix(parts[1], ".json") {
					class = "ledger"
				}
			}
		}
		p := out.Projects[project]
		if p.Classes == nil {
			p.Classes = map[string]classSize{}
		}
		cs := p.Classes[class]
		cs.Bytes += info.Size()
		cs.Files++
		p.Classes[class] = cs
		p.Total += info.Size()
		out.Projects[project] = p
		out.ClassBytes[class] += info.Size()
		out.ClassFiles[class]++
		out.TotalBytes += info.Size()
		return nil
	})
	return out, err
}

var forbiddenDirs = map[string]bool{
	"node_modules": true, "target": true, "cargo-home": true, ".cargo": true, ".rustup": true,
	".next": true, "dist": true, "build": true, "out": true, "runtime-tmp": true, "go-tmp": true,
	".turbo": true, "pgdata": true, "postgres-data": true, "postgres": true, "mysql": true,
	"mongodata": true, "redis-data": true, "source": false, "src": false, "repo": false, "checkout": false,
}
var binaryMagic = [][]byte{{0x7f, 'E', 'L', 'F'}, {0xfe, 0xed, 0xfa, 0xce}, {0xce, 0xfa, 0xed, 0xfe}, {0xcf, 0xfa, 0xed, 0xfe}, {0xfe, 0xed, 0xfa, 0xcf}, {0xca, 0xfe, 0xba, 0xbe}, {'M', 'Z'}}

type lintFinding struct {
	Path string `json:"path"`
	Rule string `json:"rule"`
}

func evidenceDirs(cfg config, only string) ([]string, error) {
	rootDevice, err := safeRootDevice(cfg.Root)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(cfg.Root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.Type()&fs.ModeSymlink != 0 {
			continue
		}
		if !e.IsDir() || e.Name() == "_registry" || e.Name() == "_cas" {
			continue
		}
		if only != "" && e.Name() != only {
			continue
		}
		project := filepath.Join(cfg.Root, e.Name())
		projectInfo, err := os.Lstat(project)
		if err != nil {
			return nil, err
		}
		projectDevice, err := fileDevice(projectInfo)
		if err != nil || projectDevice != rootDevice {
			return nil, errors.New("evidence scan crossed a filesystem boundary")
		}
		p := filepath.Join(project, "evidence")
		st, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if st.Mode()&fs.ModeSymlink != 0 {
			return nil, errors.New("evidence directory is a symlink")
		}
		if !st.IsDir() {
			continue
		}
		device, err := fileDevice(st)
		if err != nil || device != rootDevice {
			return nil, errors.New("evidence scan crossed a filesystem boundary")
		}
		out = append(out, p)
	}
	return out, nil
}

func lintEvidence(path string) ([]lintFinding, error) {
	var findings []lintFinding
	sourceContainer := map[string]bool{"source": true, "src": true, "repo": true, "checkout": true, "vendor": true}
	root := path
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			findings = append(findings, lintFinding{Path: p, Rule: "unreadable"})
			return nil
		}
		if p == root {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		name := strings.ToLower(d.Name())
		if d.IsDir() {
			if forbiddenDirs[name] {
				findings = append(findings, lintFinding{Path: p, Rule: "forbidden_directory"})
			}
			if sourceContainer[name] && containsSourceMarker(p) {
				findings = append(findings, lintFinding{Path: p, Rule: "source_copy"})
			}
			if name == "data" && isDatabaseDataDir(p) {
				findings = append(findings, lintFinding{Path: p, Rule: "database_data"})
			}
			if name == ".git" {
				findings = append(findings, lintFinding{Path: p, Rule: "nested_git"})
				return filepath.SkipDir
			}
			return nil
		}
		if name == ".git" {
			findings = append(findings, lintFinding{Path: p, Rule: "nested_git"})
			return nil
		}
		if name == "postmaster.pid" || name == "ibdata1" || name == "mongod.lock" {
			findings = append(findings, lintFinding{Path: p, Rule: "database_data"})
		}
		if strings.HasSuffix(name, ".test") || strings.HasSuffix(name, ".exe") || strings.HasSuffix(name, ".dll") || strings.HasSuffix(name, ".so") || strings.HasSuffix(name, ".dylib") || strings.HasSuffix(name, ".o") || strings.HasSuffix(name, ".a") || strings.HasSuffix(name, ".class") || strings.HasSuffix(name, ".wasm") {
			findings = append(findings, lintFinding{Path: p, Rule: "compiled_binary"})
		}
		if strings.HasSuffix(name, ".tar") || strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz") {
			findings = append(findings, lintFinding{Path: p, Rule: "input_archive"})
		}
		info, err := d.Info()
		if err != nil {
			findings = append(findings, lintFinding{Path: p, Rule: "unreadable"})
			return nil
		}
		if info.Size() > 25*1024*1024 {
			findings = append(findings, lintFinding{Path: p, Rule: "file_over_25_mib"})
		}
		if info.Size() >= 4 {
			f, err := os.Open(p)
			if err != nil {
				findings = append(findings, lintFinding{Path: p, Rule: "unreadable"})
				return nil
			}
			buf := make([]byte, 4)
			_, _ = io.ReadFull(f, buf)
			f.Close()
			for _, magic := range binaryMagic {
				if bytes.HasPrefix(buf, magic) {
					findings = append(findings, lintFinding{Path: p, Rule: "compiled_binary"})
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		return findings, err
	}
	return findings, nil
}

func isDatabaseDataDir(path string) bool {
	for _, marker := range []string{"PG_VERSION", "pg_wal", "ibdata1", "WiredTiger", "dump.rdb", "appendonly.aof"} {
		if _, err := os.Stat(filepath.Join(path, marker)); err == nil {
			return true
		}
	}
	return false
}

func scanSecrets(path string) (bool, error) {
	bin, err := exec.LookPath("gitleaks")
	if err != nil {
		return false, err
	}
	cmd := exec.Command(bin, "dir", "--no-banner", "--redact", "--no-color", path)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	err = cmd.Run()
	if err == nil {
		return false, nil
	}
	if status, ok := err.(*exec.ExitError); ok && status.ExitCode() == 1 {
		return true, nil
	}
	return false, errors.New("gitleaks execution failed")
}

func archiveEvidenceReasons(bundle string, lint func(string) ([]lintFinding, error), scan func(string) (bool, error)) []string {
	var reasons []string
	findings, err := lint(bundle)
	if err != nil {
		reasons = append(reasons, "evidence_lint_unavailable")
	} else {
		for _, finding := range findings {
			reasons = append(reasons, "evidence_lint_"+finding.Rule)
		}
	}
	secretFound, err := scan(bundle)
	if err != nil {
		reasons = append(reasons, "secret_scan_unavailable")
	} else if secretFound {
		reasons = append(reasons, "secret_finding_redacted")
	}
	return unique(reasons)
}

var planEvidenceReasons = func(bundle string) []string {
	return archiveEvidenceReasons(bundle, lintEvidence, scanSecrets)
}

func containsSourceMarker(root string) bool {
	found := false
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if found || err != nil {
			return filepath.SkipAll
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if d.Name() == ".git" || d.Name() == "go.mod" || d.Name() == "Cargo.toml" || d.Name() == "package.json" || d.Name() == "pyproject.toml" {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

func lintCommand(cfg config, args []string, stdout, stderr io.Writer) int {
	o, err := parseOptions(args, []string{"json"}, []string{"project"})
	if err != nil {
		return parseErr(stderr, err)
	}
	if len(o.pos) != 0 {
		return usageError(stderr, "lint accepts options only")
	}
	if p := o.values["project"]; p != "" {
		if _, err := projectRoot(cfg, p); err != nil {
			return usageError(stderr, err.Error())
		}
	}
	dirs, err := evidenceDirs(cfg, o.values["project"])
	if err != nil {
		fmt.Fprintf(stderr, "%s: lint: %v\n", prog, err)
		return exitEnv
	}
	var all []lintFinding
	for _, dir := range dirs {
		findings, err := lintEvidence(dir)
		if err != nil {
			fmt.Fprintf(stderr, "%s: lint: unreadable evidence\n", prog)
			return exitEnv
		}
		all = append(all, findings...)
		secretFound, err := scanSecrets(dir)
		if err != nil {
			fmt.Fprintf(stderr, "%s: lint: gitleaks is unavailable or failed\n", prog)
			return exitEnv
		}
		if secretFound {
			all = append(all, lintFinding{Path: dir, Rule: "secret_finding_redacted"})
		}
	}
	if o.bools["json"] {
		if err := emitJSON(stdout, all); err != nil {
			return exitEnv
		}
	} else if len(all) == 0 {
		fmt.Fprintln(stdout, "lint: no forbidden evidence content found")
	} else {
		for _, item := range all {
			fmt.Fprintf(stdout, "%s: %s\n", item.Rule, item.Path)
		}
	}
	if len(all) != 0 {
		return exitNegative
	}
	return exitOK
}

func pinCommand(cfg config, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usageError(stderr, "pin requires add, rm or list")
	}
	sub := args[0]
	if sub == "list" {
		if len(args) != 1 {
			return usageError(stderr, "pin list accepts no arguments")
		}
		idx, err := loadIndex(cfg)
		if err != nil {
			fmt.Fprintf(stderr, "%s: registry: %v\n", prog, err)
			return exitEnv
		}
		sort.Slice(idx.Pins, func(i, j int) bool { return idx.Pins[i].Path < idx.Pins[j].Path })
		for _, p := range idx.Pins {
			fmt.Fprintf(stdout, "%s\t%s\n", p.Path, p.Reason)
		}
		return exitOK
	}
	if sub == "add" {
		o, err := parseOptions(args[1:], nil, []string{"reason"})
		if err != nil {
			return parseErr(stderr, err)
		}
		if len(o.pos) != 1 || strings.TrimSpace(o.values["reason"]) == "" {
			return usageError(stderr, "pin add <path> --reason <text>")
		}
		p, err := cleanPath(o.pos[0])
		if err != nil {
			return usageError(stderr, err.Error())
		}
		return withIndexLock(cfg, func(idx *registryIndex) int {
			if hasPin(idx.Pins, p) {
				fmt.Fprintln(stdout, "pin already exists")
				return exitOK
			}
			idx.Pins = append(idx.Pins, pin{Path: p, Reason: o.values["reason"], Added: time.Now().UTC().Format(time.RFC3339)})
			if err := saveIndex(cfg, *idx); err != nil {
				fmt.Fprintf(stderr, "%s: save pin: %v\n", prog, err)
				return exitEnv
			}
			fmt.Fprintf(stdout, "pinned %s\n", p)
			return exitOK
		})
	}
	if sub == "rm" {
		o, err := parseOptions(args[1:], nil, nil)
		if err != nil {
			return parseErr(stderr, err)
		}
		if len(o.pos) != 1 {
			return usageError(stderr, "pin rm <path>")
		}
		p, err := cleanPath(o.pos[0])
		if err != nil {
			return usageError(stderr, err.Error())
		}
		return withIndexLock(cfg, func(idx *registryIndex) int {
			for i, value := range idx.Pins {
				if value.Path == p {
					idx.Pins = append(idx.Pins[:i], idx.Pins[i+1:]...)
					if err := saveIndex(cfg, *idx); err != nil {
						return exitEnv
					}
					fmt.Fprintf(stdout, "unpinned %s\n", p)
					return exitOK
				}
			}
			fmt.Fprintln(stderr, "agent-state: pin not found")
			return exitNegative
		})
	}
	return usageError(stderr, "pin requires add, rm or list")
}

func doctorCommand(cfg config, args []string, stdout, stderr io.Writer) int {
	o, err := parseOptions(args, []string{"json"}, nil)
	if err != nil {
		return parseErr(stderr, err)
	}
	if len(o.pos) != 0 {
		return usageError(stderr, "doctor accepts no arguments")
	}
	checks := map[string]string{"root": "ok", "registry": "ok", "git": "missing", "repo-state": "missing", "lsof": "missing", "restic": "missing", "bao-run": "missing"}
	if st, err := os.Stat(cfg.Root); err != nil || !st.IsDir() {
		checks["root"] = "missing"
	}
	if _, err := os.Stat(registryPath(cfg)); err != nil && !errors.Is(err, os.ErrNotExist) {
		checks["registry"] = "unreadable"
	}
	for _, name := range []string{"git", "repo-state", "lsof", "restic", "bao-run"} {
		if _, err := exec.LookPath(name); err == nil {
			checks[name] = "ok"
		}
	}
	checks["archive_repo"] = "not_configured"
	if cfg.Archive.Repo != "" {
		checks["archive_repo"] = "configured"
	}
	result := map[string]any{"checks": checks, "scratch_ttl": cfg.ScratchTTL, "evidence_grace": cfg.EvidenceGrace, "active_ledger_days": cfg.ActiveLedgerDays, "archive_retention_months": cfg.Archive.RetentionMonths, "ledger_retention": "forever"}
	if o.bools["json"] {
		_ = emitJSON(stdout, result)
	} else {
		for _, k := range []string{"root", "registry", "git", "repo-state", "lsof", "restic", "bao-run", "archive_repo"} {
			fmt.Fprintf(stdout, "%s: %s\n", k, checks[k])
		}
		fmt.Fprintf(stdout, "policy: scratch=%s evidence-grace=%s active-ledger=%dd archive-retention=%d months ledgers=forever\n", cfg.ScratchTTL, cfg.EvidenceGrace, cfg.ActiveLedgerDays, cfg.Archive.RetentionMonths)
	}
	if checks["root"] != "ok" {
		return exitEnv
	}
	return exitOK
}

func ledgerCommand(cfg config, args []string, stdout, stderr io.Writer) int {
	if len(args) != 3 || args[0] != "path" {
		return usageError(stderr, "ledger path <project> <goal-id>")
	}
	if !validateName(args[2]) {
		return usageError(stderr, "invalid goal id")
	}
	p, err := projectRoot(cfg, args[1])
	if err != nil {
		return usageError(stderr, err.Error())
	}
	fmt.Fprintln(stdout, filepath.Join(p, args[2]+".json"))
	return exitOK
}

func scratchCommand(cfg config, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usageError(stderr, "scratch requires new, renew or release")
	}
	switch args[0] {
	case "new":
		o, err := parseOptions(args[1:], []string{"env"}, []string{"ttl"})
		if err != nil {
			return parseErr(stderr, err)
		}
		if len(o.pos) != 1 {
			return usageError(stderr, "scratch new <project> [--ttl 3d] [--env]")
		}
		project, err := projectRoot(cfg, o.pos[0])
		if err != nil {
			return usageError(stderr, err.Error())
		}
		ttl := parseDuration(cfg.ScratchTTL)
		if value := o.values["ttl"]; value != "" {
			ttl = parseDuration(value)
			if ttl <= 0 {
				return usageError(stderr, "--ttl must be a positive duration such as 72h or 3d")
			}
		}
		id, err := newID("lease-")
		if err != nil {
			return exitEnv
		}
		path := filepath.Join(project, "scratch", id)
		if !inside(cfg.Root, path) {
			return exitUsage
		}
		return withIndexLock(cfg, func(idx *registryIndex) int {
			if err := os.MkdirAll(path, 0o700); err != nil {
				return exitEnv
			}
			now := time.Now().UTC()
			l := lease{ID: id, Project: o.pos[0], Path: path, CreatedAt: now.Format(time.RFC3339), RenewedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(ttl).Format(time.RFC3339)}
			idx.Leases = append(idx.Leases, l)
			if err := saveIndex(cfg, *idx); err != nil {
				return exitEnv
			}
			fmt.Fprintln(stdout, path)
			if o.bools["env"] {
				fmt.Fprintf(stdout, "TMPDIR=%s\nGOTMPDIR=%s\nCARGO_TARGET_DIR=%s\nPGDATA=%s\n", path, path, filepath.Join(path, "target"), filepath.Join(path, "pgdata"))
			}
			return exitOK
		})
	case "renew", "release":
		o, err := parseOptions(args[1:], nil, []string{"ttl"})
		if err != nil {
			return parseErr(stderr, err)
		}
		if len(o.pos) != 1 {
			return usageError(stderr, "scratch renew|release <lease-id> [--ttl 3d]")
		}
		id := o.pos[0]
		if !validateName(id) {
			return usageError(stderr, "invalid lease id")
		}
		return withIndexLock(cfg, func(idx *registryIndex) int {
			for i := range idx.Leases {
				l := &idx.Leases[i]
				if l.ID != id {
					continue
				}
				if l.ReleasedAt != "" {
					fmt.Fprintln(stderr, "agent-state: lease already released")
					return exitNegative
				}
				now := time.Now().UTC()
				if args[0] == "release" {
					l.ReleasedAt = now.Format(time.RFC3339)
				} else {
					ttl := parseDuration(cfg.ScratchTTL)
					if value := o.values["ttl"]; value != "" {
						ttl = parseDuration(value)
						if ttl <= 0 {
							return exitUsage
						}
					}
					l.RenewedAt = now.Format(time.RFC3339)
					l.ExpiresAt = now.Add(ttl).Format(time.RFC3339)
				}
				if err := saveIndex(cfg, *idx); err != nil {
					return exitEnv
				}
				fmt.Fprintf(stdout, "%s %s\n", args[0], id)
				return exitOK
			}
			fmt.Fprintln(stderr, "agent-state: lease not found")
			return exitNegative
		})
	default:
		return usageError(stderr, "scratch requires new, renew or release")
	}
}

var sourcePattern = regexp.MustCompile(`^(.+)@([0-9a-fA-F]{40})$`)
var sourceRepoPattern = regexp.MustCompile(`^[A-Za-z0-9.-]+/[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)

func evidenceCommand(cfg config, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usageError(stderr, "evidence requires new, put or close")
	}
	switch args[0] {
	case "new":
		o, err := parseOptions(args[1:], nil, []string{"goal", "criterion", "source"})
		if err != nil {
			return parseErr(stderr, err)
		}
		if len(o.pos) != 1 || o.values["goal"] == "" || o.values["criterion"] == "" {
			return usageError(stderr, "evidence new <project> --goal <id> --criterion <text> [--source <repo>@<sha>]")
		}
		project, err := projectRoot(cfg, o.pos[0])
		if err != nil {
			return usageError(stderr, err.Error())
		}
		if !validateName(o.values["goal"]) {
			return usageError(stderr, "invalid goal id")
		}
		var sources []sourceRef
		if s := o.values["source"]; s != "" {
			m := sourcePattern.FindStringSubmatch(s)
			if m == nil || !sourceRepoPattern.MatchString(m[1]) {
				return usageError(stderr, "--source must be <host>/<owner>/<repo>@<40-hex-commit>")
			}
			sources = append(sources, sourceRef{Repo: m[1], Commit: strings.ToLower(m[2])})
		}
		id, err := newID("evidence-")
		if err != nil {
			return exitEnv
		}
		path := filepath.Join(project, "evidence", id)
		now := time.Now().UTC().Format(time.RFC3339)
		m := evidenceManifest{Schema: evidenceSchema, ID: id, Project: o.pos[0], GoalID: o.values["goal"], Criterion: o.values["criterion"], Status: "open", CreatedAt: now, LastUsedAt: now, Sources: sources}
		if err := os.MkdirAll(path, 0o700); err != nil {
			return exitEnv
		}
		if err := writeJSONAtomic(filepath.Join(path, "manifest.json"), m, 0o600); err != nil {
			return exitEnv
		}
		fmt.Fprintln(stdout, path)
		return exitOK
	case "put":
		o, err := parseOptions(args[1:], nil, []string{"role"})
		if err != nil {
			return parseErr(stderr, err)
		}
		if len(o.pos) != 2 {
			return usageError(stderr, "evidence put <bundle> <file> --role receipt|log|screenshot|patch|blob")
		}
		role := o.values["role"]
		allowed := map[string]string{"receipt": "receipts", "log": "logs", "screenshot": "screenshots", "patch": "patches", "blob": "blobs"}
		if allowed[role] == "" {
			return usageError(stderr, "invalid evidence role")
		}
		bundle, err := cleanPath(o.pos[0])
		if err != nil || !inside(cfg.Root, bundle) {
			return usageError(stderr, "bundle must be inside the configured state root")
		}
		m, err := readManifest(bundle)
		if err != nil || m.Status != "open" {
			fmt.Fprintln(stderr, "agent-state: bundle is not an open evidence bundle")
			return exitNegative
		}
		src, err := os.Open(o.pos[1])
		if err != nil {
			return exitNegative
		}
		defer src.Close()
		info, err := src.Stat()
		if err != nil || !info.Mode().IsRegular() {
			return usageError(stderr, "evidence input must be a regular file")
		}
		if role == "screenshot" && info.Size() > 1024*1024 {
			return usageError(stderr, "screenshots are limited to 1 MiB")
		}
		if role == "screenshot" && countRole(m.Files, "screenshot") >= 20 {
			return usageError(stderr, "a bundle is limited to 20 screenshots")
		}
		if info.Size() > 25*1024*1024 && role != "blob" {
			return usageError(stderr, "files over 25 MiB must use --role blob")
		}
		if role == "blob" {
			sum, n, err := hashFile(o.pos[1])
			if err != nil {
				return exitEnv
			}
			cas := rootPath(cfg, "_cas", "sha256", sum[:2], sum)
			if _, err := os.Stat(cas); errors.Is(err, os.ErrNotExist) {
				if err := os.MkdirAll(filepath.Dir(cas), 0o700); err != nil {
					return exitEnv
				}
				if err := copyFile(cas, o.pos[1], 0o600); err != nil {
					return exitEnv
				}
			} else if err != nil {
				return exitEnv
			} else {
				existing, bytes, err := hashFile(cas)
				if err != nil || existing != sum || bytes != n {
					return exitBlocked
				}
			}
			rel, err := filepath.Rel(cfg.Root, cas)
			if err != nil {
				return exitEnv
			}
			m.Blobs = append(m.Blobs, fileRecord{Path: filepath.ToSlash(rel), SHA256: sum, Bytes: n, Role: role})
		} else {
			dir := filepath.Join(bundle, allowed[role])
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return exitEnv
			}
			name := filepath.Base(o.pos[1])
			if !validateName(name) {
				name = "evidence-file"
			}
			dest := filepath.Join(dir, name)
			if !inside(bundle, dest) {
				return exitUsage
			}
			if _, err := os.Stat(dest); err == nil {
				return usageError(stderr, "file name already exists in bundle")
			}
			if err := copyFile(dest, o.pos[1], 0o600); err != nil {
				return exitEnv
			}
			sum, n, err := hashFile(dest)
			if err != nil {
				return exitEnv
			}
			m.Files = append(m.Files, fileRecord{Path: filepath.ToSlash(filepath.Join(allowed[role], name)), SHA256: sum, Bytes: n, Role: role})
		}
		setUsed(&m)
		if err := writeJSONAtomic(filepath.Join(bundle, "manifest.json"), m, 0o600); err != nil {
			return exitEnv
		}
		fmt.Fprintln(stdout, "evidence updated")
		return exitOK
	case "close":
		o, err := parseOptions(args[1:], nil, []string{"result"})
		if err != nil {
			return parseErr(stderr, err)
		}
		if len(o.pos) != 1 || o.values["result"] == "" {
			return usageError(stderr, "evidence close <bundle> --result pass|fail|setup-failure")
		}
		bundle, err := cleanPath(o.pos[0])
		if err != nil || !inside(cfg.Root, bundle) {
			return usageError(stderr, "bundle must be inside the configured state root")
		}
		result := o.values["result"]
		if result != "pass" && result != "fail" && result != "setup-failure" {
			return usageError(stderr, "invalid evidence result")
		}
		m, err := readManifest(bundle)
		if err != nil || m.Status != "open" {
			return exitNegative
		}
		if err := sealEvidenceFiles(bundle, &m); err != nil {
			fmt.Fprintln(stderr, "agent-state: evidence contains unreadable or unsafe content")
			return exitBlocked
		}
		now := time.Now().UTC().Format(time.RFC3339)
		m.Status = "closed"
		m.Result = result
		m.ClosedAt = now
		m.LastUsedAt = now
		if err := writeJSONAtomic(filepath.Join(bundle, "manifest.json"), m, 0o600); err != nil {
			return exitEnv
		}
		fmt.Fprintln(stdout, "evidence closed")
		return exitOK
	default:
		return usageError(stderr, "evidence requires new, put or close")
	}
}

func countRole(files []fileRecord, role string) int {
	n := 0
	for _, file := range files {
		if file.Role == role {
			n++
		}
	}
	return n
}

func sealEvidenceFiles(bundle string, manifest *evidenceManifest) error {
	rootDevice, err := safeRootDevice(bundle)
	if err != nil {
		return err
	}
	roles := make(map[string]string, len(manifest.Files))
	for _, file := range manifest.Files {
		roles[file.Path] = file.Role
	}
	var files []fileRecord
	err = filepath.WalkDir(bundle, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return errors.New("evidence bundle contains a symlink")
		}
		if path == filepath.Join(bundle, "manifest.json") || entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		device, err := fileDevice(info)
		if err != nil || device != rootDevice {
			return errors.New("evidence bundle crosses a filesystem boundary")
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		sha, size, err := hashFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(bundle, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		role := roles[name]
		if role == "" {
			role = "receipt"
		}
		files = append(files, fileRecord{Path: name, SHA256: sha, Bytes: size, Role: role})
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	manifest.Files = files
	return nil
}
func copyFile(dst, src string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(dst)
		return copyErr
	}
	if syncErr != nil {
		_ = os.Remove(dst)
		return syncErr
	}
	if closeErr != nil {
		_ = os.Remove(dst)
		return closeErr
	}
	return nil
}

func gcCommand(cfg config, args []string, stdout, stderr io.Writer) int {
	o, err := parseOptions(args, []string{"dry-run", "apply"}, []string{"class", "plan", "plan-dir"})
	if err != nil {
		return parseErr(stderr, err)
	}
	if len(o.pos) != 0 {
		return usageError(stderr, "gc accepts options only")
	}
	if code := checkPlanFlags(o, stderr); code != exitOK {
		return code
	}
	class := o.values["class"]
	if class != "" && class != "scratch" && class != "evidence" {
		return usageError(stderr, "--class must be scratch or evidence")
	}
	planDirectory, err := resolvePlanDirectory(cfg, o.values["plan-dir"])
	if err != nil {
		fmt.Fprintf(stderr, "%s: plan directory: %v\n", prog, err)
		return exitUsage
	}
	if o.bools["apply"] {
		return applyGC(cfg, o.values["plan"], planDirectory, stdout, stderr)
	}
	plan, err := buildPlan(cfg, class)
	if err != nil {
		fmt.Fprintf(stderr, "%s: gc plan: %v\n", prog, err)
		return exitEnv
	}
	if err := persistPlan(plan, planDirectory); err != nil {
		fmt.Fprintf(stderr, "%s: save plan: %v\n", prog, err)
		return exitEnv
	}
	path := filepath.Join(planDirectory, plan.ID+".json")
	fmt.Fprintf(stdout, "plan_id=%s scratch_expirable=%d/%s GiB archive_then_stub=%d/%s GiB blocked=%d blocked_bytes=%s GiB path=%s\n", plan.ID, countPlanClass(plan, "scratch", "delete_scratch"), bytesGiB(plan.ScratchBytes), countPlanClass(plan, "evidence", "archive_then_stub"), bytesGiB(plan.EvidenceBytes), len(plan.Blocked), bytesGiB(plan.BlockedBytes), path)
	for _, reason := range blockedReasonCounts(plan.Blocked) {
		fmt.Fprintf(stdout, "blocked_reason=%s count=%d\n", reason.Name, reason.Count)
	}
	return exitOK
}

func resolvePlanDirectory(cfg config, requested string) (string, error) {
	if requested == "" {
		return plansPath(cfg), nil
	}
	directory, err := cleanPath(requested)
	if err != nil {
		return "", err
	}
	if inside(cfg.Root, directory) {
		if err := validateExistingPath(cfg.Root, directory); err != nil {
			return "", err
		}
		return directory, nil
	}
	rootReal, err := canonicalPath(cfg.Root)
	if err != nil {
		return "", err
	}
	directoryReal, err := canonicalPath(directory)
	if err != nil {
		return "", err
	}
	if inside(rootReal, directoryReal) {
		return "", errors.New("plan directory resolves inside the state root")
	}
	return directoryReal, nil
}

func validatePlanDirectory(cfg config, directory string) error {
	if inside(cfg.Root, directory) {
		return validateExistingPath(cfg.Root, directory)
	}
	rootReal, err := canonicalPath(cfg.Root)
	if err != nil {
		return err
	}
	directoryReal, err := canonicalPath(directory)
	if err != nil {
		return err
	}
	if inside(rootReal, directoryReal) {
		return errors.New("plan directory resolves inside the state root")
	}
	return nil
}

func countPlanClass(plan gcPlan, class, action string) int {
	count := 0
	for _, entry := range plan.Candidates {
		if entry.Class == class && entry.Action == action {
			count++
		}
	}
	return count
}

type blockedReasonCount struct {
	Name  string
	Count int
}

func blockedReasonCounts(entries []planEntry) []blockedReasonCount {
	counts := map[string]int{}
	for _, entry := range entries {
		for _, reason := range entry.Reasons {
			counts[reason]++
		}
	}
	out := make([]blockedReasonCount, 0, len(counts))
	for reason, count := range counts {
		out = append(out, blockedReasonCount{Name: reason, Count: count})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func buildPlan(cfg config, class string) (gcPlan, error) {
	plan := gcPlan{Schema: planSchema, Root: cfg.Root, CreatedAt: time.Now().UTC().Format(time.RFC3339), Policy: planPolicy{ScratchTTL: cfg.ScratchTTL, EvidenceGrace: cfg.EvidenceGrace, ArchiveRetentionMonths: cfg.Archive.RetentionMonths, ActiveLedgerDays: cfg.ActiveLedgerDays}}
	if _, err := safeRootDevice(cfg.Root); err != nil {
		return plan, err
	}
	id, err := newID("plan-")
	if err != nil {
		return plan, err
	}
	plan.ID = id
	idx, err := loadIndex(cfg)
	if err != nil {
		return plan, err
	}
	ledgerRefs, err := collectActiveLedgerRefs(cfg)
	if err != nil {
		plan.Blocked = append(plan.Blocked, planEntry{Path: cfg.Root, Class: "system", Action: "gc", Reasons: []string{"ledger_scan_unavailable"}})
		if sealErr := sealPlan(&plan); sealErr != nil {
			return plan, sealErr
		}
		return plan, nil
	}
	open, openErr := safetyOpenPaths()
	if class == "" || class == "scratch" {
		for _, item := range idx.Leases {
			if item.ReleasedAt != "" {
				continue
			}
			expires, err := time.Parse(time.RFC3339, item.ExpiresAt)
			if err != nil {
				entry := planEntry{Path: item.Path, Class: "scratch", Action: "delete_scratch", Reasons: []string{"invalid_lease"}}
				if size, measureErr := measureTreeWithin(item.Path, cfg.Root); measureErr == nil {
					entry.Bytes = size
				} else {
					entry.Reasons = append(entry.Reasons, "unreadable_content")
				}
				plan.Blocked = append(plan.Blocked, entry)
				plan.BlockedBytes += entry.Bytes
				continue
			}
			if time.Now().Before(expires) {
				continue
			}
			entry := planEntry{Path: item.Path, Class: "scratch", Action: "delete_scratch"}
			bytes, measureErr := measureTreeWithin(item.Path, cfg.Root)
			entry.Bytes = bytes
			if measureErr != nil {
				entry.Reasons = append(entry.Reasons, "unreadable_content")
			}
			entry.Reasons = append(entry.Reasons, safetyReasonsWithRefs(cfg, idx, item.Path, open, openErr, ledgerRefs)...)
			if len(entry.Reasons) == 0 {
				fingerprint, fingerprintErr := treeFingerprint(item.Path)
				if fingerprintErr != nil {
					entry.Reasons = append(entry.Reasons, "unreadable_content")
				} else {
					entry.TreeSHA256 = fingerprint
				}
			}
			entry.Reasons = unique(entry.Reasons)
			if len(entry.Reasons) != 0 {
				plan.Blocked = append(plan.Blocked, entry)
				plan.BlockedBytes += entry.Bytes
			} else {
				plan.Candidates = append(plan.Candidates, entry)
				plan.ScratchBytes += entry.Bytes
			}
		}
	}
	if class == "" || class == "evidence" {
		dirs, err := evidenceDirs(cfg, "")
		if err != nil {
			return plan, err
		}
		grace := parseDuration(cfg.EvidenceGrace)
		for _, dir := range dirs {
			children, err := os.ReadDir(dir)
			if err != nil {
				return plan, err
			}
			for _, child := range children {
				bundle := filepath.Join(dir, child.Name())
				if child.Type()&fs.ModeSymlink != 0 {
					plan.Blocked = append(plan.Blocked, planEntry{Path: bundle, Class: "evidence", Action: "archive_then_stub", Reasons: []string{"symlink_path"}})
					continue
				}
				if !child.IsDir() {
					continue
				}
				entry := planEntry{Path: bundle, Class: "evidence", Action: "archive_then_stub"}
				bytes, measureErr := measureTreeWithin(bundle, cfg.Root)
				if measureErr != nil {
					entry.Bytes = bytes
					entry.Reasons = append(entry.Reasons, "unreadable_content")
					plan.Blocked = append(plan.Blocked, entry)
					plan.BlockedBytes += bytes
					continue
				}
				entry.Bytes = bytes
				manifest, manifestErr := readManifest(bundle)
				if manifestErr != nil {
					entry.Reasons = append(entry.Reasons, "unmanaged_no_manifest")
				} else {
					if manifest.Status != "closed" || manifest.ClosedAt == "" {
						continue
					}
					closed, parseErr := time.Parse(time.RFC3339, manifest.ClosedAt)
					if parseErr != nil {
						entry.Reasons = append(entry.Reasons, "invalid_closed_at")
					} else if time.Since(closed) < grace {
						continue
					}
					if manifest.Archive != nil && manifest.Archive.VerifiedAt != "" {
						entry.Reasons = append(entry.Reasons, "already_archived")
					}
					entry.Reasons = append(entry.Reasons, planEvidenceReasons(bundle)...)
					manifestHash, _, hashErr := hashFile(filepath.Join(bundle, "manifest.json"))
					if hashErr != nil {
						entry.Reasons = append(entry.Reasons, "unreadable_content")
					} else {
						entry.ManifestSHA256 = manifestHash
					}
				}
				entry.Reasons = append(entry.Reasons, safetyReasonsWithRefs(cfg, idx, bundle, open, openErr, ledgerRefs)...)
				entry.Reasons = unique(entry.Reasons)
				if len(entry.Reasons) != 0 {
					plan.Blocked = append(plan.Blocked, entry)
					plan.BlockedBytes += entry.Bytes
				} else {
					plan.Candidates = append(plan.Candidates, entry)
					plan.EvidenceBytes += entry.Bytes
				}
			}
		}
	}
	sortEntries(plan.Candidates)
	sortEntries(plan.Blocked)
	if err := sealPlan(&plan); err != nil {
		return plan, err
	}
	return plan, nil
}

func persistPlan(plan gcPlan, directory string) error {
	if err := validatePlan(plan); err != nil {
		return err
	}
	if err := validatePlanDirectory(config{Root: plan.Root}, directory); err != nil {
		return err
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	return writeJSONAtomic(filepath.Join(directory, plan.ID+".json"), plan, 0o600)
}

func loadPlan(cfg config, id, directory string) (gcPlan, error) {
	var plan gcPlan
	if !strings.HasPrefix(id, "plan-") || strings.ContainsAny(id, "/\\") || !validateName(id) {
		return plan, errors.New("invalid plan id")
	}
	if err := validatePlanDirectory(cfg, directory); err != nil {
		return plan, err
	}
	path := filepath.Join(directory, id+".json")
	if inside(cfg.Root, path) {
		if err := validateExistingPath(cfg.Root, path); err != nil {
			return plan, err
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return plan, err
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return plan, errors.New("plan is not a private regular file")
	}
	if err := readJSONFile(path, &plan); err != nil {
		return plan, err
	}
	if plan.Schema != planSchema || plan.ID != id || plan.Root != cfg.Root {
		return plan, errors.New("plan does not match configured root")
	}
	if err := validatePlan(plan); err != nil {
		return plan, err
	}
	return plan, nil
}

func applyGC(cfg config, id, planDirectory string, stdout, stderr io.Writer) int {
	plan, err := loadPlan(cfg, id, planDirectory)
	if err != nil || plan.Action != "" {
		fmt.Fprintf(stderr, "%s: cannot load a matching GC plan\n", prog)
		return exitBlocked
	}
	var items []planEntry
	for _, entry := range plan.Candidates {
		if entry.Class == "scratch" && entry.Action == "delete_scratch" {
			items = append(items, entry)
		}
	}
	if len(items) == 0 {
		fmt.Fprintln(stderr, "agent-state: plan has no scratch deletion candidates; evidence requires archive --apply --plan")
		return exitBlocked
	}
	return withIndexLock(cfg, func(idx *registryIndex) int {
		ledgerRefs, err := collectActiveLedgerRefs(cfg)
		if err != nil {
			fmt.Fprintln(stderr, "agent-state: ledger safety scan is unavailable")
			return exitBlocked
		}
		open, openErr := safetyOpenPaths()
		for _, entry := range items {
			if !inside(cfg.Root, entry.Path) {
				return exitBlocked
			}
			var leaseFound bool
			for _, lease := range idx.Leases {
				if lease.Path != entry.Path || lease.ReleasedAt != "" {
					continue
				}
				leaseFound = true
				expires, parseErr := time.Parse(time.RFC3339, lease.ExpiresAt)
				if parseErr != nil || time.Now().Before(expires) {
					return exitBlocked
				}
			}
			if !leaseFound {
				return exitBlocked
			}
			bytes, err := measureTreeWithin(entry.Path, cfg.Root)
			if err != nil || bytes != entry.Bytes {
				fmt.Fprintln(stderr, "agent-state: scratch changed since plan creation")
				return exitBlocked
			}
			if entry.TreeSHA256 == "" {
				return exitBlocked
			}
			fingerprint, hashErr := treeFingerprint(entry.Path)
			if hashErr != nil || fingerprint != entry.TreeSHA256 {
				fmt.Fprintln(stderr, "agent-state: scratch changed since plan creation")
				return exitBlocked
			}
			if reasons := safetyReasonsWithRefs(cfg, *idx, entry.Path, open, openErr, ledgerRefs); len(reasons) != 0 {
				fmt.Fprintf(stderr, "agent-state: plan is now blocked by %s\n", strings.Join(reasons, ","))
				return exitBlocked
			}
		}
		trash := rootPath(cfg, "_registry", "trash", id)
		type movedPath struct {
			source string
			target string
		}
		var moved []movedPath
		for _, entry := range items {
			rel, _ := filepath.Rel(cfg.Root, entry.Path)
			dst := filepath.Join(trash, rel)
			if err := validateExistingPath(cfg.Root, filepath.Dir(dst)); err != nil {
				return exitBlocked
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return exitEnv
			}
			if _, err := os.Lstat(dst); err == nil {
				return exitBlocked
			} else if !errors.Is(err, os.ErrNotExist) {
				return exitEnv
			}
			if err := os.Rename(entry.Path, dst); err != nil {
				for i := len(moved) - 1; i >= 0; i-- {
					_ = os.Rename(moved[i].target, moved[i].source)
				}
				fmt.Fprintln(stderr, "agent-state: move to trash failed")
				return exitEnv
			}
			moved = append(moved, movedPath{source: entry.Path, target: dst})
			for i := range idx.Leases {
				if idx.Leases[i].Path == entry.Path && idx.Leases[i].ReleasedAt == "" {
					idx.Leases[i].ReleasedAt = time.Now().UTC().Format(time.RFC3339)
				}
			}
		}
		if err := saveIndex(cfg, *idx); err != nil {
			for i := len(moved) - 1; i >= 0; i-- {
				_ = os.Rename(moved[i].target, moved[i].source)
			}
			return exitEnv
		}
		fmt.Fprintf(stdout, "staged %d scratch path(s) in registry trash; purge requires a separate plan\n", len(items))
		return exitOK
	})
}
