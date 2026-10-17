# DRY guide

The always-on `DRY_POLICY` section in the global agent instructions is the summary. This guide is the
detail it links to (`agent-dry guide`; the tool contract is `agent-dry guide tools`). Full design:
`rice-kb:docs/specs/agent-dry.md`.

## The rule

Decide reuse before acting. Every multi-step unit of work starts with a lookup and one explicit
decision. When the work is about to repeat, the tool is built first and the current task is its first
user. Nothing is reconstructed from a finished transcript: there is no "save what I did for next time"
step, because the lookup already records what it could not find.

## Work

### 1. Find

```
agent-dry find <intent words>
```

Run it before a multi-step unit of work and again before each new procedure inside it (a remote
change, a PR landing, a data transform, a sweep over repositories). Phrase the intent the way you
would describe the job: `agent-dry find watch pr checks`, `agent-dry find ssh to mkh-pc run script`.

It searches agent-dry tools, the current project's justfile recipes, package scripts, Makefile
targets and `scripts/`, installed skills, recurring families that still lack a tool, and earlier
misses. Read the result:

| Line | Meaning | Action |
| --- | --- | --- |
| `name  summary  [kind scope] entry` | an existing tool or project command | use it; `agent-dry show <name>` for flags |
| `WANTED <family>: build <name> before use` | known recurring work with no tool yet | build it first (trigger T2) |
| `SEEN <date> <project>: earlier miss` | this work was done inline before | build it first (trigger T2) |
| no output, exit 1 | nothing known | decide below; the miss is now recorded |

### 2. Decide

State one line before acting:

```
reuse <tool> | extend <tool>: <change> | build <name>: <trigger> | inline: <reason>
```

Build first when nothing fits and any trigger holds:

| Trigger | Holds when | Example |
| --- | --- | --- |
| T1 repetition in the plan | the plan repeats a similar operation 3 or more times | the same check on 10 repositories, a poll until green, a retry loop |
| T2 confirmed recurrence | `find` reports WANTED or SEEN | a second session needing the same remote procedure |
| T3 declared recurrence | the user, a runbook, a schedule or CI says it recurs, or two projects or hosts need it now | a rollout to three hosts |
| T4 fragile boundary | deterministic work across SSH and fish quoting, credentials, remote or destructive changes, or structured parsing, needed twice or more | login, read and revoke against OpenBao |
| T5 output reduction | raw output would be large (about 2,000 tokens or more) and you need a small part of it, read twice or more | failed CI log tails, JSON field extraction |

Otherwise work inline with native CLIs. Inline is also right for one-shot runs, exploratory spikes and
anything a single installed CLI call already owns (`gh pr view 12` is not a tool; the PR landing
sequence is). Why not build on first sight: agents over-build when they start writing code, and
one-off tools rarely pay back; repetition you can see is the signal that does.

### 3. Reuse or extend

Use a match whose preconditions hold. When it nearly fits, extend it with a flag or subcommand rather
than writing a second tool: keep its tests green and add one for the new case.

### 4. Build

```
agent-dry new <name> --kind script --scope global --task <ticket or request> --summary "Use when ..." --intent "..."
# implement the smallest tool that serves the current task, parameters instead of constants
agent-dry check <name>
agent-dry install
```

`new` runs the duplicate check first and refuses when a similar tool exists (use `--extends` or
`--distinct-from --reason`). Commit through the owning repository's normal flow, then use the tool for
the work that triggered it. New tools start in `trial`; the audit promotes them after two real uses.

### Placement

| What | Home |
| --- | --- |
| logic about one repository's code, build or data | that repository's `scripts/` or justfile, in its own language (`--scope project`) |
| generic or cross-project logic: owner infrastructure, forge lifecycle, harness operations, policy checks, fleet sweeps | an agent-dry tool (`--scope global`) |
| judgment no script can encode | a short agent-dry skill whose deterministic steps call tools |
| a rule every agent must always know | the global instructions (rare) |

Bash for thin compositions of CLIs, Go for structured, concurrent or long-lived tools, TypeScript only
where the consumer already uses it, never Python.

## Orchestration

- Look up before fan-out. Before dispatching workers, run `find` for the procedures they will repeat.
  When several workers will perform the same procedure (T1 across workers), build the tool once
  before dispatch and give every worker its name.
- Pass references, not reconstructions. Give each worker the tool names, existing research, Rice
  documents and evidence its scope needs, so it does not rediscover them.
- One owner per tool and per shared change. Parallel workers never build the same helper; a worker
  that needs a new generic tool asks the integration owner, who builds or assigns it once.
- Delegate a search once. After delegating discovery, wait for that result instead of repeating it,
  and reuse delivered evidence under its gate's rules instead of re-running checks.
- Workers follow this guide inside their scope; their reports name tools used, extended or built.

## Code

- Search the codebase before writing a helper, constant, type or schema, with zvec-grep (`zg query`
  or its MCP tools for concepts, `zg query --rg` or `rg` for exact names); extend what exists.
  agent-dry never indexes project code: zvec-grep owns code search, agent-dry only its registry.
- Keep one source of truth for every rule, constant, schema and procedure. Generate derived copies
  instead of hand-copying them, and point documents at the owner instead of restating it.
- DRY is about knowledge, not similar-looking text. Two lines that encode different facts stay
  separate; extract when the second real use shares the same meaning.
- Across repositories, share through a library, a pinned dependency or a vendored agent-dry release,
  never a pasted copy. A diverged copy is a defect: reconcile it to one canonical version.
- When consolidating duplicates, the original callers' tests are the regression bar.

## Vendoring

A repository that needs a tool inside its own tree (a CI gate, a pre-commit hook) vendors it:

```
agent-dry vendor check-no-dashes --into <repo> --as scripts/check-no-dashes.sh
agent-dry vendor --check <repo>
```

The repository gains `.agent-dry/vendor.json` (source, ref, files) and `.agent-dry/SHA256SUMS`, which
`sha256sum -c` or `shasum -a 256 -c` verifies from the repository root without agent-dry installed.
`vendor --check` reports integrity failures and copies that are behind the canonical tool.

## Lifecycle

- `trial` to `active` after successful use in two distinct tasks (audit) or owner promotion.
- `deprecated` keeps working and names its replacement; `retired` leaves lookups and needs
  `superseded_by`. Nothing is deleted.
- Edits are small diffs with the existing tests as the bar.

## Measurement

`agent-dry audit --since 14d` reports the lookup-first rate, inline reimplementations of registered
tools, PR lifecycle commands per merge, raw SSH and hand-built OpenBao logins, diverged copies across
repositories, and tools to promote or retire. It measures; it never writes tools.
