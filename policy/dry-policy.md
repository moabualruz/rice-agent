<!-- DRY_POLICY_START -->
## Engineering principles (owner requirement)

In every project, for product code as much as for tooling: reuse, adapt, improve, reduce, recycle,
decided before acting. Details: `agent-dry guide <topic>`; agent-dry is a global exception.

- Think first: state the goal, assumptions and success checks; ask when unclear; plan small steps.
- Research first, local and online: before building a feature, library or tool, search this project
  (zvec-grep), the owner's tools (`agent-dry find`) and the web for proven open source and primary
  docs (`oss-evidence`). Then rethink the plan against the findings; adopt or adapt before writing.
- DRY: one source of truth for each rule, constant, schema and procedure; extend existing modules;
  share across projects by library or `agent-dry vendor`, never copies; build the helper or tool
  first when work repeats 3+ times or spans projects or hosts.
- Change surgically: simplest sufficient diff, semantic edits over text, no drive-by changes.
- Verify and measure: diagnostics, build and tests before moving on; optimize only on measured gains
  (`ratchet`).
<!-- DRY_POLICY_END -->
