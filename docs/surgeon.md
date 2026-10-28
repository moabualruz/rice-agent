# Surgeon: surgical, semantic code changes

Surgeon is a policy plus an index of existing engines. The owner's product `rice-edit` owns
plan-verified semantic edits (rename, move, navigate, diagnostics, experimental ast-grep rewrites;
Go, TypeScript, Rust, Java, Dart) with a journal and a verifier. Language servers and structural tools
are external CLIs listed by `rice-agent find`. rice-agent adds no editing engine.

## Choose the edit mechanism

1. Semantic first: a rename, move, extract or signature change goes through `rice-edit` when the
   language is qualified, otherwise through the language's own refactoring (for example
   `gopls rename`, rust-analyzer structural search and replace). These follow references; text
   replacement does not.
2. Structural second: pattern rewrites across many files use `ast-grep` (or the language's codemod
   tool such as OpenRewrite), with the rule kept as data in the owning repository.
3. Text last: the harness's own edit tool for local changes inside one function or file.

## Keep it surgical

- Touch only what the task needs; no drive-by reformatting, renames or reordering.
- After a semantic rename, sweep comments, strings and documentation for the old name.
- Prefer deleting code that is no longer needed over adding new code around it.

## Verify, cheapest first

Diagnostics (language server or `go vet`, `tsc --noEmit`), then build, then the owning tests. Do not
move on with a red step. Report what was verified at what scope.

## Evidence

Semantic tooling improves correctness (reference precision, lint-on-edit) more reliably than it
saves tokens; one study found language-server use usually cost more tokens. Choose it for correctness
on cross-file changes, not for brevity. Details: `rice-kb:docs/research/agent-ecosystem-surgeon-20261017.md`.
