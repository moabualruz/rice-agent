<!-- DRY_POLICY_START -->
## DRY principle (owner requirement)

Decide reuse before acting, in work, orchestration and code. When work is about to repeat, build the
tool first; the current task is its first user. agent-dry (its CLI, global tools and this section) is
a global exception to per-project configuration, like zvec-grep and herdr. Details: `agent-dry guide`
(tool contract: `agent-dry guide tools`); design: `rice-kb:docs/specs/agent-dry.md`.

- Work: before a multi-step unit of work, and before each new procedure inside it, run
  `agent-dry find <intent words>` (agent-dry tools, this project's commands, installed skills, known
  recurring work, earlier misses). Reuse or extend a match. Build first when nothing fits and the plan
  repeats an operation three or more times, `find` reports WANTED or SEEN, the work recurs or a second
  project or host needs it, it crosses a fragile boundary (SSH and fish quoting, credentials, remote
  or destructive changes, parsing) twice or more, or its raw output needs filtering. Otherwise use
  native CLIs inline; `find` records the miss.
- Orchestration: look up before fan-out and build a procedure shared by several workers once, before
  dispatch. Give each worker the tool names, research and evidence its scope needs; one owner per tool
  or shared change, so parallel workers never build the same thing.
- Code: search before writing a helper, constant, type or schema and extend what exists; keep one
  source of truth for each rule, constant and procedure; share across repositories through a library
  or a vendored pinned release (`agent-dry vendor`), never a pasted copy.
- Placement: logic about one repository goes in its scripts or justfile in its language; generic
  logic becomes an agent-dry tool (`agent-dry new`, then `agent-dry check`); a skill only for judgment
  a script cannot encode.
<!-- DRY_POLICY_END -->
