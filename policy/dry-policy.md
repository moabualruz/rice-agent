<!-- DRY_POLICY_START -->
## Reuse principles (owner requirement)

Reuse, adapt, improve, reduce, recycle, decided before acting, in every project. agent-dry is a
global exception like zvec-grep and herdr; details: `agent-dry guide <topic>`.

- Find first: `agent-dry find <intent>` before each multi-step procedure or fan-out; reuse or extend.
- Build a tool only when work repeats 3+ times, `find` says WANTED or SEEN, it spans projects or
  hosts, crosses a fragile boundary twice or needs output filtering; scout open source first.
- Code: state assumptions, make the simplest sufficient change, search (zvec-grep) before adding a
  helper, constant or schema, keep one source of truth, edit surgically, verify with diagnostics,
  build and tests.
- Keep only measured gains (`ratchet`); share across repositories by library or `agent-dry vendor`.
<!-- DRY_POLICY_END -->
