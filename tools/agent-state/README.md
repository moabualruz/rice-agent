# agent-state

`agent-state` provides one layout and retention policy for local agent execution state. It keeps goal ledgers in their canonical location, stores evidence as manifests and files, gives scratch work an expiring lease, and writes reviewed plans before cleanup.

## Safety policy

- Scratch defaults to a three-day lease. Expired scratch is eligible for a plan only after checks for Git dirt or unpushed commits, pins, open files and process cwd, live database lock PIDs, active ledger references, and unreadable data.
- Ledger references are active when their recorded turn is within seven days. Configure `agent_state.active_ledger_days` to change that window.
- Closed evidence has a 14-day local grace period. A GC plan may propose archive-then-stub candidates, but it never archives evidence itself. Each archive and stub requires `archive <bundle> --apply --plan <id>` with a matching reviewed plan.
- `gc --apply --plan <id>` stages only scratch candidates in registry trash. It does not purge trash. Evidence candidates require the archive command.
- Evidence snapshots use the configured restic repository and password reference. The default retention policy is 12 months for evidence snapshots. Goal ledgers are kept forever. The retention policy command also requires an apply plan.
- Archive, verify, and restore refuse to run unless an archive repository and password reference are configured. Password retrieval uses `bao-run`; secret values are not stored or printed.
- Paths in configuration, pins and manifests are data. The gh-helper worktree pin is not compiled into the tool and can be changed with `pin add`, `pin rm`, or `agent_state.pinned_paths`.

## Configuration

Read `agent_state` from `~/.config/agent-dry/config.json`, or the file named by `AGENT_DRY_CONFIG`. Environment variable `AGENT_STATE_ROOT` overrides the root. The standard root is `~/.local/state/agent-execution`.

```json
{
  "agent_state": {
    "root": "<absolute-state-root>",
    "active_ledger_days": 7,
    "scratch_ttl": "72h",
    "evidence_grace": "336h",
    "project_budget_bytes": 2147483648,
    "warn_total_bytes": 8589934592,
    "fail_total_bytes": 16106127360,
    "pinned_paths": [],
    "archive": {
      "repo": "<restic-repository-reference>",
      "password_ref": "<openbao-path>#<key>",
      "retention_months": 12
    }
  }
}
```

Archive setup is intentionally separate. `agent-state` does not create an OpenBao key, initialize a restic repository, or infer repository credentials.

## Commands

- `report [--project P] [--brief] [--json]` reports logical sizes by project and class against the two-GiB project, eight-GiB warning, and 15-GiB failure budgets.
- `lint [--project P]` identifies forbidden build output, dependencies, database data, nested Git repositories, compiled files, source copies and redacted secret findings.
- `gc --dry-run [--class scratch|evidence] [--plan-dir DIR]` writes a plan file with an ID. The default directory is `_registry/plans`; `--plan-dir` stores the only copy in the selected directory, which can keep inventory runs outside the state root.
- `migrate [--dry-run] [--plan-dir DIR]` plans manifests for legacy `<project>/evidence/<bundle>` directories. `migrate --apply --plan <id> [--plan-dir DIR]` writes only the reviewed manifests and a receipt. It never moves, deletes or archives legacy content.
- `pin add <path> --reason <text>`, `pin rm <path>`, and `pin list` manage cleanup exclusions.
- `doctor` reports root, registry, command and archive configuration status without contacting the archive.
- `ledger path <project> <goal-id>` prints the canonical ledger path.
- `scratch new <project> [--ttl 3d] [--env]`, `scratch renew <lease-id>`, and `scratch release <lease-id>` manage scratch leases.
- `evidence new <project> --goal <id> --criterion <text> [--source <host>/<owner>/<repo>@<sha>]`, `evidence put <bundle> <file> --role <role>`, and `evidence close <bundle> --result <pass|fail|setup-failure>` manage bundles.
- `archive <bundle> --apply --plan <id> [--plan-dir DIR]` archives and verifies a closed bundle before replacing it with a stub. `archive --policy [--plan-dir DIR]` writes an exact snapshot-retention plan; `archive --policy --apply --plan <id> [--plan-dir DIR]` removes only snapshots named by that reviewed plan.
- `verify <bundle>|--all [--sample 5%]` checks restic data and restored file hashes. `restore <bundle> [--to PATH]` verifies and restores a bundle.

`gc` and `migrate` are dry-run by default. Exit code 4 means a safety gate blocked the operation and no change was made.

Migration requires a unique ledger with a matching `goal_id` and filename, associated by an absolute bundle/file reference, a project-qualified `agent-state://` reference, or an exact goal/bundle name match. Conflicts and missing associations are reported, not guessed. Protected, pinned, active-ledger-referenced, locked and unreadable entries remain unmanaged, with reasons in the plan. Existing manifests are never overwritten. Publication is atomic and create-only; a manifest that appears concurrently is skipped and reported. Receipts distinguish migrated and skipped entries. Rollback removes only the files this transaction published, preserving concurrent files. Plans bind the inferred manifest, ledger hash, payload fingerprint and policy; changed inputs require a new plan.

Migrated manifests record kind `evidence`, file sizes and SHA256 hashes. Known class directories supply file roles; other files are marked `legacy`. Creation time is the oldest file mtime (the directory mtime for an empty bundle). Last use is the latest file or ledger timestamp. These are conservative inventory timestamps, not proof of closure: manifests start open with no invented criterion or result. Close them explicitly before planning archival. Repeating migration skips managed bundles; applying an empty plan writes nothing. Apply holds the registry lock through journaled, fsynced atomic writes and receipt creation; any failure rolls back the entire batch.
