package audit

import (
	"bytes"
	"regexp"
	"sort"
	"strings"

	"github.com/moabualruz/agent-dry/internal/manifest"
	"github.com/moabualruz/agent-dry/internal/paths"
)

// toolStat counts one registered tool's inline reimplementations and invocations.
type toolStat struct {
	tool        *manifest.Tool
	replaces    []*regexp.Regexp
	invoke      *regexp.Regexp
	inline      int
	inlineSess  map[string]bool
	invocations int
	invokedSess map[string]bool
	okSess      map[string]bool // sessions with an invocation whose result was not an error
}

// hostStat counts raw ssh commands that name one configured host.
type hostStat struct {
	name     string
	re       *regexp.Regexp
	commands int
	sessions map[string]bool
}

type sessionState struct {
	mutated     bool
	lookupFirst bool
	lifecycle   int
	merges      int
}

type pendingCall struct {
	session string
	tools   []int
}

// collector folds tool_use events into the metric counters. It keeps no command text.
type collector struct {
	tools    []*toolStat
	hosts    []*hostStat
	seen     map[string]bool
	sessions map[string]*sessionState
	pending  map[string]pendingCall

	toolUses, bashCommands, duplicates int
	findCommands                       int
	lifecycleTotal                     int
	sshCommands                        int
	sshSess                            map[string]bool
	hostRun                            int
	baoLogins                          int
	baoSess                            map[string]bool
	baoRun                             int
	commandsByDay                      map[string]commandCounts
	commandsByHost                     map[string]commandCounts
	commandsByHarness                  map[string]commandCounts
}

func newCollector(tools []*manifest.Tool, hosts map[string]paths.Host, warn func(string, ...any)) *collector {
	c := &collector{
		seen:              map[string]bool{},
		sessions:          map[string]*sessionState{},
		pending:           map[string]pendingCall{},
		sshSess:           map[string]bool{},
		baoSess:           map[string]bool{},
		commandsByDay:     map[string]commandCounts{},
		commandsByHost:    map[string]commandCounts{},
		commandsByHarness: map[string]commandCounts{},
	}
	for _, t := range tools {
		ts := &toolStat{tool: t, invoke: invokeRe(t.Name), inlineSess: map[string]bool{}, invokedSess: map[string]bool{}, okSess: map[string]bool{}}
		for _, p := range t.Replaces {
			re, err := regexp.Compile(p)
			if err != nil {
				warn("tool %s: replaces pattern %d does not compile: %v", t.Name, len(ts.replaces)+1, err)
				continue
			}
			ts.replaces = append(ts.replaces, re)
		}
		c.tools = append(c.tools, ts)
	}
	names := make([]string, 0, len(hosts))
	for n := range hosts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		addr := hosts[n].SSH
		if i := strings.LastIndex(addr, "@"); i >= 0 {
			addr = addr[i+1:]
		}
		if n == "" && addr == "" {
			continue
		}
		c.hosts = append(c.hosts, &hostStat{name: n, re: hostRe(n, addr), sessions: map[string]bool{}})
	}
	return c
}

func (c *collector) state(session string) *sessionState {
	st := c.sessions[session]
	if st == nil {
		st = &sessionState{}
		c.sessions[session] = st
	}
	return st
}

// add folds one tool_use. Repeated ids (forked workers inherit their parent's history, and
// streaming can repeat a line) count once, for the first file that holds them.
func (c *collector) add(session string, ev event) {
	if ev.id != "" {
		if c.seen[ev.id] {
			c.duplicates++
			return
		}
		c.seen[ev.id] = true
	}
	c.toolUses++
	st := c.state(session)
	if mutatingTool(ev.tool) {
		st.mutated = true
		return
	}
	if ev.tool != "Bash" || ev.cmd == "" {
		return
	}
	c.bashCommands++
	cmd := ev.cmd
	c.countCommandUsage(ev, cmd)

	if findRe.MatchString(cmd) {
		c.findCommands++
		if !st.mutated {
			st.lookupFirst = true
		}
	}
	if mutateBashRe.MatchString(cmd) {
		st.mutated = true
	}

	flowMerge := prFlowMergeRe.MatchString(cmd)
	if ghMergeRe.MatchString(cmd) || flowMerge {
		st.merges++
	}
	if prStatusRe.MatchString(cmd) || (!flowMerge && prFlowRe.MatchString(cmd)) {
		st.lifecycle++
		c.lifecycleTotal++
	}

	hostRun := hostRunRe.MatchString(cmd)
	if hostRun {
		c.hostRun++
	}
	if !hostRun && len(c.hosts) > 0 && sshRe.MatchString(cmd) {
		touched := false
		for _, h := range c.hosts {
			if h.re.MatchString(cmd) {
				h.commands++
				h.sessions[session] = true
				touched = true
			}
		}
		if touched {
			c.sshCommands++
			c.sshSess[session] = true
		}
	}

	if strings.Contains(cmd, baoLogin) {
		c.baoLogins++
		c.baoSess[session] = true
	}
	if baoRunRe.MatchString(cmd) {
		c.baoRun++
	}

	var called []int
	for i, t := range c.tools {
		if t.invoke.MatchString(cmd) {
			t.invocations++
			t.invokedSess[session] = true
			called = append(called, i)
			continue
		}
		for _, re := range t.replaces {
			if re.MatchString(cmd) {
				t.inline++
				t.inlineSess[session] = true
				break
			}
		}
	}
	if len(called) > 0 && ev.id != "" {
		c.pending[ev.id] = pendingCall{session: session, tools: called}
	}
}

// wantResult reports whether a user line may hold the result of a pending tool invocation.
func (c *collector) wantResult(line []byte) bool {
	for id := range c.pending {
		if bytes.Contains(line, []byte(id)) {
			return true
		}
	}
	return false
}

func (c *collector) result(r result) {
	p, ok := c.pending[r.id]
	if !ok {
		return
	}
	delete(c.pending, r.id)
	if r.isError {
		return
	}
	for _, i := range p.tools {
		c.tools[i].okSess[p.session] = true
	}
}

// endFile drops invocations whose result never appeared; they count as invoked, not as successful.
func (c *collector) endFile() {
	clear(c.pending)
}

type commandCounts struct {
	Find  int `json:"find"`
	New   int `json:"new"`
	Check int `json:"check"`
	Guide int `json:"guide"`
}

func (c *collector) countCommandUsage(ev event, cmd string) {
	day := "unknown"
	if !ev.at.IsZero() {
		day = ev.at.Format("2006-01-02")
	}
	host := ev.host
	if host == "" {
		host = "unknown"
	}
	harness := ev.harness
	if harness == "" {
		harness = "unknown"
	}
	for name, re := range commandUsageRe {
		count := len(re.FindAllStringIndex(cmd, -1))
		if count == 0 {
			continue
		}
		c.commandsByDay[day] = addCommandCount(c.commandsByDay[day], name, count)
		c.commandsByHost[host] = addCommandCount(c.commandsByHost[host], name, count)
		c.commandsByHarness[harness] = addCommandCount(c.commandsByHarness[harness], name, count)
	}
}

func addCommandCount(counts commandCounts, name commandName, amount int) commandCounts {
	switch name {
	case commandFind:
		counts.Find += amount
	case commandNew:
		counts.New += amount
	case commandCheck:
		counts.Check += amount
	case commandGuide:
		counts.Guide += amount
	}
	return counts
}

func (c *collector) commandHost(cmd, localHost string) string {
	if !hostRunRe.MatchString(cmd) && !sshRe.MatchString(cmd) {
		return localHost
	}
	for _, h := range c.hosts {
		if h.re.MatchString(cmd) {
			return h.name
		}
	}
	return "unknown"
}
