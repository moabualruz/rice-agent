# AGENTS.md - agent-dry

## Purpose

Private repository for the owner's cross-project agent tooling: reusable tools (scripts and CLIs),
the `agent-dry` registry CLI that finds them, the rare skill a script cannot replace, and the source
of the `DRY_POLICY` managed instruction section. Private, all rights reserved.

## Tracking and planning

- Rice Observe is the tracker: effort `agent-dry-v1` in the `rice-kb` source. Plan map
  `rice-kb:.scratch/agent-dry-v1/map.md`, tickets `rice-kb:.scratch/agent-dry-v1/issues/`, spec
  `rice-kb:docs/specs/agent-dry.md`, decision
  `rice-kb:docs/adr/agent-dry-proactive-tool-first-20261017.md`. Read and write them with the Rice
  MCP tools or `rice-client document read|write`.
- Keep planning, research and status in Rice. This repository holds code, manifests, the policy
  section source and code-facing docs.

## Working rules

- Work on `main` only: no branches, no worktrees. `git pull --rebase` before pushing; never force push.
- Enable the hook once per clone: `git config core.hooksPath .githooks` (dash check and gitleaks).
- No Python in any form. Bash for thin CLI compositions, Go for the `agent-dry` CLI and structured
  or concurrent tools, TypeScript only where the consuming installer already uses it.
- No em dash or en dash in any file, commit message or Rice text. Use a hyphen, comma, semicolon,
  colon or parentheses.
- No AI attribution trailers in commits.
- Never commit secrets. Refer to secrets by name or OpenBao reference only.
- Generic content only: no CuraOS or other proprietary content. Host addresses and service endpoints
  come from configuration, never from literals in a tool.
- Find before adding: run `agent-dry find <intent>` and extend an existing tool before creating a
  new one; create tools only with `agent-dry new` so the duplicate check runs first.
- Every tool ships a manifest, `--help`, documented exit codes and a test; mutating tools declare
  side effects and support `--dry-run`.
- `policy/dry-policy.md` must begin and end with its `DRY_POLICY` markers; the installer refuses
  anything else.

## Delivery checks

- `just ci` (vet, test, build, `agent-dry check --all`, dash and secret checks)
- `bash tools/check-no-dashes/check-no-dashes --tree`
- `gitleaks git --no-banner --redact`
- CI (`.github/workflows/ci.yml`) runs the guards job (dash and secret checks) and the go job (vet,
  test, build, `agent-dry check --all`) on the self-hosted pool
  (`runs-on: [self-hosted, linux, x64, generic]`); never hosted runners.
