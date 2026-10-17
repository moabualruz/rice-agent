# agent-dry

Private. All rights reserved. This repository is not open source and carries no license grant.

Cross-project DRY tooling for coding agents (Claude Code, Codex, Gemini and Antigravity, opencode,
Cursor, Hermes). Agents look up existing tools before acting, and when work is about to repeat they
build a verified tool first and use it for the current task. Reuse is decided at the start of the
work, not reconstructed afterwards.

## Status

Planning. Repository governance and the draft policy section exist; the `agent-dry` CLI and the
first tools are tickets in effort `agent-dry-v1`.

| Document | Rice reference |
| --- | --- |
| Specification | `rice-kb:docs/specs/agent-dry.md` |
| Decision | `rice-kb:docs/adr/agent-dry-proactive-tool-first-20261017.md` |
| Plan map and tickets | `rice-kb:.scratch/agent-dry-v1/map.md`, `rice-kb:.scratch/agent-dry-v1/issues/` |
| Research | `rice-kb:docs/research/agent-dry-*-20261017.md` (Hermes, prior art, vendor mechanisms, local evidence) |

Read one with `rice-client document read --id <reference>`.

## Layout

| Path | Content |
| --- | --- |
| `policy/dry-policy.md` | `DRY_POLICY` managed section source, installed into every harness entry file by the Rice Observe section installer (ticket 06); not installed yet |
| `scripts/check-no-dashes.sh` | repository dash gate (copy of the shared variant; ticket 07 makes agent-dry its canonical home) |
| `.githooks/pre-commit` | dash check and gitleaks on staged changes |
| `cmd/`, `internal/`, `schemas/`, `tools/`, `skills/` | planned: the CLI, the `tool/v1` manifest schema, tools and skills (tickets 02 to 05) |

## Checks

```sh
git config core.hooksPath .githooks
just check
```
