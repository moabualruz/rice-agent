# Forge: verified iterative improvement

Forge is a capability, not a separate product. Building tools is the DRY workflow (`agent-dry new`,
then `agent-dry check`). Improving something iteratively follows the karpathy/autoresearch contract,
with one deterministic step, `ratchet`, called by whatever loop drives the work (the harness's goal or
loop feature, or a person). ratchet never edits code and never loops by itself.

## Contract

- program: the human-owned task statement and constraints; the agent does not edit it.
- editable surface: an explicit path allowlist; everything else, including the guard and the metric,
  is frozen.
- guard: a correctness check (tests, build) that must pass before a metric counts.
- metric: one scalar printed by a command, with a direction; repeated runs and a noise floor.
- decision: keep only a strict improvement beyond the noise floor; otherwise revert to the last kept
  commit; simpler code wins ties.
- budget: per-trial timeout and a total trial cap; stop when the budget is spent or the goal is met.
- log: one append-only row per trial (commit, metric, status, description).
- isolation and handoff: a dedicated branch; kept commits go to human review, never auto-merge.

```
ratchet init --metric "<cmd>" --direction lower --guard "<cmd>" --editable <paths> --budget 20
# loop: make one candidate change, then
ratchet try -m "<what changed>"
ratchet status
ratchet finish        # kept commits and diff for review
```

## Fit and limits

- Use it where a cheap, deterministic metric exists: test duration, binary size, lines, token counts,
  benchmark medians, failing-check counts. Do not hill-climb design quality with a model judge.
- Behavior preservation is a hard gate, not a trade-off against the metric.
- Self-healing of the tool library: CI runs `agent-dry check --all` on a schedule, so a tool that breaks
  after an environment change shows up without anyone touching it.
- Extract Forge into its own repository only when it gains its own persistent state, targets outside
  agent-dry or long-running jobs.
