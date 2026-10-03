# CLAUDE.md

@AGENTS.md

## Claude Code

- Use plan mode before editing the binary swap, restart or rollback in
  `internal/install`. That is the one place this tool changes system state,
  and a mistake there takes a production web server down. The rules it must
  keep are listed in AGENTS.md under "Rules for `install`".
- "Validated" means every command in AGENTS.md's "Validating changes"
  section passes locally, including a `check` run against both a
  distribution build and an upstream release binary when `internal/caddybin`
  or `internal/check` changed. The privileged paths are only validated by
  the CI integration job; say so when reporting on changes to them.
- Running `check` locally is safe; it is read-only by design (invariant 7).
  `build` to a scratch path is safe. `install` against the host's Caddy is
  refused by design (it is Ubuntu's package), and sudo is not available
  without a password, so test `install` only in a scratch directory as in
  AGENTS.md's validation section.
- When a parser needs a new output format, capture the real output with the
  actual command first and put it in the test as a constant with a comment
  saying where it came from. Do not invent sample output.
- Design decisions already made with the author (plugin set from the binary,
  versions pinned, no build server, refuse on distro builds, majors never
  crossed automatically) are recorded in AGENTS.md. Do not re-ask them.
- When editing files with a script, compute every replacement before
  opening anything for writing, and make the doc edits part of the same
  guarded step as the code edits, before the commit. Two rounds landed
  code without their docs because a README pattern failed after the commit
  command was already queued, and one script truncated a test file by
  opening it for writing before the replacement raised.
- Copilot review threads on #1 number in the dozens and every reply is
  itself a "review" in the API, so always list reviews with
  `gh api --paginate`; without it the newest Copilot review falls off the
  first page and looks absent.
- Each Copilot round on this project has produced three to eight confirmed
  defects, mostly in the patterns under "Lessons from review" in AGENTS.md.
  Run `/address-copilot-review` until a round confirms nothing new.
