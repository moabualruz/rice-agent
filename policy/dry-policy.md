<!-- DRY_POLICY_START -->
## DRY principle (owner requirement)

Decide reuse before acting, in work, orchestration and code: build a tool before work repeats and use
it for the current task. agent-dry is a global exception, like zvec-grep and herdr. Details:
`agent-dry guide`.

- Work: run `agent-dry find <intent>` before each multi-step procedure; reuse or extend a match. Build
  first (`agent-dry new`, `agent-dry check`) when the plan repeats an operation 3+ times, `find` says
  WANTED or SEEN, the work recurs or spans projects or hosts, it crosses a fragile boundary twice, or
  its output needs filtering. Otherwise work inline.
- Orchestration: look up before fan-out; build a shared procedure once before dispatch; one owner per
  tool.
- Code: search before adding a helper, constant or schema; one source of truth; share across
  repositories by library or `agent-dry vendor`, never a pasted copy.
<!-- DRY_POLICY_END -->
