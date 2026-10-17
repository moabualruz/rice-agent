# Reuse: one source of truth across projects

Reuse is the cross-project half of DRY. It finds knowledge that exists in several places and gives it
one owner. agent-dry provides the mechanics; detection uses mature external tools.

## Find duplication

- Tool copies: `agent-dry audit` reports diverged copies of registered tools across configured
  repositories (`repos` in `~/.config/agent-dry/config.json`).
- General code: `jscpd` across several repository roots (`--skip-local` reports only clones that
  cross roots; JSON or SARIF output). Structural duplicates: `ast-grep` patterns.
- Treat results as candidates. Two similar blocks that encode different facts stay separate.

## Choose the consolidation form

| Situation | Form |
| --- | --- |
| a script or CLI several repositories call | agent-dry tool; repositories that need an in-repo copy vendor it |
| library code in one language | a package or module in that language's registry, pinned by consumers |
| configuration or policy text | one source file and an installer (`agent-dry section` for instruction sections) |
| a rule set (lint, codemod) | data in the owning tool's repository, referenced by consumers |

## Consolidate safely

1. Pick the canonical version: the union of required behavior across copies, using every caller's
   tests as the regression bar.
2. Land the canonical version in its owner first, release it (tag), then replace each copy through
   that repository's own delivery flow: `agent-dry vendor <tool> --into <repo> --as <path>`.
3. Skip a repository with someone else's uncommitted or unpushed work; record it instead.
4. Keep copies current: `agent-dry vendor --check <repo>` reports drift; Renovate can bump pinned
   releases where it is deployed.
