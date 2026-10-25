// Command agent-state manages bounded local execution state and retained evidence.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const prog = "agent-state"

const (
	exitOK       = 0
	exitNegative = 1
	exitUsage    = 2
	exitEnv      = 3
	exitBlocked  = 4
	gib          = int64(1024 * 1024 * 1024)
)

const usageText = `Usage: agent-state <command> [options]

Manage agent-execution ledgers, evidence, scratch leases, retention plans and restic archives.

Commands:
  report [--project P] [--brief] [--json]  sizes and retention budgets
  lint [--project P]                      inspect evidence for forbidden content
  gc [--dry-run] [--class CLASS]          write a plan; apply needs --apply --plan ID
     [--plan-dir PATH]                    store a reviewed plan outside the state root if needed
  pin add|rm|list                         protect paths from cleanup
  doctor                                  inspect local configuration and dependencies
  ledger path <project> <goal-id>         print a canonical ledger path
  scratch new|renew|release                manage expiring scratch leases
  evidence new|put|close                  manage evidence bundles
  archive <bundle> --apply --plan ID [--plan-dir PATH]  archive, verify, then replace with a stub
  archive --policy [--plan-dir PATH]      plan evidence snapshot retention
  verify <bundle>|--all                   verify restic archive data
  restore <bundle> [--to PATH]            restore verified evidence from restic

Defaults: scratch TTL 3 days, evidence grace 14 days, active ledger window 7 days,
project budget 2 GiB, total warning 8 GiB, total failure 15 GiB. Evidence archives
are retained for 12 months; ledgers are retained forever. Evidence archive and stub
operations always require an explicit reviewed plan.

Exit codes:
  0 success
  1 negative result
  2 usage error
  3 environment or setup failure
  4 blocked by a safety gate, nothing changed

Example:
  agent-state gc --dry-run --class scratch
`

type options struct {
	bools  map[string]bool
	values map[string]string
	pos    []string
}

func parseOptions(args, booleanNames, valueNames []string) (options, error) {
	out := options{bools: map[string]bool{}, values: map[string]string{}}
	bools, values := map[string]bool{}, map[string]bool{}
	for _, name := range booleanNames {
		bools[name] = true
	}
	for _, name := range valueNames {
		values[name] = true
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			out.pos = append(out.pos, a)
			continue
		}
		key, val, has := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		if bools[key] {
			if has && val != "true" && val != "false" {
				return out, fmt.Errorf("--%s expects no value", key)
			}
			out.bools[key] = !has || val == "true"
			continue
		}
		if !values[key] {
			return out, fmt.Errorf("unknown option %s", a)
		}
		if !has {
			i++
			if i >= len(args) || strings.HasPrefix(args[i], "--") {
				return out, fmt.Errorf("--%s needs a value", key)
			}
			val = args[i]
		}
		out.values[key] = val
	}
	return out, nil
}

func usageError(w io.Writer, msg string) int {
	fmt.Fprintf(w, "%s: %s\nRun '%s --help' for usage.\n", prog, msg, prog)
	return exitUsage
}

type config struct {
	Root             string `json:"root"`
	ConfiguredRoot   string `json:"-"`
	ActiveLedgerDays int    `json:"active_ledger_days"`
	ScratchTTL       string `json:"scratch_ttl"`
	EvidenceGrace    string `json:"evidence_grace"`
	ProjectBudget    int64  `json:"project_budget_bytes"`
	WarnTotal        int64  `json:"warn_total_bytes"`
	FailTotal        int64  `json:"fail_total_bytes"`
	Archive          struct {
		Repo            string `json:"repo"`
		PasswordRef     string `json:"password_ref"`
		RetentionMonths int    `json:"retention_months"`
	} `json:"archive"`
	PinnedPaths []string `json:"pinned_paths"`
}

type fileConfig struct {
	AgentState config `json:"agent_state"`
}

func loadConfig(getenv func(string) string) (config, error) {
	home := getenv("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	cfgPath := getenv("AGENT_DRY_CONFIG")
	if cfgPath == "" {
		cfgPath = filepath.Join(home, ".config", "agent-dry", "config.json")
	}
	c := config{Root: filepath.Join(home, ".local", "state", "agent-execution"), ActiveLedgerDays: 7,
		ScratchTTL: "72h", EvidenceGrace: "336h", ProjectBudget: 2 * gib, WarnTotal: 8 * gib, FailTotal: 15 * gib}
	c.Archive.RetentionMonths = 12
	if raw, err := os.ReadFile(cfgPath); err == nil {
		var parsed fileConfig
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return c, fmt.Errorf("read agent-dry config: invalid JSON")
		}
		mergeConfig(&c, parsed.AgentState)
	} else if !errors.Is(err, os.ErrNotExist) {
		return c, fmt.Errorf("read agent-dry config: %w", err)
	}
	if value := getenv("AGENT_STATE_ROOT"); value != "" {
		c.Root = value
	}
	if c.Root == "" {
		return c, errors.New("agent_state.root is empty")
	}
	root, err := filepath.Abs(c.Root)
	if err != nil {
		return c, err
	}
	c.ConfiguredRoot = filepath.Clean(root)
	c.Root, err = canonicalPath(c.ConfiguredRoot)
	if err != nil {
		return c, fmt.Errorf("resolve state root: %w", err)
	}
	if c.ActiveLedgerDays < 1 {
		return c, errors.New("active_ledger_days must be positive")
	}
	if c.ProjectBudget <= 0 || c.WarnTotal <= 0 || c.FailTotal <= c.WarnTotal {
		return c, errors.New("invalid agent_state budgets")
	}
	if c.Archive.RetentionMonths <= 0 {
		return c, errors.New("archive.retention_months must be positive")
	}
	if parseDuration(c.ScratchTTL) <= 0 {
		return c, errors.New("scratch_ttl must be a positive duration such as 72h or 3d")
	}
	if parseDuration(c.EvidenceGrace) <= 0 {
		return c, errors.New("evidence_grace must be a positive duration such as 336h or 14d")
	}
	return c, nil
}

func mergeConfig(dst *config, src config) {
	if src.Root != "" {
		dst.Root = src.Root
	}
	if src.ActiveLedgerDays != 0 {
		dst.ActiveLedgerDays = src.ActiveLedgerDays
	}
	if src.ScratchTTL != "" {
		dst.ScratchTTL = src.ScratchTTL
	}
	if src.EvidenceGrace != "" {
		dst.EvidenceGrace = src.EvidenceGrace
	}
	if src.ProjectBudget != 0 {
		dst.ProjectBudget = src.ProjectBudget
	}
	if src.WarnTotal != 0 {
		dst.WarnTotal = src.WarnTotal
	}
	if src.FailTotal != 0 {
		dst.FailTotal = src.FailTotal
	}
	if src.Archive.Repo != "" {
		dst.Archive.Repo = src.Archive.Repo
	}
	if src.Archive.PasswordRef != "" {
		dst.Archive.PasswordRef = src.Archive.PasswordRef
	}
	if src.Archive.RetentionMonths != 0 {
		dst.Archive.RetentionMonths = src.Archive.RetentionMonths
	}
	dst.PinnedPaths = append(dst.PinnedPaths, src.PinnedPaths...)
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usageError(stderr, "command required")
	}
	if args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(stdout, usageText)
		return exitOK
	}
	if args[0] == "help" || (len(args) > 1 && (args[1] == "--help" || args[1] == "-h")) {
		fmt.Fprint(stdout, usageText)
		return exitOK
	}
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", prog, err)
		return exitEnv
	}
	switch args[0] {
	case "report":
		return reportCommand(cfg, args[1:], stdout, stderr)
	case "lint":
		return lintCommand(cfg, args[1:], stdout, stderr)
	case "gc":
		return gcCommand(cfg, args[1:], stdout, stderr)
	case "pin":
		return pinCommand(cfg, args[1:], stdout, stderr)
	case "doctor":
		return doctorCommand(cfg, args[1:], stdout, stderr)
	case "ledger":
		return ledgerCommand(cfg, args[1:], stdout, stderr)
	case "scratch":
		return scratchCommand(cfg, args[1:], stdout, stderr)
	case "evidence":
		return evidenceCommand(cfg, args[1:], stdout, stderr)
	case "archive":
		return archiveCommand(cfg, args[1:], stdout, stderr)
	case "verify":
		return verifyCommand(cfg, args[1:], stdout, stderr)
	case "restore":
		return restoreCommand(cfg, args[1:], stdout, stderr)
	default:
		return usageError(stderr, "unknown command "+args[0])
	}
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func validateName(s string) bool { return namePattern.MatchString(s) && s != "." && s != ".." }
func emitJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}
func parseErr(stderr io.Writer, err error) int {
	if err != nil {
		return usageError(stderr, err.Error())
	}
	return exitOK
}

func rootPath(cfg config, parts ...string) string {
	all := append([]string{cfg.Root}, parts...)
	return filepath.Join(all...)
}

func inside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func cleanPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func requireRepo(cfg config, stderr io.Writer) int {
	if strings.TrimSpace(cfg.Archive.Repo) == "" {
		fmt.Fprintf(stderr, "%s: archive repository is not configured; set agent_state.archive.repo\n", prog)
		return exitEnv
	}
	return exitOK
}

func checkPlanFlags(o options, stderr io.Writer) int {
	if o.bools["dry-run"] && o.bools["apply"] {
		return usageError(stderr, "--dry-run and --apply cannot be combined")
	}
	if o.bools["apply"] && o.values["plan"] == "" {
		fmt.Fprintf(stderr, "%s: --apply requires --plan <id>\n", prog)
		return exitUsage
	}
	if !o.bools["apply"] && o.values["plan"] != "" {
		fmt.Fprintf(stderr, "%s: --plan requires --apply\n", prog)
		return exitUsage
	}
	return exitOK
}

func parseDuration(s string) time.Duration {
	if strings.HasSuffix(s, "d") {
		days, err := strconv.ParseInt(strings.TrimSuffix(s, "d"), 10, 64)
		maxDays := int64(^uint64(0)>>1) / int64(24*time.Hour)
		if err != nil || days <= 0 || days > maxDays {
			return 0
		}
		return time.Duration(days) * 24 * time.Hour
	}
	duration, err := time.ParseDuration(s)
	if err != nil || duration <= 0 {
		return 0
	}
	return duration
}
