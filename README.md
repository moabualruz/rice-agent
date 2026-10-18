# agent-dry

Private. All rights reserved. This repository is not open source and carries no license grant.

Cross-project DRY tooling for coding agents (Claude Code, Codex, Gemini and Antigravity, opencode,
Cursor, Hermes). Agents look up existing tools before acting, and when work is about to repeat they
build a verified tool first and use it for the current task. Reuse is decided at the start of the
work, not reconstructed afterwards.

The rule every agent follows is the `DRY_POLICY` section in the global instruction files; the detail
is `agent-dry guide`, and the tool contract is `agent-dry guide tools`.

## Use

```sh
agent-dry find watch pr checks        # look up before acting
agent-dry show pr-flow                # flags, side effects, test command
agent-dry new <name> --kind script --scope global --task <ref> --summary "Use when ..." --intent "..."
agent-dry check <name>                # admission gates G1 to G6
agent-dry install                     # link agent-dry and global tools into ~/.local/bin
agent-dry install --remote hetzner    # hosts without Go get a bundle over SSH
just deploy v0.2.3 --dry-run          # release a tag here and to every hosts.<name>.install host
agent-dry vendor check-no-dashes --into <repo> --as scripts/check-no-dashes.sh
agent-dry audit --since 14d           # is DRY happening: lookups, reimplementations, copies
```

## Tools

| Tool | Use when |
| --- | --- |
| `check-no-dashes` | gating staged changes, a range or the tree against em and en dashes |
| `host-run` | running a command or script on a configured remote host (fish-safe, stdin bound) |
| `job-watch` | running and observing a long local job without hand-written poll loops |
| `pr-flow` | checking, watching and landing pull requests and workflow runs |
| `repo-state` | one compact snapshot of a repository's branch, HEAD and dirty state |
| `bao-run` | running a command with an OpenBao token that never reaches argv or output |

`agent-dry find` is the source of truth; this table is a convenience.

## Planning

Rice Observe effort `agent-dry-v1` in `rice-kb`: spec `rice-kb:docs/specs/agent-dry.md`, decision
`rice-kb:docs/adr/agent-dry-proactive-tool-first-20261017.md`, map and tickets under
`rice-kb:.scratch/agent-dry-v1/`, research `rice-kb:docs/research/agent-dry-*-20261017.md`. Read one
with `rice-client document read --id <reference>`.

## Layout

| Path | Content |
| --- | --- |
| `cmd/agent-dry`, `internal/` | the CLI: `registry` (find, show, index, guide), `gates` (new, check), `dist` (install, doctor, vendor, bundle, section), `audit` |
| `schemas/tool/v1` | tool manifest JSON Schema |
| `tools/<name>/` | tools: `tool.json`, entry, `test.sh` |
| `registry/wanted.json` | recurring work with evidence and no tool yet |
| `policy/dry-policy.md` | `DRY_POLICY` managed section source for the global instructions |
| `docs/` | the guide and tool contract embedded in the binary |
| `testdata/` | fixtures per package |

## Checks

```sh
git config core.hooksPath .githooks
just ci
```
