# Rice Agent

Private. All rights reserved. This repository is not open source and carries no license grant.
It evolves the existing `agent-dry` repository and preserves its Git history.

Cross-project tools for coding agents (Claude Code, Codex, Gemini and Antigravity, opencode,
Cursor, Hermes). Rice Agent keeps the existing verified toolbelt and adds the canonical local
instruction writer. Agents look up existing tools before acting, and when work is about to repeat
they build a verified tool first and use it for the current task.

Use `rice-agent guide` for the toolbelt details and `rice-agent guide tools` for the tool contract.
The `agent-dry` executable is a temporary one-release alias to the same implementation.

## Use

```sh
rice-agent find watch pr checks        # look up before acting
rice-agent show pr-flow                # flags, side effects, test command
rice-agent new <name> --kind script --scope global --task <ref> --summary "Use when ..." --intent "..."
rice-agent check <name>                # admission gates G1 to G6
rice-agent install                     # link Rice Agent and global tools into ~/.local/bin
rice-agent install --remote hetzner    # hosts without Go get a bundle over SSH
rice-agent instruction render --source ~/.config/agent-instructions --output ./rendered-instructions
rice-agent instruction write --plan - < approved-instruction-plan.json
rice-catalog resolve --project P --repository github.com/OWNER/REPO --path . --job J --harness H --preview-context --packet ./catalog-packet.json --json > ./catalog-result.json
rice-agent instruction render --source ~/.config/agent-instructions --catalog-result ./catalog-result.json --catalog-packet ./catalog-packet.json --output ./catalog-render
rice-agent instruction check --catalog-export ./catalog-render/compiled/catalog-export.json
just deploy v0.2.3 --dry-run           # release a tag here and to every hosts.<name>.install host
rice-agent vendor check-no-dashes --into <repo> --as scripts/check-no-dashes.sh
rice-agent audit --since 14d            # is reuse happening: lookups, reimplementations, copies
```

Rendering writes only a new output directory. It does not edit instruction sources, install links,
or alter harness configuration. The Catalog result and context packet must match, be complete, and
contain no missing mandatory guidance. The output includes `catalog-export.json` as a pinned,
versioned offline bundle and `CATALOG_GUIDANCE.md` with rendered rules, plan templates and
asset-free skills, including pack revisions and entry and pack digests. The packet retains every
entry. Capability references and skills with assets stay in the JSON bundle and are not flattened
into instruction text. `instruction check` validates the bundle without Catalog access. `--source`
remains the local instruction-tree fallback, and invalid Catalog content never silently falls back
to it. Compare
the `compiled/` tree with current outputs before any separate cutover.

For migrated rules, `instruction source-plan` derives the existing writer plan directly from the
pinned export rather than maintaining a second authored payload. A mapping explicitly binds each
target to one pack revision, entry, digest and prior file hash. For example, in a scratch fixture:

```json
{
  "schema": "rice-agent/instruction-sources/v1",
  "root": "/absolute/scratch/instruction-fixture",
  "files": [{
    "path": "shared/modules/example.md",
    "before_sha256": null,
    "mode": 420,
    "ref": {"pack": "namespace/owner-core", "revision": "approved-revision", "entry": "example"},
    "digest": "sha256:<exact-entry-digest>"
  }]
}
```

Use the actual entry reference and digest from the approved export. `before_sha256` is the bare
64-character SHA-256 of an existing target, or null only when creating a new target. Mode 420 is
JSON's decimal representation of permission mode 0644. Planning writes nothing and rejects stale
snapshots, symlinks, unsafe paths, missing or empty rules, mismatched pins and duplicate ownership.
A previously materialized rule can be planned again without changing its bytes.

```sh
rice-agent instruction source-plan --catalog-export ./catalog-render/compiled/catalog-export.json --mapping ./source-mapping.json > ./source-plan.json
rice-agent instruction write --plan - < ./source-plan.json
```

Retain the approved mapping and Catalog export alongside the plan for complete lineage. Review the
plan before the separate write. Rule files are generated exports; edit their authored Guidance
entries, then resolve and approve the next pinned export. Compare every materialized rule with its
authored entry and render the scratch instruction tree before any separately authorized host cutover.
Unmapped kernel and module fallbacks are not changed. This command does not publish packs, alter
bindings or establish that an arbitrary local export was approved by Catalog.

`rice-agent instruction write --plan -` is the pinned local materializer for product-owned instruction
plans. It receives explicit roots, relative paths, expected previous hashes and content over stdin,
refuses path escapes, symlinks and changed snapshots, and writes each file atomically. It performs no
network lookup, so an approved offline release works the same way. Callers must verify the executable
version and digest before delegating.

The `agent-dry` alias remains only for the bounded migration release. It delegates to the same Rice
Agent command implementation and is removed after known callers and approved-host cutovers are
verified. The GitHub repository rename is a coordinator step after this local migration is verified.

## Tools

| Tool | Use when |
| --- | --- |
| `check-no-dashes` | gating staged changes, a range or the tree against em and en dashes |
| `host-run` | running a command or script on a configured remote host (fish-safe, stdin bound) |
| `job-watch` | running and observing a long local job without hand-written poll loops |
| `pr-flow` | checking, watching and landing pull requests and workflow runs |
| `repo-state` | one compact snapshot of a repository's branch, HEAD and dirty state |
| `bao-run` | running a command with an OpenBao token that never reaches argv or output |

`rice-agent find` is the source of truth; this table is a convenience.

## Planning

Rice Observe effort `rice-agent-v1` in `rice-kb`: [design](rice-kb:docs/research/agent-projects-into-rice-20261028.md),
tickets under `rice-kb:.scratch/rice-agent-v1/issues/`. Historical implementation work remains in
`rice-kb:.scratch/agent-dry-v1/` and its accepted spec `rice-kb:docs/specs/agent-dry.md`.

## Layout

| Path | Content |
| --- | --- |
| `cmd/rice-agent` | canonical Rice Agent CLI |
| `cmd/agent-dry` | temporary one-release CLI alias |
| `internal/` | registry, admission, distribution, instruction rendering and audit |
| `schemas/tool/v1` | tool manifest JSON Schema |
| `tools/<name>/` | tools: `tool.json`, entry, `test.sh` |
| `registry/wanted.json` | recurring work with evidence and no tool yet |
| `policy/dry-policy.md` | current `DRY_POLICY` section source during migration |
| `docs/` | the guide and tool contract embedded in the binary |
| `testdata/` | fixtures per package |

## Checks

```sh
git config core.hooksPath .githooks
just ci
```
