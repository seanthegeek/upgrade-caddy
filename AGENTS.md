# AGENTS.md

Instructions for AI coding agents working on this project.

## What this project is

`upgrade-caddy` is a Go command-line tool that keeps a custom-built
[Caddy](https://caddyserver.com) web server current without losing the
plugins compiled into it. It has three commands:

- `check` reports whether Caddy, or any plugin compiled into the installed
  binary, is behind the latest version on the Go module proxy. Implemented.
- `build` builds a new Caddy with the same plugins at the same pinned
  versions, using the xcaddy library. Not yet implemented.
- `install` builds, validates against the live config, swaps the binary and
  restarts the service. Not yet implemented.

**Why this exists**: Caddy's own `caddy upgrade` and `caddy add-package`
commands are marked experimental, depend on the project's build server
(which has no uptime guarantee), and are slated for removal from core
([caddyserver/caddy#7010](https://github.com/caddyserver/caddy/issues/7010)).
The maintainers' recommendation is xcaddy. This tool wraps xcaddy with the
lifecycle pieces xcaddy leaves to the user: knowing what is installed,
knowing what is out of date, and swapping the binary safely.

Caddy's security policy supports only the latest 2.x release and has never
backported fixes to an older minor or major. "Behind latest" is therefore the
only state that matters, and the tool is built around that.

## Files

- `main.go` parses flags, dispatches to a command and maps the result to an
  exit code. No logic lives here.
- `internal/check` the check command: gathers, compares, prints.
- `internal/caddybin` inspects a Caddy binary: Go build info read straight
  from the file, plus `caddy version` and `caddy list-modules` run as
  subprocesses.
- `internal/goproxy` `@latest` lookups and newer-major probing against a Go
  module proxy.
- `internal/semver` version comparison, including Go pseudo-versions.
- `internal/pkgmgr` asks dpkg, rpm, pacman or apk which package owns a file.
- `internal/systemd` finds service units whose `ExecStart` runs a binary.
- `README.md` user-facing docs.

## Non-negotiable design invariants

These are deliberate decisions, several made for supply-chain reasons. Don't
"simplify" them away.

1. **The plugin set comes from the installed binary, never from the user.**
   It is read from the binary's embedded Go build info and `caddy
   list-modules --packages --versions`. Asking the user to retype their
   plugin list is how a DNS provider module gets dropped by accident.
2. **Plugin versions stay pinned to what is installed.** `build` and
   `install` reproduce the current plugin set at the current versions with a
   newer Caddy. Bumping a plugin only happens through an explicit flag
   (`--upgrade <module>`, `--upgrade-all`, `--with <module@version>`). A
   silent bump is a supply-chain risk the user has not reviewed.
3. **A newer major version always counts as an update, but is never
   crossed automatically.** Caddy has never backported security fixes to a
   previous major, so `check` treats a newer major as "outdated" and exits
   `2`, with no switch to hide it. Go keeps the major in the module path
   (`.../caddy/v2`) and plugins built for one major do not compile against
   the next, so `build` must refuse to cross one without an explicit flag.
   Reporting and acting are deliberately separate.
4. **Never use Caddy's download/build server.** Builds go through the xcaddy
   library on the local machine. The build server is the thing upstream is
   removing.
5. **Distribution builds are detected, reported, and refused for mutation.**
   A binary with no Go module information (Ubuntu's `caddy` package is one)
   cannot have its plugin set reproduced, so `build` and `install` refuse it.
   `check` still works and says why. `IsDistroBuild()` means exactly "no
   module info", nothing else.
6. **Package ownership is a separate flag from distro build.** The official
   Caddy apt repository installs a dpkg-owned `/usr/bin/caddy` that *does*
   carry module info. Overwriting any package-owned file is still wrong,
   because the next package upgrade undoes it, so `install` refuses on
   ownership independently. A `dpkg-divert` based `--divert` escape hatch is
   the documented way to do it if it is ever added.
7. **`check` is strictly read-only.** It is the tool used to debug the other
   two commands, so it must never be the thing that changes state. The only
   subprocesses it may run are the Caddy binary itself with `version` and
   `list-modules`, and read-only package manager and systemctl queries.
8. **Build info is read from the file, not from the binary's own report.**
   `debug/buildinfo.ReadFile` is the source of truth for versions and
   checksums. The binary is only executed to map Caddy module IDs to Go
   module paths, which build info does not contain.
9. **Latest versions come from the Go module proxy, not the GitHub API.**
   One mechanism covers Caddy and every plugin, it needs no credentials, and
   `GOPROXY` is honoured so air-gapped and corporate setups work.
10. **Exit codes are a contract.** `0` current, `1` error, `2` updates
    available. Scripts and cron jobs rely on them. Report output goes to
    stdout and errors to stderr, and `--json` output must stay parseable.

## Rules for `install` (when it is implemented)

- Run `caddy validate` with the *new* binary against the live config before
  touching anything.
- Write to a temporary path in the same directory, then rename over the old
  binary. Rename is atomic and the running process keeps its old inode.
- Keep the previous binary as a rollback target.
- Find the service by scanning unit `ExecStart` paths for the binary being
  replaced. Do not assume it is called `caddy.service`.
- Restart, don't reload. Reload re-reads config but keeps the old process
  and therefore the old binary.
- Require root only when the target directory or `systemctl` actually needs
  it. Check, don't assume.
- Write a lockfile of resolved module versions and checksums after each
  build so the next run can diff against it.

## Conventions

These rules apply to anyone, human or agent, making changes to this repo.
They are checked in rather than living in any one agent's private memory so
every collaborator picks them up the same way.

- **Wait for explicit commit AND push permission on the default branch.
  These are separate grants.** Finish the implementation, run the tests,
  summarize the diff, then stop and ask. "Commit this" mid-session counts as
  permission for that one commit, not a standing grant, and permission to
  commit is NOT permission to push. Wait for an explicit "push it" before
  `git push`. If the prior commit was itself unauthorized, do not push it to
  "tidy up"; surface the situation and let the author decide.
- **Self-test before every `git commit`:** has the author typed "commit" (or
  an unambiguous equivalent) in a present-tense imperative since your last
  commit? If not, ask. Conditional phrasings like "if everything works we
  can push" are plans to confirm, not authorizations. The literal text of
  the author's last message is the source of truth.
  - **Self-test before every `git push`:** has the author typed "push" since
    your last push? Same rule.
  - **Exception: branches you created in-session.** On a feature branch you
    created yourself this session, commit and push freely; the whole branch
    is reviewed at PR time. This never extends to `main` or to branches the
    author created.
- **Project-specific rules belong in this file, not in any agent's private
  memory.** If you catch yourself saving a rule about the codebase rather
  than about working with this particular user, write it here instead.
- **Plain language over jargon.** Comments, doc comments, this file, commit
  messages and user-facing docs should describe what the code does in words
  a non-specialist would understand. When a domain term is the right word,
  use it and add a brief gloss the first time it appears. When in doubt,
  prefer the plainer rewrite even if it is a few words longer.
- **Prefer detection over assumption for anything OS, distro or
  package-manager specific.** This tool's value is in getting those details
  right. If you are not certain about a package manager's output format, a
  systemd rendering, or a proxy's behaviour, run the command or fetch the
  page and capture the real output into a test. Several of the parsers here
  exist because an early guess about an output format was wrong.
- **Verify library behaviour against the installed version, not memory.**
  Before calling an unfamiliar function from a dependency, read its source
  in the module cache (`go env GOMODCACHE`) or run `go doc <pkg>.<Symbol>`.
  Training data is not authoritative; the downloaded module is.
- **Research order for Caddy and xcaddy behaviour: their source, then the
  official docs, then GitHub issues.** Fetch the raw file from GitHub when a
  parser depends on exact output. Blog posts, forum answers and AI-generated
  explainers are pointers to primary sources at best.
- **Read official documentation in full before implementing against an
  unfamiliar API.** Detail pages, not quick references.
- **Don't swallow errors.** Wrap with context and return them
  (`fmt.Errorf("reading build info from %s: %w", path, err)`). Define
  sentinel errors (`goproxy.ErrNotFound`) and check them with `errors.Is`.
  Discarding an error with `_` is allowed only for best-effort reads where
  the comment says so. No panics in `internal/` packages.
- **Keep `README.md` in sync** whenever flags, exit codes or behaviour
  change. The usage block and exit-code sentence there are the same
  information as `main.go`, and readers rely on both being accurate.

## Go code style

- Formatter: `gofmt`. Static checks: `go vet`. Both must be clean. No
  third-party linters are required.
- Standard library only unless a dependency earns its place. The planned
  exception is the xcaddy library for `build`, which will also raise the
  minimum Go version in `go.mod`.
- `go.mod` currently declares Go 1.22, the local toolchain is 1.22 with
  `GOTOOLCHAIN=auto`. Upstream Caddy builds with Go 1.26. Expect a toolchain
  download the first time xcaddy is added.
- One package per concern under `internal/`. Packages that shell out keep a
  thin runner and a pure parser so the parser can be tested on captured
  output without root, a package manager or systemd present.
- Exported identifiers get doc comments. Unexported helpers get one when
  the format they handle is not obvious from the code.
- Testing: the standard library `testing` package, run with `go test ./...`.
  No assertion library. Use table-driven tests (a slice of cases looped over
  in one test function) for parsers and comparisons. Fake HTTP with
  `net/http/httptest`. Tests must not need network access or root. Tests
  that use the host's Caddy skip under `-short` and when it is absent.
- Every parser, comparison and decision function gets a test. Captured real
  output (from `systemctl show`, `dpkg -S`, `caddy list-modules`) is the
  preferred test input; say where it came from in a comment.

## Markdown style

- All markdown must pass VS Code's default markdownlint config, which
  disables MD013 (line length). `.vscode/settings.json` additionally
  disables MD024 so a changelog can repeat headings.

## Validating changes

```bash
gofmt -l .            # prints nothing when clean
go vet ./...
go test ./...         # add -short to skip the test that runs the host's caddy
go build -o upgrade-caddy . && ./upgrade-caddy check

# Markdown
npx --yes markdownlint-cli '*.md' --disable MD013
```

All of these should pass before a change is considered complete. `check`
should be run against both a distribution build (Ubuntu's `/usr/bin/caddy`)
and an upstream release binary (download the tarball from GitHub releases
and pass `--binary`) whenever `internal/caddybin` or `internal/check`
changes, since the two take different paths through the code.

## GitHub releases

- Releases are made by version tag, not branch. Tags are prefixed with `v`.
  Release titles exclude the `v`.
- Attach statically linked Linux binaries for amd64 and arm64, built with
  `CGO_ENABLED=0 go build -trimpath`.
- `go install github.com/seanthegeek/upgrade-caddy@<tag>` must keep working,
  which means the module path in `go.mod` must not change.

## Documentation

`README.md` gives an overview. As the tool grows, command-level detail
belongs in bite-sized pages under `docs/` rather than a monolithic readme.

## Out of scope

- Service managers other than systemd. `install` should detect that
  systemctl is absent and say so, not try to drive OpenRC, launchd or
  Windows services.
- Caddy's download/build server, in any form. See invariant 4.
- Making distribution builds "work". The refusal in invariant 5 is the
  correct behaviour, not a placeholder.
- Managing the Caddyfile or Caddy's configuration. This tool only replaces
  the binary.
