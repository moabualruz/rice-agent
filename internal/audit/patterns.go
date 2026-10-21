package audit

import (
	"regexp"
	"strings"
)

// Command boundaries: a pattern below only matches where a word starts a shell command or an
// argument, so "perform" never counts as rm and "--rm" never counts either.
const (
	cmdStart  = "(?:^|[\\s;&|(`'\"])"
	pathStart = "(?:^|[\\s;&|(`'\"/])"
	argEnd    = "(?:$|[\\s;&|)`'\"])"
)

var (
	// findRe is a registry lookup.
	findRe = regexp.MustCompile(pathStart + `agent-dry\s+find` + argEnd)

	// mutateBashRe is the conservative set of mutating shell commands (metric 1). A redirect into a
	// tracked path is not detectable from command text, so it is not counted.
	mutateBashRe = regexp.MustCompile(cmdStart + `(?:` +
		`git(?:\s+-C\s+\S+)?\s+(?:commit|push)` + argEnd +
		`|gh\s+pr\s+(?:create|merge)` + argEnd +
		`|rm\s|mv\s` +
		`|sed\s+(?:-[A-Za-z]+\s+)*-i` +
		`|perl\s+(?:-0\d*\s+)?-0?\d*p?i` +
		`)`)

	// prStatusRe is a PR or CI status command (metric 3).
	prStatusRe = regexp.MustCompile(cmdStart + `gh\s+(?:pr\s+(?:view|checks|status|list)|run\s+(?:list|view|watch))` + argEnd)
	// ghMergeRe is a gh merge.
	ghMergeRe = regexp.MustCompile(cmdStart + `gh\s+pr\s+merge` + argEnd)
	// prFlowRe and prFlowMergeRe are pr-flow invocations.
	prFlowRe      = invokeRe("pr-flow")
	prFlowMergeRe = regexp.MustCompile(pathStart + `pr-flow(?:\.sh)?\s+merge` + argEnd)

	// sshRe is a raw ssh invocation (metric 4).
	sshRe     = regexp.MustCompile(cmdStart + `ssh` + `(?:$|\s)`)
	hostRunRe = invokeRe("host-run")

	// baoLogin is the hand-built OpenBao userpass login path (metric 5).
	baoLogin = "/v1/auth/userpass/login"
	baoRunRe = invokeRe("bao-run")
)

// invokeRe matches a command that runs name, directly, by path or as name.sh.
func invokeRe(name string) *regexp.Regexp {
	return regexp.MustCompile(pathStart + regexp.QuoteMeta(name) + `(?:\.(?:sh|bash|ts|mjs|js))?` + argEnd)
}

// hostRe matches a configured host's address or short name as a whole word.
func hostRe(name, addr string) *regexp.Regexp {
	var alts []string
	if addr != "" {
		alts = append(alts, regexp.QuoteMeta(addr))
	}
	if name != "" && name != addr {
		alts = append(alts, regexp.QuoteMeta(name))
	}
	return regexp.MustCompile(`(?:^|[^A-Za-z0-9._-])(?:` + strings.Join(alts, "|") + `)(?:$|[^A-Za-z0-9._-])`)
}

// mutatingTool reports whether a tool_use name mutates files.
func mutatingTool(name string) bool {
	switch name {
	case "Edit", "Write", "NotebookEdit", "MultiEdit":
		return true
	}
	return false
}

// redactToken hides miss-log tokens that look like credentials; only tokens are ever printed.
var secretTokenRe = regexp.MustCompile(`^(?:hvs\.|hvb\.|gh[pousr]_|github_pat_|sk-|xox[abp]-)|[A-Za-z0-9_.+/=-]{33,}`)

func redactToken(t string) string {
	if secretTokenRe.MatchString(t) {
		return "<redacted>"
	}
	if len(t) > 32 {
		return t[:32]
	}
	return t
}

// commandName is one CLI subcommand included in the usage report.
type commandName string

const (
	commandFind  commandName = "find"
	commandNew   commandName = "new"
	commandCheck commandName = "check"
	commandGuide commandName = "guide"
)

var commandUsageRe = map[commandName]*regexp.Regexp{
	commandFind:  findRe,
	commandNew:   agentDryCommandRe(commandNew),
	commandCheck: agentDryCommandRe(commandCheck),
	commandGuide: agentDryCommandRe(commandGuide),
}

func agentDryCommandRe(name commandName) *regexp.Regexp {
	return regexp.MustCompile(pathStart + `agent-dry\s+` + regexp.QuoteMeta(string(name)) + argEnd)
}
