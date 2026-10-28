# Optimizer: measured efficiency

Optimizer is a workflow. It changes something only after measuring it, and keeps the change only when
the measurement improves without losing correctness. It owns no service; measurements are dated local
JSON snapshots cited in Rice documents.

## Measure first

| Question | Source |
| --- | --- |
| Are agents reusing tools or re-typing them? | `rice-agent audit --since 14d [--json]` |
| How many tokens and how much money per session or day? | `ccusage` (local, `--json --offline`); the harness's own `/usage`, `/cost` and `/context` |
| How much always-on context does each harness load? | byte size of the global entry files and loaded MCP tool definitions |
| Which CI jobs fail or take long? | GitHub Actions run data (`pr-flow runs`), Rice Observe pipeline and failure records |

## Reduce, structural changes first

1. Remove duplicated or stale always-on instructions before anything else; move detail behind a
   pointer (`rice-agent guide <topic>`).
2. Keep raw output out of context: bounded tool output, filtering tools, subagents for wide searches.
3. Replace re-typed procedures with tools (the DRY workflow).
4. Only then consider output-compression add-ons, and only with an A/B measurement: independent
   tests found far smaller gains than vendors claim, sometimes higher total cost.

## Decide by A/B

Compare the same task set with and without the change, scored on billed tokens per completed task and
pass rate, with repeated runs. For a code or configuration change with a scalar metric, run it through
`ratchet` (keep only a strict, guarded improvement). Record the before and after snapshot in Rice.

## Known check

When Claude Code traffic goes through a third-party proxy, MCP tool search is disabled unless
`ENABLE_TOOL_SEARCH` is set, so every MCP tool definition loads up front. Check this before optimizing
anything else on such setups.
