// Package audit measures whether agents look up, reuse and stop re-typing registered work.
// It registers the audit command.
package audit

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/moabualruz/agent-dry/internal/cli"
	"github.com/moabualruz/agent-dry/internal/manifest"
	"github.com/moabualruz/agent-dry/internal/paths"
)

// defaultMaxFiles bounds a run; main sessions are read before subagent transcripts.
const defaultMaxFiles = 1000

// now is replaced by tests.
var now = time.Now

const usageText = `Usage: agent-dry audit [--since 14d] [--transcripts <dir>] [--max-files N] [--json]

Measure lookup, reuse and re-typing (spec section 10) from Claude Code transcripts, Codex sessions,
the find miss log and the tool manifests, against the recorded baselines. Reports agent-dry
find/new/check/guide counts by day, host and harness. Prints counts, tool names, configured host
names and miss tokens only; never command text.

Options:
  --since D          window: Nd or Nw (from local midnight), Nh or a Go duration (default 14d);
                     files modified and tool calls made before it are skipped
  --transcripts DIR  Claude transcript root (default ~/.claude/projects); also reads
                     ~/.codex/sessions/YYYY/MM/DD/*.jsonl; session id is the file stem
  --max-files N      read at most N files, main sessions first, newest first (default 1000; 0 = no bound)
  --json             print the report as JSON (schema audit/v1)

Exit codes: 0 report printed, 1 no transcript in the window, 2 usage error,
3 unreadable config, transcript root or miss log.

Example: agent-dry audit --since 32d --max-files 400
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
		return cli.Usagef(env, "audit: %v; run agent-dry audit --help", err)
	}
	if fs.NArg() > 0 {
		return cli.Usagef(env, "audit: unexpected argument %q; run agent-dry audit --help", fs.Arg(0))
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
		fmt.Fprintf(env.Stderr, "agent-dry audit: warning: "+format+"\n", a...)
	}

	cfg, err := paths.LoadConfig(env.Getenv)
	if err != nil {
		return cli.Envf(env, "audit: %v", err)
	}
	home := paths.Home(env.Getenv)

	var tools []*manifest.Tool
	if root, err := paths.Root(env.Getenv); err != nil {
		warn("no agent-dry root (%v); reporting transcript metrics only", err)
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

	c := newCollector(tools, cfg.Hosts, warn)
	info := transcriptInfo{NotRead: notRead}
	localHost := env.Getenv("HOSTNAME")
	if localHost == "" {
		localHost, _ = os.Hostname()
	}
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

	r := build(c, tools, info, end)
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
