package audit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/moabualruz/agent-dry/internal/manifest"
)

// Baselines from the agent-dry local evidence study (2026-09-15 to 2026-10-17), spec section 10.
const (
	baseLookup = "0"
	baseMerge  = "18.6"
	baseSSH    = "about 100"
	baseBao    = "39"
	tgtLookup  = "80 percent"
	tgtInline  = "inline under 10 percent"
	tgtMerge   = "6 or fewer"
	tgtSSH     = "20 or fewer"
	tgtBao     = "0"
	tgtCopies  = "vendored copies identical"
)

// inlineBaselines and copyBaselines are per tool; a tool without a measured baseline has none.
var (
	inlineBaselines = map[string]string{"check-no-dashes": "259 inline, 26 runs"}
	copyBaselines   = map[string]string{"check-no-dashes": "10 copies, 5 variants"}
)

// Text caps bound each metric section in long audit reports.
const (
	maxInlineRows = 8
	maxCopyRows   = 6
	maxHostRows   = 3
	maxNames      = 6
	maxMissText   = 3
	maxUsageRows  = 8
)

type report struct {
	Schema       string         `json:"schema"`
	Window       windowInfo     `json:"window"`
	Transcripts  transcriptInfo `json:"transcripts"`
	CommandUsage commandUsage   `json:"command_usage"`
	LookupFirst  lookupMetric   `json:"lookup_first"`
	Inline       []inlineMetric `json:"inline"`
	PR           prMetric       `json:"pr_lifecycle"`
	SSH          sshMetric      `json:"ssh"`
	OpenBao      baoMetric      `json:"openbao"`
	Copies       *copyCensus    `json:"copies,omitempty"`
	Hygiene      *hygiene       `json:"hygiene,omitempty"`
	Misses       missSummary    `json:"misses"`
	Tools        int            `json:"tools_loaded"`
	Warnings     int            `json:"warnings"`
}

type windowInfo struct {
	Since string `json:"since"`
	From  string `json:"from"`
	To    string `json:"to"`
}

type transcriptInfo struct {
	Files        int `json:"files"`
	Main         int `json:"main"`
	Subagent     int `json:"subagent"`
	NotRead      int `json:"not_read_over_max_files"`
	Unreadable   int `json:"unreadable"`
	Sessions     int `json:"sessions"`
	ToolUses     int `json:"tool_uses"`
	BashCommands int `json:"bash_commands"`
	DuplicateIDs int `json:"duplicate_tool_use_ids"`
}

type commandUsage struct {
	ByDay     []commandUsageDay     `json:"by_day"`
	ByHost    []commandUsageHost    `json:"by_host"`
	ByHarness []commandUsageHarness `json:"by_harness"`
}

type commandUsageDay struct {
	Day string `json:"day"`
	commandCounts
}

type commandUsageHost struct {
	Host string `json:"host"`
	commandCounts
}

type commandUsageHarness struct {
	Harness string `json:"harness"`
	commandCounts
}

type lookupMetric struct {
	MutatingSessions int      `json:"sessions_with_mutation"`
	LookupFirst      int      `json:"sessions_lookup_first"`
	Rate             *float64 `json:"rate"`
	FindCommands     int      `json:"find_commands"`
	Baseline         string   `json:"baseline"`
	Target           string   `json:"target"`
}

type inlineMetric struct {
	Tool             string   `json:"tool"`
	Inline           int      `json:"inline"`
	InlineSessions   int      `json:"inline_sessions"`
	Runs             int      `json:"runs"`
	RunSessions      int      `json:"run_sessions"`
	InlineShare      *float64 `json:"inline_share"`
	Baseline         string   `json:"baseline,omitempty"`
	Target           string   `json:"target"`
	ReplacesPatterns int      `json:"replaces_patterns"`
}

type prMetric struct {
	MergeSessions int      `json:"sessions_with_merge"`
	Lifecycle     int      `json:"lifecycle_in_merge_sessions"`
	Merges        int      `json:"merges"`
	PerMerge      *float64 `json:"per_merge"`
	LifecycleAll  int      `json:"lifecycle_all_sessions"`
	Baseline      string   `json:"baseline"`
	Target        string   `json:"target"`
}

type hostMetric struct {
	Host     string `json:"host"`
	Commands int    `json:"commands"`
	Sessions int    `json:"sessions"`
}

type sshMetric struct {
	HostsConfigured int          `json:"hosts_configured"`
	Commands        int          `json:"commands"`
	Sessions        int          `json:"sessions"`
	PerSession      *float64     `json:"per_session"`
	Hosts           []hostMetric `json:"hosts"`
	HostRun         int          `json:"host_run"`
	Baseline        string       `json:"baseline"`
	Target          string       `json:"target"`
}

type baoMetric struct {
	Logins   int    `json:"logins"`
	Sessions int    `json:"sessions"`
	BaoRun   int    `json:"bao_run"`
	Baseline string `json:"baseline"`
	Target   string `json:"target"`
}

type hygiene struct {
	TrialOlder30d       []string `json:"trial_older_than_30d"`
	Unused              []string `json:"unused_in_window"`
	Unused60d           []string `json:"unused_60d"`
	FailingTests        []string `json:"failing_tests"`
	PromotionCandidates []string `json:"promotion_candidates"`
	Promoted            []string `json:"promoted"`
}

type hygieneEvidence struct {
	usage               *collector
	failedTests         []string
	promotionCandidates []string
	promoted            []string
}

func ratio(n, d int) *float64 {
	if d == 0 {
		return nil
	}
	v := float64(n) / float64(d)
	return &v
}

// build turns the collector state into the report.
func build(c *collector, tools []*manifest.Tool, info transcriptInfo, now time.Time, evidence hygieneEvidence) report {
	r := report{Schema: "audit/v1", Tools: len(tools), Transcripts: info}
	r.Transcripts.Sessions = len(c.sessions)
	r.Transcripts.ToolUses = c.toolUses
	r.Transcripts.BashCommands = c.bashCommands
	r.Transcripts.DuplicateIDs = c.duplicates
	r.CommandUsage = commandUsage{
		ByDay:     commandUsageDays(c.commandsByDay),
		ByHost:    commandUsageHosts(c.commandsByHost),
		ByHarness: commandUsageHarnesses(c.commandsByHarness),
	}

	r.LookupFirst = lookupMetric{FindCommands: c.findCommands, Baseline: baseLookup, Target: tgtLookup}
	r.PR = prMetric{LifecycleAll: c.lifecycleTotal, Baseline: baseMerge, Target: tgtMerge}
	for _, st := range c.sessions {
		if st.mutated {
			r.LookupFirst.MutatingSessions++
			if st.lookupFirst {
				r.LookupFirst.LookupFirst++
			}
		}
		if st.merges > 0 {
			r.PR.MergeSessions++
			r.PR.Merges += st.merges
			r.PR.Lifecycle += st.lifecycle
		}
	}
	r.LookupFirst.Rate = ratio(r.LookupFirst.LookupFirst, r.LookupFirst.MutatingSessions)
	r.PR.PerMerge = ratio(r.PR.Lifecycle, r.PR.Merges)

	r.Inline = []inlineMetric{}
	for _, t := range c.tools {
		if len(t.replaces) == 0 {
			continue
		}
		r.Inline = append(r.Inline, inlineMetric{
			Tool: t.tool.Name, Inline: t.inline, InlineSessions: len(t.inlineSess),
			Runs: t.invocations, RunSessions: len(t.invokedSess),
			InlineShare: ratio(t.inline, t.inline+t.invocations),
			Baseline:    inlineBaselines[t.tool.Name], Target: tgtInline, ReplacesPatterns: len(t.replaces),
		})
	}

	r.SSH = sshMetric{HostsConfigured: len(c.hosts), Commands: c.sshCommands, Sessions: len(c.sshSess),
		PerSession: ratio(c.sshCommands, len(c.sshSess)), HostRun: c.hostRun, Baseline: baseSSH, Target: tgtSSH, Hosts: []hostMetric{}}
	for _, h := range c.hosts {
		r.SSH.Hosts = append(r.SSH.Hosts, hostMetric{Host: h.name, Commands: h.commands, Sessions: len(h.sessions)})
	}
	r.OpenBao = baoMetric{Logins: c.baoLogins, Sessions: len(c.baoSess), BaoRun: c.baoRun, Baseline: baseBao, Target: tgtBao}

	if len(tools) > 0 {
		h := &hygiene{
			TrialOlder30d: []string{}, Unused: []string{}, Unused60d: []string{},
			FailingTests: evidence.failedTests, PromotionCandidates: evidence.promotionCandidates,
			Promoted: evidence.promoted,
		}
		if h.FailingTests == nil {
			h.FailingTests = []string{}
		}
		if h.PromotionCandidates == nil {
			h.PromotionCandidates = []string{}
		}
		if h.Promoted == nil {
			h.Promoted = []string{}
		}
		old := now.AddDate(0, 0, -30)
		usageByName := map[string]*toolStat{}
		if evidence.usage != nil {
			for _, stat := range evidence.usage.tools {
				usageByName[stat.tool.Name] = stat
			}
		}
		for _, t := range c.tools {
			if !t.tool.Listed() {
				continue
			}
			if t.tool.Status == "trial" {
				if d, err := time.Parse("2006-01-02", t.tool.Origin.Date); err == nil && d.Before(old) {
					h.TrialOlder30d = append(h.TrialOlder30d, t.tool.Name)
				}
			}
			if t.invocations == 0 {
				h.Unused = append(h.Unused, t.tool.Name)
			}
			if usageByName[t.tool.Name] == nil || usageByName[t.tool.Name].invocations == 0 {
				h.Unused60d = append(h.Unused60d, t.tool.Name)
			}
		}
		r.Hygiene = h
	}
	return r
}

func commandUsageDays(counts map[string]commandCounts) []commandUsageDay {
	keys := sortedCountKeys(counts)
	rows := make([]commandUsageDay, 0, len(keys))
	for _, key := range keys {
		rows = append(rows, commandUsageDay{Day: key, commandCounts: counts[key]})
	}
	return rows
}

func commandUsageHosts(counts map[string]commandCounts) []commandUsageHost {
	keys := sortedCountKeys(counts)
	rows := make([]commandUsageHost, 0, len(keys))
	for _, key := range keys {
		rows = append(rows, commandUsageHost{Host: key, commandCounts: counts[key]})
	}
	return rows
}

func commandUsageHarnesses(counts map[string]commandCounts) []commandUsageHarness {
	keys := sortedCountKeys(counts)
	rows := make([]commandUsageHarness, 0, len(keys))
	for _, key := range keys {
		rows = append(rows, commandUsageHarness{Harness: key, commandCounts: counts[key]})
	}
	return rows
}

func sortedCountKeys(counts map[string]commandCounts) []string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func writeJSON(w io.Writer, r report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func pct(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", *v*100)
}

func num(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.1f", *v)
}

func names(list []string) string {
	if len(list) == 0 {
		return "none"
	}
	if len(list) > maxNames {
		return strings.Join(list[:maxNames], ", ") + fmt.Sprintf(", +%d more", len(list)-maxNames)
	}
	return strings.Join(list, ", ")
}

// writeText prints the compact table: counts, tool names, host names and miss tokens only.
func writeText(w io.Writer, r report) error {
	t := r.Transcripts
	fmt.Fprintf(w, "agent-dry audit: window %s (%s to %s)\n", r.Window.Since, r.Window.From, r.Window.To)
	more := ""
	if t.NotRead > 0 {
		more = fmt.Sprintf("; %d more not read (--max-files)", t.NotRead)
	}
	fmt.Fprintf(w, "read %d transcripts (%d main, %d subagent%s), %d sessions, %d Bash commands, %d tools\n",
		t.Files, t.Main, t.Subagent, more, t.Sessions, t.BashCommands, r.Tools)

	var table bytes.Buffer
	tw := tabwriter.NewWriter(&table, 0, 0, 2, ' ', 0)
	row := func(cols ...string) { fmt.Fprintln(tw, strings.Join(cols, "\t")) }
	row("METRIC", "VALUE", "BASELINE", "TARGET")

	lf := r.LookupFirst
	row("1 lookup before first mutation", fmt.Sprintf("%d of %d sessions (%s)", lf.LookupFirst, lf.MutatingSessions, pct(lf.Rate)), lf.Baseline, lf.Target)

	switch {
	case r.Tools == 0:
		row("2 inline vs tool runs", "no tools registered", "", "")
	case len(r.Inline) == 0:
		row("2 inline vs tool runs", "no tool declares replaces", "", "")
	default:
		for i, m := range r.Inline {
			if i == maxInlineRows {
				row("  ...", fmt.Sprintf("+%d tools (see --json)", len(r.Inline)-maxInlineRows), "", "")
				break
			}
			row("2 inline vs runs: "+m.Tool, fmt.Sprintf("%d inline, %d runs (%s inline)", m.Inline, m.Runs, pct(m.InlineShare)), dash(m.Baseline), m.Target)
		}
	}

	p := r.PR
	row("3 PR lifecycle per merge", fmt.Sprintf("%s (%d commands, %d merges, %d sessions)", num(p.PerMerge), p.Lifecycle, p.Merges, p.MergeSessions), p.Baseline, p.Target)

	s := r.SSH
	if s.HostsConfigured == 0 {
		row("4 raw ssh per touching session", "no hosts in config", s.Baseline, s.Target)
	} else {
		row("4 raw ssh per touching session", fmt.Sprintf("%s (%d commands, %d sessions)", num(s.PerSession), s.Commands, s.Sessions), s.Baseline, s.Target)
		for i, h := range s.Hosts {
			if i == maxHostRows {
				row("    ...", fmt.Sprintf("+%d hosts (see --json)", len(s.Hosts)-maxHostRows), "", "")
				break
			}
			row("    "+h.Host, fmt.Sprintf("%d commands, %d sessions", h.Commands, h.Sessions), "", "")
		}
	}
	row("  host-run invocations", fmt.Sprintf("%d", s.HostRun), "", "")

	b := r.OpenBao
	row("5 hand-built OpenBao logins", fmt.Sprintf("%d (%d sessions)", b.Logins, b.Sessions), b.Baseline, b.Target)
	row("  bao-run invocations", fmt.Sprintf("%d", b.BaoRun), "", "")

	if r.Copies != nil {
		shown := 0
		var hidden int
		for _, c := range r.Copies.Tools {
			if c.Copies == 0 {
				continue
			}
			if shown == maxCopyRows {
				hidden++
				continue
			}
			shown++
			row("6 copies: "+c.Tool, fmt.Sprintf("%d copies, %d variants, %d identical", c.Copies, c.Variants, c.Canonical), dash(c.Baseline), tgtCopies)
		}
		if hidden > 0 {
			row("  ...", fmt.Sprintf("+%d tools (see --json)", hidden), "", "")
		}
		if shown == 0 {
			row("6 copies", fmt.Sprintf("none in %d roots (%s)", r.Copies.Roots, r.Copies.Source), "", tgtCopies)
		}
	}

	if r.Hygiene != nil {
		row("7 trial older than 30 days", names(r.Hygiene.TrialOlder30d), "", "")
		row("  unused in window", names(r.Hygiene.Unused), "", "")
		row("  unused in 60 days", names(r.Hygiene.Unused60d), "", "")
		row("  failing tool tests", names(r.Hygiene.FailingTests), "", "")
		row("  promotion candidates", names(r.Hygiene.PromotionCandidates), "", "")
		row("  promoted", names(r.Hygiene.Promoted), "", "")
	}

	m := r.Misses
	if !m.Present {
		row("8 find misses", "no miss log", "", "")
	} else {
		var tops []string
		for i, ts := range m.Top {
			if i == maxMissText {
				break
			}
			tops = append(tops, fmt.Sprintf("%s (%d)", strings.Join(ts.Tokens, " "), ts.Count))
		}
		v := fmt.Sprintf("%d in window of %d", m.InWindow, m.Total)
		if len(tops) > 0 {
			v += "; top: " + strings.Join(tops, "; ")
		}
		row("8 find misses", v, "", "")
	}
	row("9 agent-dry commands", "find/new/check/guide", "", "")
	for i, usage := range r.CommandUsage.ByDay {
		if i == maxUsageRows {
			row("  ...", fmt.Sprintf("+%d days (see --json)", len(r.CommandUsage.ByDay)-maxUsageRows), "", "")
			break
		}
		row("  day "+usage.Day, commandCountsText(usage.commandCounts), "", "")
	}
	for i, usage := range r.CommandUsage.ByHost {
		if i == maxUsageRows {
			row("  ...", fmt.Sprintf("+%d hosts (see --json)", len(r.CommandUsage.ByHost)-maxUsageRows), "", "")
			break
		}
		row("  host "+usage.Host, commandCountsText(usage.commandCounts), "", "")
	}
	for i, usage := range r.CommandUsage.ByHarness {
		if i == maxUsageRows {
			row("  ...", fmt.Sprintf("+%d harnesses (see --json)", len(r.CommandUsage.ByHarness)-maxUsageRows), "", "")
			break
		}
		row("  harness "+usage.Harness, commandCountsText(usage.commandCounts), "", "")
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimRight(table.String(), "\n"), "\n") {
		if _, err := fmt.Fprintln(w, strings.TrimRight(line, " ")); err != nil {
			return err
		}
	}
	if r.Warnings > 0 {
		fmt.Fprintf(w, "%d warnings on stderr\n", r.Warnings)
	}
	return nil
}

func commandCountsText(c commandCounts) string {
	return fmt.Sprintf("find=%d new=%d check=%d guide=%d", c.Find, c.New, c.Check, c.Guide)
}

func dash(s string) string {
	if s == "" {
		return "none recorded"
	}
	return s
}
