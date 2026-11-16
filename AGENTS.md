# AGENTS.md - Rice Agent

## Purpose

Cross-project agent tooling and canonical instruction writer, released under the MIT License.
It evolves the earlier agent-dry repository while preserving history. `rice-agent` is the canonical
CLI; `agent-dry` is a temporary one-release alias to the same implementation.

## Tracking and planning

- The maintainer tracks work in a private Rice Observe instance (effort `rice-agent-v1`, source `rice-kb`);
  `rice-kb:` identifiers in manifests and docs point there. Read and write it with Rice MCP tools or
  `rice-client document read|write`.
- Keep planning, research and status in Rice. This repository holds code, manifests, the policy
  section source and code-facing docs.

## Working rules

- Start implementation work in a `wt` worktree created from freshly fast-forwarded `main`. Preserve history and unrelated work. Never force push or change GitHub visibility; repository rename requires the coordinator step.
- Enable the hook once per clone: `git config core.hooksPath .githooks` (dash check and gitleaks).
- No Python in any form. Bash for thin CLI compositions, Go for the `rice-agent` CLI and structured
  or concurrent tools, TypeScript only where the consuming installer already uses it.
- No em dash or en dash in any file, commit message or Rice text. Use a hyphen, comma, semicolon,
  colon or parentheses.
- No AI attribution trailers in commits.
- Never commit secrets. Refer to secrets by name or OpenBao reference only.
- Generic content only: no CuraOS or other proprietary content. Host addresses and service endpoints
  come from configuration, never from literals in a tool.
- Find before adding: run `rice-agent find <intent>` and extend an existing tool before creating a
  new one; create tools only with `rice-agent new` so the duplicate check runs first.
- Every tool ships a manifest, `--help`, documented exit codes and a test; mutating tools declare
  side effects and support `--dry-run`.
- `policy/dry-policy.md` must begin and end with its `DRY_POLICY` markers; the installer refuses
  anything else.

## Delivery checks

- `just ci` (vet, test, build, `rice-agent check --all`, dash and secret checks)
- `bash tools/check-no-dashes/check-no-dashes --tree`
- `gitleaks git --no-banner --redact`
- CI (`.github/workflows/ci.yml`) runs the guards job (dash and secret checks) and the go job (vet,
  test, build, `rice-agent check --all`) on GitHub-hosted runners (`runs-on: ubuntu-latest`). No workflow may use `pull_request_target`, a
  secret or a self-hosted runner for pull requests; `scripts/workflows.test.sh` enforces it.
