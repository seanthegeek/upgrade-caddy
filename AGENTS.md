# AGENTS.md

Instructions for AI coding agents working on this project.

## What this project is

`upgrade-caddy` is a Go command-line tool that keeps a custom-built
[Caddy](https://caddyserver.com) web server current without losing the
plugins compiled into it. It has three commands, plus `version`:

- `check` reports whether Caddy, or any plugin compiled into the installed
  binary, is behind the latest version on the Go module proxy. Implemented.
- `build` builds a new Caddy with the same plugins at the same pinned
  versions, using the xcaddy library, and writes a lockfile. Implemented.
- `install` builds (or takes a prior build), validates against the live
  config, swaps the binary with a rollback copy kept, restarts the service
  and rolls back if it does not come up. Implemented.

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
  exit code. It also holds the `version` variable. No other logic lives
  here.
- `CHANGELOG.md` follows Keep a Changelog. Every user-visible change gets a
  line under `[Unreleased]` in the same commit that makes it.
- `internal/check` the check command: gathers, compares, prints.
- `internal/install` the install command, the only package that changes
  system state. `Resolve` decides everything (target, units, config to
  validate, root needed, build plan) with no side effects; `Run` validates,
  swaps, restarts and rolls back in a fixed order. `swap`, `rollback`,
  `restartAndVerify` and `validate` are small and tested on temp files and
  a fake `systemd.Controller`.
- `internal/build` the build command. `Resolve` turns the installed binary
  and flags into a `Plan` with every version decided and talks only to the
  module proxy, so it is tested with a fake one. `Plan.Build` runs xcaddy,
  inspects the result to confirm it matches the plan, moves it into place
  and writes `<output>.lock.json`.
- `internal/caddybin` inspects a Caddy binary: Go build info read straight
  from the file, plus `caddy version` and `caddy list-modules` run as
  subprocesses.
- `internal/goproxy` `@latest` lookups and newer-major probing against a Go
  module proxy.
- `internal/semver` version comparison, including Go pseudo-versions.
- `internal/pkgmgr` asks dpkg, rpm, pacman or apk which package owns a file.
- `internal/systemd` finds service units whose `ExecStart` runs a binary,
  parses their config flags, and drives systemctl through a `Controller`
  interface so install's restart sequence can be tested with a fake.
- `README.md` user-facing docs.
- `.github/workflows/ci.yml` runs the hermetic checks on every push to
  main and every pull request, then `ci/integration.sh` on throwaway Ubuntu
  runners.
- `.github/workflows/codeql.yml` runs CodeQL's security-and-quality
  queries over the Go code and the workflow files on pushes, pull requests
  and weekly. Findings land in the repository's Security tab and on pull
  requests; treat them like failing tests.
- `ci/integration.sh` the privileged integration test: first install,
  in-place upgrade with a running unit and file capabilities, rollback,
  a root build, and the refusal over Ubuntu's package. It writes a unit
  file and runs the tool as root, so it refuses to run unless
  `UPGRADE_CADDY_CI=1` is set. Never run it on a machine you care about.

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
   cannot have its plugin set reproduced, so `build` refuses it unless
   `--fresh` says to ignore it, and `install` refuses it outright. `check`
   still works and says why. `IsDistroBuild()` means exactly "no module
   info", nothing else.
6. **`install` never writes over a system package, and there is no bypass
   flag.** Package ownership is checked separately from distro build: the
   official Caddy apt repository installs a dpkg-owned `/usr/bin/caddy` that
   *does* carry module info, and it is still refused, because the next
   package upgrade would undo the install. The refusal message tells the
   user to uninstall the package, unmask and recreate the unit per Caddy's
   manual-install docs, and then run `install --fresh --target`. The Debian
   specifics in that message (unit left masked, `caddy` user kept,
   `/etc/caddy` kept on remove and deleted on purge) were verified against
   Ubuntu's maintainer scripts and are stated only for dpkg. The author
   chose this over a `dpkg-divert` escape hatch; do not add one.
7. **`check` is strictly read-only.** It is the tool used to debug the other
   two commands, so it must never be the thing that changes state. The only
   subprocesses it may run are the Caddy binary itself with `version` and
   `list-modules`, and read-only package manager and systemctl queries.
8. **Build info is read from the file, not from the binary's own report.**
   `debug/buildinfo.ReadFile` is the source of truth for versions and
   checksums; the version `list-modules` prints is kept only when build
   info has none. The binary is executed only for `version` and for
   `list-modules`, which maps Caddy module IDs to Go module paths, something
   build info does not contain.
9. **Latest versions come from the Go module proxy, not the GitHub API.**
   One mechanism covers Caddy and every plugin and it needs no credentials.
   `GOPROXY`, `GONOPROXY` and `GOPRIVATE` are followed the way the `go`
   command follows them: ordered sources, comma versus pipe fallback, `off`
   and `direct` meaning "cannot look up" (reported as not checked, never
   silently redirected to the public proxy), and private patterns never sent
   to a proxy. A 404 from a proxy is final; `direct` is not consulted.
   `file://` proxies are read from disk. Lookups use the proxy's optional
   `@latest` endpoint and do not apply retractions, and the newer-major
   probe gives up after two consecutive missing major numbers; these are
   documented limitations, not bugs to fix quietly. A hard lookup failure
   (timeout, 5xx, bad response) makes `check` exit 1, never 0.
10. **Exit codes are a contract.** `0` current, `1` error, `2` updates
    available. Scripts and cron jobs rely on them. Report output goes to
    stdout and errors to stderr, and `--json` output must stay parseable.

## Rules for `install`

These are implemented in `internal/install`; keep them true.

- `validate` runs with the *new* binary against every distinct live config
  (taken from each unit's `--config`, `--adapter` and `--envfile` flags, in
  its `WorkingDirectory`) before anything changes. No config known means a
  warning and no validation, not a failure.
- The new binary is produced or staged in the target's own directory, the
  current one is hard-linked to `<target>.previous`, then the new one is
  renamed over the target. There is never an instant without a binary at
  the path. Mode and `getcap` file capabilities carry over, and owner does
  when running as root.
- The service is found by scanning unit `ExecStart` paths for the target.
  It is never assumed to be `caddy.service`.
- Restart, don't reload. Reload keeps the old process and so the old binary.
- After restart, wait for active and for it to stay active, and where
  `/proc/<MainPID>/exe` is readable (root, or the same user) confirm it
  is the target. On failure restore `.previous`, restart again, keep the
  bad binary as `<target>.failed`, exit 1.
- Root is required only for what actually needs it (unwritable directory,
  unit restart, `setcap`), and that is checked before the build starts.
  `--from` exists so the build can run as a normal user.
- The lockfile from the build is moved to `<target>.lock.json`, the old one
  is kept as `<target>.lock.json.previous`, and a rollback restores both
  the binary and the lockfile. Failing to install the lockfile is an
  install failure that rolls the binary back.
- Package ownership must be known before the target is touched.
  `pkgmgr.Find` fails closed: a package manager that cannot answer either
  way is an error, and `install` refuses on it rather than assuming "not
  owned".
- Rollback restores the previous binary before trying to keep the failed
  one as `.failed`, and its restarts run under a fresh deadline because the
  caller's context is often already cancelled. Error messages say "rolled
  back" only when the restore succeeded.

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
- Standard library only unless a dependency earns its place. Two do:
  - the xcaddy library (`github.com/caddyserver/xcaddy`), which `build`
    uses. Read its `builder.go` and `environment.go` in the module cache
    before changing how it is driven; its `OnStep` callback and the way it
    derives the Caddy module path from the version's major are the parts
    this tool depends on;
  - `golang.org/x/mod`, the Go team's own implementation of module version
    rules, used for version comparison, pseudo-version detection,
    major-version path suffixes (including the `gopkg.in` dot form), proxy
    path escaping and `GONOPROXY` matching. `internal/semver` and
    `internal/goproxy` are thin layers over it; do not reimplement any of
    those rules by hand, a review found four edge-case deviations in the
    previous hand-rolled versions.
- `go.mod` declares Go 1.22 and the local toolchain is 1.22 with
  `GOTOOLCHAIN=auto`. Building Caddy itself needs whatever Caddy's own
  `go.mod` asks for (1.26 at the time of writing); xcaddy shells out to `go`
  and the auto toolchain switch handles the download.
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

# build: dry runs are cheap and cover Resolve end to end
./upgrade-caddy build --dry-run                      # refuses on the distro build here
./upgrade-caddy build --fresh --dry-run --with github.com/caddy-dns/cloudflare --output /tmp/x

# Markdown
npx --yes markdownlint-cli '*.md' --disable MD013
```

All of these should pass before a change is considered complete. `check`
should be run against both a distribution build (Ubuntu's `/usr/bin/caddy`)
and an upstream release binary (download the tarball from GitHub releases
and pass `--binary`) whenever `internal/caddybin` or `internal/check`
changes, since the two take different paths through the code.

`install` cannot be run for real on this host: `/usr/bin/caddy` is Ubuntu's
package (refused by design) and sudo needs a password. What can be run
unprivileged, and should be after any change to `internal/install`:

```bash
# in a scratch directory holding a binary that build produced
./upgrade-caddy install --target /usr/bin/caddy --dry-run           # refusal with uninstall instructions
./upgrade-caddy install --target scratch/caddy-x --config scratch/Caddyfile   # build, validate, swap, .previous
./upgrade-caddy install --from scratch/caddy-x --target scratch/new/caddy     # stage without building
./upgrade-caddy install --from scratch/caddy-x --target scratch/new/caddy --config scratch/bad.Caddyfile  # must change nothing
```

The restart, verify and rollback sequence is covered by the fake
`systemd.Controller` tests locally and for real by `ci/integration.sh` on
GitHub Actions, where the runner is a throwaway VM with systemd and
passwordless sudo. A change to `internal/install` or `internal/systemd` is
not validated until that job is green.

A real `build` (no `--dry-run`) downloads a Go toolchain and Caddy's
dependencies and takes several minutes. Run one to a scratch path whenever
`Plan.Build`, the step logger or the lockfile writer changes, then run
`check --binary` on the result: a built binary with a plugin is the only
way to exercise check's plugin table against real data.

## GitHub releases

- Releases are made by version tag, not branch. Tags are prefixed with `v`.
  Release titles exclude the `v` (GoReleaser's `name_template` does this).
- `.github/workflows/release.yml` runs on every `v*` tag: it checks the
  tag matches `version` in `main.go`, extracts that version's section from
  `CHANGELOG.md` as the release notes, and runs GoReleaser with
  `.goreleaser.yaml`, which builds static (`CGO_ENABLED=0`, `-trimpath`)
  archives for linux/amd64, linux/arm64, linux/armv7, darwin/amd64,
  darwin/arm64 and freebsd/amd64 plus `checksums.txt`. Windows is not a
  target: `install` uses Unix-only calls and there is no systemd.
- To release: set `version` in `main.go`; in `CHANGELOG.md` turn the
  `[Unreleased]` section into `[X.Y.Z] - YYYY-MM-DD`, add an empty
  `[Unreleased]` above it, and update the link definitions at the bottom
  (`[Unreleased]: .../compare/vX.Y.Z...HEAD` and
  `[X.Y.Z]: .../releases/tag/vX.Y.Z`); commit, tag `vX.Y.Z`, push the tag.
  The workflow refuses if the tag and `main.go` disagree or the changelog
  has no section for the version.
- Verify `.goreleaser.yaml` changes locally with
  `goreleaser release --snapshot --clean` (output in `dist/`, ignored).
- `go install github.com/seanthegeek/upgrade-caddy@<tag>` must keep working,
  which means the module path in `go.mod` must not change.

## Documentation

`README.md` gives an overview. As the tool grows, command-level detail
belongs in bite-sized pages under `docs/` rather than a monolithic readme.

## Out of scope

- Service managers other than systemd. When systemctl is absent `install`
  says so in its plan and swaps the binary without restarting anything; it
  does not try to drive OpenRC, launchd or Windows services.
- Caddy's download/build server, in any form. See invariant 4.
- Making distribution builds "work". The refusal in invariant 5 is the
  correct behaviour, not a placeholder.
- Managing the Caddyfile or Caddy's configuration. This tool only replaces
  the binary.
