# Scout: research before building

Scout is a procedure, not a product. Use it before building any capability that the open-source
ecosystem may already provide (a library, CLI, service or application). It ends in one verdict per
candidate and a published decision, never in code.

## When

- `agent-dry find` has no match and the work would add a new capability (not a thin composition of
  CLIs you already have).
- A tool, dependency or service is about to be introduced, replaced or upgraded across projects.

## Steps

1. State the capability in one sentence and the hard constraints: target platforms, language and
   runtime limits (no Python runtime for non-Python projects), local-first, license allowlist,
   offline use, maintenance budget.
2. Check what the owner already runs first: `agent-dry find`, the global exceptions (zvec-grep,
   herdr), owner products such as rice-edit and Rice Observe, and installed CLIs (`command -v`).
   Reuse one of those before looking outside. Then collect candidates from primary sources:
   `gh search repos <terms> --sort stars`, awesome lists (seeds only) and upstream documentation.
3. Gather evidence per candidate with one command:
   `oss-evidence <owner/repo>... [--package <ecosystem>:<name>] --json`
   (GitHub metadata, latest release, OpenSSF Scorecard and its Maintained check, deps.dev dependents,
   OSV advisories). Missing evidence stays missing; it is never scored as zero.
4. Apply vetoes first, then signals (`oss-evidence --rubric`):
   - vetoes: archived; no license or a license outside the allowlist; a runtime the owner policy
     excludes; known critical advisories without a fix; hosted only when local-first is required.
   - signals: release and commit recency, maintainers, dependents and adoption, Scorecard, fit to the
     constraints, integration cost (wrapper needed or not).
5. Verify the top candidate locally: install it in a scratch location, run its `--help` and one
   representative task, and record versions.
6. Rethink: compare the evidence with the original plan, change the plan when it shows a better or
   existing solution, and state what changed.
7. Decide with ThoughtWorks rings: adopt, trial, assess or hold. Build only when every candidate is
   vetoed or fails the representative task, and say why.
8. Publish the evidence and verdict as a Rice research or decision document (the `rice-research`
   skill), then integrate: an external entry in `registry/providers.json` for a CLI, a pinned
   dependency for a library, or an agent-dry tool only for a thin composition.

## Output

One table (candidate, verdict, vetoes hit, key evidence with retrieval date) plus the decision and
its integration path. Keep raw API output out of the conversation; `oss-evidence --json` is the record.
