# CLAUDE.md

@AGENTS.md

## Claude Code

- Use plan mode before implementing or editing the binary swap and service
  restart in `install`. That is the one place this tool changes system
  state, and a mistake there takes a production web server down. The rules
  it must follow are listed in AGENTS.md under "Rules for `install`".
- This project has no CI yet. "Validated" means every command in AGENTS.md's
  "Validating changes" section passes, including a `check` run against both
  a distribution build and an upstream release binary when `internal/caddybin`
  or `internal/check` changed.
- Running `check` locally is safe; it is read-only by design (invariant 7).
  Running `build` or `install` locally is not, once they exist: the host's
  Caddy is Ubuntu's package and is in use.
- When a parser needs a new output format, capture the real output with the
  actual command first and put it in the test as a constant with a comment
  saying where it came from. Do not invent sample output.
- Design decisions already made with the author (plugin set from the binary,
  versions pinned, no build server, refuse on distro builds, majors never
  crossed automatically) are recorded in AGENTS.md. Do not re-ask them.
