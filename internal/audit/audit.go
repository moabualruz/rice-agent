// Package audit measures whether agents look up, reuse and stop re-typing registered work.
// It registers the audit command.
package audit

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/moabualruz/rice-agent/internal/cli"
	"github.com/moabualruz/rice-agent/internal/gates"
	"github.com/moabualruz/rice-agent/internal/manifest"
	"github.com/moabualruz/rice-agent/internal/paths"
)

// defaultMaxFiles bounds a run; main sessions are read before subagent transcripts.
const defaultMaxFiles = 1000

// now and hermesLookPath are replaced by tests.
var now = time.Now
var hermesLookPath = exec.LookPath

const usageText = `Usage: rice-agent audit [--since 14d] [--transcripts <dir>] [--max-files N] [--json]

Measure lookup, reuse and re-typing (spec section 10) from Claude Code, Codex and Hermes sessions,
the find miss log and the tool manifests, against the recorded baselines. Reports Rice Agent
find/new/check/guide counts by day, host and harness, failing tool tests and 60-day inactivity. A
trial tool with successful use in two distinct sessions and a passing test is promoted to active.
Prints counts, tool names, configured host names and miss tokens only; never command text.

Options:
  --since D          report window: Nd or Nw (from local midnight), Nh or a Go duration (default 14d);
                     60-day inactivity and promotion usage are measured independently
  --transcripts DIR  Claude transcript root (default ~/.claude/projects); also reads
                     ~/.codex/sessions/YYYY/MM/DD/*.jsonl and Hermes JSONL from its export CLI
  --max-files N      read at most N files, main sessions first, newest first (default 1000; 0 = no bound)
  --json             print the report as JSON (schema audit/v1)

Exit codes: 0 report printed, 1 no transcript in the window, 2 usage error,
3 unreadable config, transcript root or miss log.

Example: rice-agent audit --since 32d --max-files 400
`

func init() {
	cli.Register(cli.Command{Name: "audit", Summary: "effectiveness report from transcripts, misses and manifests", Run: run})
}

var sinceRe = regexp.MustCompile(`^([0-9]+)([dwh])$`)

// parseSince accepts 14d, 2w, 36h or any Go duration.
func parseSince(s string) (time.Duration, error) {
	if m := sinceRe.FindStringSubmatch(s); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, err
		}
		unit := map[string]time.Duration{"d": 24 * time.Hour, "w": 7 * 24 * time.Hour, "h": time.Hour}[m[2]]
		if n <= 0 {
			return 0, fmt.Errorf("--since must be positive")
		}
		return time.Duration(n) * unit, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("--since %q: want Nd, Nw, Nh or a Go duration", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("--since must be positive")
	}
	return d, nil
}

func run(env *cli.Env) int {
	for _, a := range env.Args {
		if a == "--help" || a == "-h" || a == "-help" {
			fmt.Fprint(env.Stdout, usageText)
			return cli.ExitOK
		}
	}
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	since := fs.String("since", "14d", "")
	dir := fs.String("transcripts", "", "")
	maxFiles := fs.Int("max-files", defaultMaxFiles, "")
	asJSON := fs.Bool("json", false, "")
	if err := fs.Parse(env.Args); err != nil {
		return cli.Usagef(env, "audit: %v; run rice-agent audit --help", err)
	}
	if fs.NArg() > 0 {
		return cli.Usagef(env, "audit: unexpected argument %q; run rice-agent audit --help", fs.Arg(0))
	}
	window, err := parseSince(*since)
	if err != nil {
		return cli.Usagef(env, "audit: %v", err)
	}
	if *maxFiles < 0 {
		return cli.Usagef(env, "audit: --max-files must be 0 or more")
	}

	warnings := 0
	warn := func(format string, a ...any) {
		warnings++
		fmt.Fprintf(env.Stderr, "rice-agent audit: warning: "+format+"\n", a...)
	}

	cfg, err := paths.LoadConfig(env.Getenv)
	if err != nil {
		return cli.Envf(env, "audit: %v", err)
	}
	home := paths.Home(env.Getenv)

	var tools []*manifest.Tool
	if root, err := paths.Root(env.Getenv); err != nil {
		warn("no rice-agent root (%v); reporting transcript metrics only", err)
	} else {
		loaded, err := manifest.LoadAll(filepath.Join(root, "tools"))
		if err != nil {
			warn("some manifests did not load: %v", err)
		}
		tools = loaded
	}

	end := now()
	cutoff := end.Add(-window)
	if m := sinceRe.FindStringSubmatch(*since); m != nil && m[2] != "h" {
		// Day windows start at local midnight so the printed dates are the whole window.
		y, mo, d := cutoff.Date()
		cutoff = time.Date(y, mo, d, 0, 0, 0, 0, cutoff.Location())
	}
	tdir := *dir
	if tdir == "" {
		tdir = filepath.Join(home, ".claude", "projects")
	}
	files, _, err := discover(tdir, cutoff, 0)
	if err != nil {
		return cli.Envf(env, "audit: transcript root: %v", err)
	}
	codexFiles, err := discoverCodex(filepath.Join(home, ".codex", "sessions"), cutoff)
	if err != nil {
		return cli.Envf(env, "audit: Codex sessions: %v", err)
	}
	files = append(files, codexFiles...)
	files, notRead := limitTranscripts(files, *maxFiles)

	localHost := env.Getenv("HOSTNAME")
	if localHost == "" {
		localHost, _ = os.Hostname()
	}
	c, info := collectTranscripts(files, cutoff, tools, cfg.Hosts, localHost, warn)
	info.NotRead = notRead
	hermesInfo, hermesNotRead := collectHermes(c, cutoff, remainingFiles(*maxFiles, len(files)), warn)
	addTranscriptInfo(&info, hermesInfo)
	info.NotRead += hermesNotRead

	usageCutoff := end.Add(-60 * 24 * time.Hour)
	usageFiles, _, err := discover(tdir, usageCutoff, 0)
	if err != nil {
		return cli.Envf(env, "audit: transcript root for 60-day usage: %v", err)
	}
	usageCodexFiles, err := discoverCodex(filepath.Join(home, ".codex", "sessions"), usageCutoff)
	if err != nil {
		return cli.Envf(env, "audit: Codex sessions for 60-day usage: %v", err)
	}
	usageFiles = append(usageFiles, usageCodexFiles...)
	usageFiles, usageNotRead := limitTranscripts(usageFiles, *maxFiles)
	if usageNotRead > 0 {
		warn("60-day usage incomplete: %d transcripts not read (--max-files)", usageNotRead)
	}
	usage, _ := collectTranscripts(usageFiles, usageCutoff, tools, cfg.Hosts, localHost, warn)
	_, hermesUsageNotRead := collectHermes(usage, usageCutoff, remainingFiles(*maxFiles, len(usageFiles)), warn)
	if hermesUsageNotRead > 0 {
		warn("60-day usage incomplete: %d Hermes sessions not read (--max-files)", hermesUsageNotRead)
	}

	failedTests := make([]string, 0)
	testPassed := make(map[string]bool, len(tools))
	for _, tool := range tools {
		if !tool.Listed() {
			continue
		}
		passed := gates.ToolTestPasses(tool)
		testPassed[tool.Name] = passed
		if !passed {
			failedTests = append(failedTests, tool.Name)
		}
	}

	promotionCandidates := make([]string, 0)
	promoted := make([]string, 0)
	for _, stat := range usage.tools {
		if stat.tool.Status != "trial" || len(stat.okSess) < 2 || !testPassed[stat.tool.Name] {
			continue
		}
		promotionCandidates = append(promotionCandidates, stat.tool.Name)
		changed, err := manifest.PromoteIfTrial(stat.tool)
		if err != nil {
			warn("tool %s promotion failed: %v", stat.tool.Name, err)
			continue
		}
		if changed {
			promoted = append(promoted, stat.tool.Name)
		}
	}
	evidence := hygieneEvidence{
		usage: usage, failedTests: failedTests,
		promotionCandidates: promotionCandidates, promoted: promoted,
	}
	r := build(c, tools, info, end, evidence)
	r.Window = windowInfo{Since: *since, From: cutoff.Format("2006-01-02"), To: end.Format("2006-01-02")}

	misses, err := summarizeMisses(filepath.Join(paths.StateDir(env.Getenv), "misses.jsonl"), cutoff)
	if err != nil {
		return cli.Envf(env, "audit: miss log: %v", err)
	}
	r.Misses = misses

	if len(tools) > 0 {
		roots, source := copyRoots(cfg.Repos, home)
		census := censusCopies(tools, roots, source)
		if census.Truncated {
			warn("copy search stopped after %d entries", maxWalkEntries)
		}
		r.Copies = &census
	}
	r.Warnings = warnings

	if *asJSON {
		err = writeJSON(env.Stdout, r)
	} else {
		err = writeText(env.Stdout, r)
	}
	if err != nil {
		return cli.Envf(env, "audit: write report: %v", err)
	}
	if info.Files == 0 {
		return cli.ExitNegative
	}
	return cli.ExitOK
}

func remainingFiles(maxFiles, used int) int {
	if maxFiles <= 0 {
		return 0
	}
	if used >= maxFiles {
		return -1
	}
	return maxFiles - used
}

func addTranscriptInfo(dst *transcriptInfo, src transcriptInfo) {
	dst.Files += src.Files
	dst.Main += src.Main
	dst.Subagent += src.Subagent
	dst.Unreadable += src.Unreadable
	dst.Sessions += src.Sessions
	dst.ToolUses += src.ToolUses
	dst.BashCommands += src.BashCommands
	dst.DuplicateIDs += src.DuplicateIDs
}

func collectHermes(c *collector, cutoff time.Time, maxFiles int, warn func(string, ...any)) (transcriptInfo, int) {
	var info transcriptInfo
	hermes, err := hermesLookPath("hermes")
	if err != nil {
		return info, 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, hermes, "sessions", "export", "--format", "jsonl", "--newer-than", cutoff.UTC().Format(time.RFC3339), "-")
	cmd.Stderr = io.Discard
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		warn("Hermes export could not start")
		return info, 0
	}
	if err := cmd.Start(); err != nil {
		warn("Hermes export could not start")
		return info, 0
	}
	var notRead int
	err = eachLine(stdout, func(line []byte) {
		if len(bytes.TrimSpace(line)) == 0 {
			return
		}
		if maxFiles < 0 || (maxFiles > 0 && info.Files >= maxFiles) {
			notRead++
			return
		}
		session, events, results, ok := scanHermesLine(line, cutoff)
		if !ok {
			info.Unreadable++
			return
		}
		for _, ev := range events {
			ev.harness = "hermes"
			c.add("hermes:"+session, ev)
		}
		for _, r := range results {
			c.result(r)
		}
		c.endFile()
		info.Files++
		info.Main++
	})
	waitErr := cmd.Wait()
	if err != nil || waitErr != nil {
		warn("Hermes export failed or returned unreadable data")
	}
	return info, notRead
}

func collectTranscripts(files []transcript, cutoff time.Time, tools []*manifest.Tool, hosts map[string]paths.Host, localHost string, warn func(string, ...any)) (*collector, transcriptInfo) {
	c := newCollector(tools, hosts, warn)
	info := transcriptInfo{}
	for _, f := range files {
		session := f.harness + ":" + f.session
		add := func(ev event) {
			ev.host = c.commandHost(ev.cmd, localHost)
			c.add(session, ev)
		}
		var scanErr error
		if f.harness == "codex" {
			scanErr = scanCodexFile(f.path, cutoff, add)
		} else {
			scanErr = scanFile(f.path, cutoff, add, c.wantResult, c.result)
		}
		c.endFile()
		if scanErr != nil {
			info.Unreadable++
			warn("transcript %s: %v", filepath.Base(f.path), scanErr)
			continue
		}
		info.Files++
		if f.subagent {
			info.Subagent++
		} else {
			info.Main++
		}
	}
	return c, info
}
