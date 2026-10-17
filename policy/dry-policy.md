<!-- DRY_POLICY_START -->
## Reuse principles (owner requirement)

Reuse, adapt, improve, reduce, recycle, decided before acting. agent-dry is a global exception, like
zvec-grep and herdr. Details: `agent-dry guide [scout|surgeon|reuse|optimizer|forge]`.

- Find first: `agent-dry find <intent>` before each multi-step procedure or fan-out
  (tools, project commands, skills, external CLIs); reuse or extend a match.
- Scout before building: prefer proven open source (`oss-evidence`). Build a tool first only when the
  plan repeats work 3+ times, `find` says WANTED or SEEN, the work spans projects or hosts, it crosses
  a fragile boundary twice, or its output needs filtering.
- Change surgically: smallest diff; semantic edits over text; verify with diagnostics, build and tests.
- Keep only measured gains: measure before optimizing; iterate with `ratchet`.
- One owner per tool or shared change; share across repositories by library or `agent-dry vendor`.
<!-- DRY_POLICY_END -->
