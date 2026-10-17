<!-- DRY_POLICY_START -->
## DRY tooling (owner requirement)

Decide reuse before acting. The task that needs a tool builds it first and is its first user.

- Find first: before a multi-step unit of work, and before each new procedure inside it, run
  `agent-dry find <intent words>`. It searches agent-dry tools, the current project's commands,
  installed skills and recorded earlier misses. Use a match; extend it with a flag or subcommand
  when it nearly fits.
- Build first when nothing fits and any trigger holds: the plan repeats a similar operation three
  or more times; `find` reports the work as WANTED or SEEN; the work is declared recurring or two
  projects or hosts need it; it is deterministic work across a fragile boundary (SSH and fish
  quoting, credentials, remote or destructive changes, structured parsing) done twice or more; or
  its raw output needs filtering before you read it. Otherwise use native CLIs inline; `find`
  already recorded the miss.
- Place it once: project-specific logic in that project's scripts or justfile, in its language;
  generic and cross-project logic as an agent-dry tool; a skill only for judgment a script cannot
  encode, with its deterministic steps calling tools.
- Admit it verified: scaffold with `agent-dry new`, then pass `agent-dry check` (manifest,
  `--help`, exit codes, a test on the motivating case and one varied input, declared side effects
  and `--dry-run` for mutations) before relying on it.
- Keep one source of truth: a repository that needs an in-repo copy vendors a pinned release with
  a drift check; a diverged copy is a defect to reconcile.
<!-- DRY_POLICY_END -->
