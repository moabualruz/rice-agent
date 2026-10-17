# Tool contract

Every agent-dry tool follows this contract. `agent-dry check` enforces it (gates G1 to G6).

## Layout

```
tools/<name>/
  tool.json      tool/v1 manifest (schemas/tool/v1/tool.schema.json)
  <name>         executable entry, no extension, so it links onto PATH as <name>
  test.sh        tests; run from the tool directory as `bash test.sh`; exit 0 only when every case passes
  README.md      optional; only what --help cannot say
```

## Manifest essentials

- `name` equals the directory name; kebab-case; must not collide with an existing command on PATH.
- `summary` starts with "Use when", at most 80 characters, trigger first.
- `intents`: 3 to 12 phrases an agent would type when it needs the tool ("watch pr checks",
  "ssh to mkh-pc"). They drive `agent-dry find`.
- `side_effects` is the strongest effect the tool can have. Anything above `read` requires `dry_run`
  (the flag that prints the plan and changes nothing).
- `exit_codes` documents every code the tool returns.
- `replaces`: Go RE2 regular expressions matching the hand-written Bash commands this tool
  supersedes (for example an inline `rg` for U+2014). The audit counts them.
- `config`: dotted keys the tool reads from `~/.config/agent-dry/config.json`.
- `origin`: date, builder model and the motivating task; `promoted_from` when an existing script was promoted.
- New tools start with `status: "trial"`.

## Behavior

- `--help` (and `-h`) prints `Usage: <name> ...` first, then options, exit codes and one example; exit 0.
- Exit codes: 0 success, 1 negative result (check failed, nothing found, job failed), 2 usage error,
  3 environment or setup failure (missing config, missing dependency, unreachable host). A promoted
  tool may keep its established codes when they are documented in the manifest.
- Output is bounded by default (tail or limit flags with sensible defaults); never stream unbounded
  logs. Diagnostics go to stderr. Offer `--json` where scripts consume the output.
- No secret value in argv, output, logs or temporary files. Secrets are read inside the process from
  the configured reference and passed to children through the environment or stdin.
- Host addresses, endpoints and paths that differ per machine come from the config file, never literals.
- Never print raw Git remote URLs or Git configuration values (they can embed credentials).

## Portability

- Targets: macOS (arm64) and Linux (x86_64). Hetzner has no Go, just or shellcheck; tools run there.
- Bash tools use `#!/usr/bin/env bash`, `set -euo pipefail`, and stay compatible with Bash 3.2
  (no associative arrays, `mapfile`, `${var,,}` or `|&`). Handle BSD and GNU differences explicitly
  (`stat`, `date`, `sed -i`, `sha256sum` versus `shasum -a 256`).
- Required external commands go in `requires` and are checked at startup with exit 3 when missing.
- No Python in any form.

## Tests

- `test.sh` covers the motivating case, at least one varied input, the `--help` contract and each
  documented failure exit code. It uses fixtures and stubs (a stub `ssh`, `gh` or HTTP server on PATH)
  instead of live systems; live checks are recorded separately in the ticket.
- Tests must pass with `bash test.sh` on macOS and Linux and must clean up their temporary files.
