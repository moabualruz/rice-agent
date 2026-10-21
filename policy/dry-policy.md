<!-- DRY_POLICY_START -->
## Engineering principles (owner requirement)

Every project, product code and tooling alike: reuse, adapt, improve, reduce, recycle, decided
before acting. Details: `agent-dry guide <topic>`; agent-dry is a global exception.

- In a resumed or long-running session, re-read `~/.config/agent-instructions/shared/KERNEL.md` before the next multi-step task.
- Every multi-step task starts with `agent-dry find <intent>`, before any subagent fan-out; then state the choice: reuse, extend, build or inline.
- Think first: goal, assumptions and success checks; ask when unclear; plan small steps.
- Research first: before building, search this project (zvec-grep), the owner's tools
  (`agent-dry find`) and the web (`oss-evidence`); rethink, then adopt or adapt before writing.
- DRY: one source of truth per rule, constant, schema and procedure; extend existing modules;
  share by library or `agent-dry vendor`, never copies; build the tool first when work repeats
  3+ times or spans projects or hosts.
- Change surgically: simplest sufficient diff, no drive-by changes.
- Verify through the gates: local hooks and CI run diagnostics, build and tests; failures come
  back as repairs; nothing waits on checks. Optimize only on measured gains (`ratchet`).
<!-- DRY_POLICY_END -->
