# Reuse: one source of truth across projects

Reuse is the cross-project half of DRY. It finds knowledge that exists in several places and gives it
one owner. rice-agent provides the mechanics; detection uses mature external tools.

## Find duplication

- Tool copies: `rice-agent audit` reports diverged copies of registered tools across configured
  repositories (`repos` in `~/.config/rice-agent/config.json`).
- General code: `jscpd` across several repository roots (`--skip-local` reports only clones that
  cross roots; JSON or SARIF output). Structural duplicates: `ast-grep` patterns.
- Treat results as candidates. Two similar blocks that encode different facts stay separate.

## Choose the consolidation form

| Situation | Form |
| --- | --- |
| a script or CLI several repositories call | rice-agent tool; repositories that need an in-repo copy vendor it |
| library code in one language | a package or module in that language's registry, pinned by consumers |
| configuration or policy text | one source file and an installer (`rice-agent section` for instruction sections) |
| a rule set (lint, codemod) | data in the owning tool's repository, referenced by consumers |

## Consolidate safely

1. Pick the canonical version: the union of required behavior across copies, using every caller's
   tests as the regression bar.
2. Land the canonical version in its owner first, release it (tag), then replace each copy through
   that repository's own delivery flow: `rice-agent vendor <tool> --into <repo> --as <path>`.
3. Skip a repository with someone else's uncommitted or unpushed work; record it instead.
4. Keep copies current: `rice-agent vendor --check <repo>` reports drift; Renovate can bump pinned
   releases where it is deployed.
