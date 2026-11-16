# Contributing

Rice Agent is MIT licensed (see `LICENSE`). Issues and pull requests are welcome.

## Set up

1. Install Go (the version in `go.mod`), Node 24.18.0, `just`, `jq`, `shellcheck` and `gitleaks`.
   `mise install` reads `mise.toml` for Go and Node.
2. Run the full local gate: `just ci`. `scripts/worktree-doctor.sh` checks the tools first; without
   mise, or when `CI=true`, a Node 24.18.0 already on `PATH` is accepted.
3. Optional: `git config core.hooksPath .githooks` runs the dash and secret checks before commit.

## Rules

- No em dash or en dash anywhere; `just dashes` checks the tree.
- Never commit a secret; `just secrets` runs gitleaks.
- Keep the change small and add or update a test for new behavior.
- GitHub Actions files may use only GitHub-hosted runners, read-only permissions and no secrets;
  `go test ./internal/ghworkflows` enforces this.

`rice-kb:` identifiers in manifests and docs, and the Rice Catalog commands in the README, belong to
the maintainer's private tooling. Contributions do not need them.
