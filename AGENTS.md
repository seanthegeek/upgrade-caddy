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
  inspects the result to confirm it matches the plan, checks again that no
  unit runs the output path (the check `Run` made before the build is
  minutes old by then), moves it into place and writes
  `<output>.lock.json`.
- `internal/caddybin` inspects a Caddy binary: Go build info read straight
  from the file, plus `caddy version` and `caddy list-modules` run as
  subprocesses.
- `internal/goproxy` `@latest` lookups and newer-major probing against a Go
  module proxy.
- `internal/semver` version comparison, including Go pseudo-versions.
- `internal/pkgmgr` asks dpkg, rpm, pacman, apk, Homebrew or FreeBSD `pkg`
  which package owns a file, failing closed when none can say.
- `internal/systemd` finds service units whose `ExecStart` runs a binary,
  parses their config flags, and drives systemctl through a `Controller`
  interface so install's restart sequence can be tested with a fake. A
  bare executable name in `ExecStart` (`caddy run`, allowed since systemd
  239) is looked up the way systemd does before running it: in the unit's
  `ExecSearchPath=` when set, else systemd's compiled-in default path
  (`systemd-path search-binaries-default`), never the unit's `PATH`.
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
   silent bump is a supply-chain risk the user has not reviewed. The same
   goes for module replacements: one recorded in the installed binary,
   for a plugin or for Caddy itself, must be kept with `--replace` or
   dropped with `--drop-replace` (or the whole binary ignored with
   `--fresh`), whatever `--upgrade` or `--with` say about versions, since
   choosing a version is not choosing a source. The one exception is
   `--with` naming a different major's module path, which is a new source
   outright; the old path's replacement cannot apply and is dropped with a
   note. `verify` checks the output carries exactly the replacements asked
   for, no more and no fewer, across every module in the build (the
   binary's full replacement list, read from build info), not just Caddy
   and the plugins that register Caddy modules, so a `--replace` for a
   plain dependency or a mistyped path is caught too.
3. **A newer major version always counts as an update, but is never
   crossed automatically.** Caddy has never backported security fixes to a
   previous major, so `check` treats a newer major as "outdated" and exits
   `2`, with no switch to hide it. Go keeps the major in the module path
   (`.../caddy/v2`) and plugins built for one major do not compile against
   the next, so `build` must refuse to cross one without an explicit flag.
   Reporting and acting are deliberately separate. A bare path holds v0,
   v1 and `vN+incompatible` alike, so the installed version's major, not
   the path's, decides what counts as "newer major" and where probing
   starts. A gopkg.in path spells its major out, `.v0` included, and
   `.v0` is major 0, so that `.v1` counts as newer. A branch or commit
   given to `--with` is resolved by `go get` after `Resolve` has run, so
   `verify` applies the same rule to the resolved version: on a bare path
   it may not land in another major than the installed one without
   `--allow-major`. A `vN+incompatible` release on the bare path and a
   `/vN` module path are one major, counted once by `check`.
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
   `list-modules`, and read-only package manager and systemd queries
   (`systemctl show`, `busctl get-property`, `systemd-path`).
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
   `file://` proxies are parsed as URLs and read from disk; a bare host
   gets `https://`; the variables are read from the process environment
   and then the `GOENV` file, as `go env` resolves them. One lookup is
   made per Go module, however many Caddy modules it registers. Lookups
   use the proxy's optional
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
  its `WorkingDirectory`) before anything changes. The working directory
  is the one systemd would use: `/` when the unit sets none, the unit
  user's home for `~` (looked up through `os/user`; when that fails the
  unit is refused for validation rather than validated in the wrong
  place). No config known means a warning and no validation, not a
  failure.
- The target is resolved through symlinks first and the real file is what
  is replaced; hard-linking and renaming a symlink would leave the referent
  and every service executing it on the old binary. `swap` refuses a
  symlink as a second line of defence.
- The new binary is produced or staged in the target's own directory, the
  current one is hard-linked to `<target>.previous`, then the new one is
  renamed over the target. There is never an instant without a binary at
  the path. Only "not there" counts as a first install: any other failure
  to stat the target, the current lockfile or `.lock.json.previous` stops
  the install, since going on would replace a file with no rollback copy
  or delete the live lockfile. Owner is applied first (chown clears set-ID bits), then every
  mode bit `os.Chmod` accepts, so setuid, setgid and sticky survive; owner
  applies only when running as root, and as root any chown failure (a
  root-squashed export, a restricted user namespace) fails the swap rather
  than installing a binary with the wrong owner. Capabilities are read and written as the raw
  `security.capability` extended attribute (Linux only; a no-op elsewhere):
  no `getcap`/`setcap` dependency, and an unreadable attribute is an error
  in `Resolve`, never "no capabilities", because a rename drops them.
- Staged files (`--from` copies, the build output's lockfile) are created
  with `os.CreateTemp`, unpredictable and exclusive, and written through
  the open handle, so a path planted in a shared directory is never
  followed by a privileged install.
- When a unit has several `ExecStart` commands, the config flags come from
  the command whose executable is the target, not the first one, and the
  argv comes from the unit's D-Bus `ExecStart` property (`busctl
  --json=short`), which keeps argument boundaries that `systemctl show`
  flattens; the flattened parse is only the fallback when busctl fails.
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
  way is an error, and so is finding no package manager at all on macOS or
  FreeBSD (Homebrew and `pkg` are probed there); `install` refuses on it
  rather than assuming "not owned". On Linux, none of the known managers
  being present means not owned.
- Error messages never contain proxy credentials: our own errors run
  GOPROXY URLs through `url.Redacted`, an entry that cannot be parsed is
  named by position only (url.Parse's error echoes its input), and
  net/http redacts its own.
- `--from` accepts only a binary whose lockfile describes it
  (`Lockfile.Describes`: Caddy path, version and checksum, exactly equal
  and empty included, and the exact plugin set with versions and
  checksums), since the lockfile is installed beside the target as its
  attestation. The identity compared is the full one the lockfile
  records: for Caddy, path, version, checksum and replacement; for
  plugins, module ID, package, version, checksum and replacement, with
  each registration counted (a Go module registering two Caddy modules
  must be listed twice). The replacement matters because a module
  replaced by a local directory has no checksum, so the directory is all
  that tells two such builds apart. `Resolve` checks the source pair, and
  `Run` checks the staged copies again before validating, because the
  source directory is typically writable by a less privileged user and
  either file could have been replaced between the two.
- `Resolve`'s checks are repeated by `recheck` right before validation
  and the swap, because a build can take minutes: the target must resolve
  to the same file (same device, inode, size and modification time), still
  belong to no package, carry the same capabilities, and be run by the
  same units with the same working directory and config flags (anything
  that changes what `validate` would check). Any difference is a refusal
  that asks for a re-run, never a silent re-plan.
- After a restart, a main process whose `/proc/<pid>/exe` is missing has
  vanished and fails verification; only a permission error falls back to
  the active state.
- Rollback restores the previous binary before trying to keep the failed
  one as `.failed`, returns the restore outcome separately from the
  diagnostic-copy outcome, and its restarts are verified the same way as
  the forward ones under a fresh deadline per unit, because the caller's
  context is often already cancelled. Error messages say "rolled back" only
  when the restore succeeded, and add "did not come back up" when the
  verified restart on the restored binary failed. On a first install the
  state to restore is "nothing there": a failed restart stops every unit
  that was restarted, the one that failed verification included (it was
  restarted too and may still be active), then removes the new binary and
  lockfile again; if any stop fails, the files are left in place and the
  error says so, because removing a binary from under a service that may
  still run leaves it on an unlinked executable. When installing the
  lockfile fails after the old one was moved aside, the old one is put
  back and a failure to do that is reported too, naming
  `.lock.json.previous`. A `MainPID` query failure fails verification;
  only a PID of 0 falls back to the active state.
- `main` cancels the context on SIGINT and SIGTERM, so a service manager,
  CI cancellation or `timeout(1)` lets install roll back instead of dying
  mid-swap.
- The new binary never runs as root before it is installed: a plugin's
  initialisation code runs the moment the binary starts, before the
  service's user and sandbox would apply, and the lockfile beside a
  `--from` binary describes it without authenticating it. When `install`
  runs as root, `Resolve` picks the accounts once (`chooseAccounts`) and
  the plan prints them: `version` and `list-modules` run as the user who
  invoked sudo (`SUDO_UID`), else the first unit's user, else root, said
  plainly; `validate` runs as the unit's `User=`, `Group=` and
  `SupplementaryGroups=` (a `caddybin.Account`, resolved through
  `os/user`), because that is who opens the config at runtime. A unit
  without `User=` runs as root and is validated as root, with its
  `Group=` and exactly its `SupplementaryGroups=` when it sets them and
  nothing from the group database, which is what systemd does
  (`get_supplementary_groups` in `src/core/exec-invoke.c`, v255; a root
  service with a restricted capability set reads files by its groups
  like anyone else); a `DynamicUser=` unit, whose account exists only
  while it runs, is validated as the inspection account; a unit user or
  group that cannot be looked up is a refusal. `Account.Apply` treats a
  different supplementary group list as a different identity. A Go
  module that registers several Caddy modules is named to `--upgrade`
  and `--drop-replace` by any of its IDs.
  The staged copy and the build output are made executable before they
  are inspected so another account can run them, and a unit whose user
  changed between `Resolve` and the swap fails `recheck`.

## Lessons from review

Eight rounds of Copilot review plus a layered cross-check review found
about fifty real defects in this codebase. Almost all of them fell into a
handful of patterns. Check new code against this list before calling it
done; each item names where the pattern bit before.

- **An error is never a negative answer.** When a check that gates a safety
  decision cannot be performed, that is a refusal, not the permissive
  branch. Bitten by: package ownership (command failure read as "not
  owned"), file capabilities (missing `getcap` read as "none"), the
  newer-major probe (failure read as "no newer major"), `MainPID` and
  `/proc/<pid>/exe` lookups (error read as "verified"), modules without
  package metadata (silently dropped), `systemctl show` failing (read as
  "no unit runs this binary"), a response body that failed to read (bytes
  that arrived parsed anyway), a stat failure on the target before the
  swap (read as "first install", so no `.previous`). Write the error
  path first.
- **Every state change has a verified inverse, and the first install is
  its own case.** Define the pre-change state explicitly (for a first
  install it is "nothing there, units stopped"), restore it on any failure,
  verify the restore the same way as the forward step, and word the
  message from the actual outcome. Bitten by: lockfile outside the
  rollback, rollback restarts not verified, a cancelled context reused
  for recovery, "rolled back" claimed when the restore failed, a failed
  `.failed` link aborting the restore, earlier units left running after a
  failed first install.
- **Nothing from configuration goes into an error message unredacted.**
  `GOPROXY` entries carry credentials; `url.Redacted` for anything
  parseable, position only for anything that is not (`url.Parse`'s error
  echoes its input). Bitten four times across three rounds.
- **"Like the go command" means read the go command.** `cmd/go` source is
  in `$(go env GOROOT)/src/cmd/go`; `modfetch/proxy.go` for GOPROXY,
  `cfg/cfg.go` for GOENV. Cite the file in the comment. Bitten by: `off`
  and `direct`, scheme-less hosts, `file://` URLs, the GOENV file, an
  entry list with no entries, and (declined, correctly) an unreadable
  GOENV file that the go command itself ignores.
- **Parse structured output, not display output.** `systemctl show`
  flattens argv, so a path with a space is lost; the D-Bus property via
  `busctl --json=short` keeps it. Caddy's `list-modules --json` exists on
  newer versions and would replace the text parser the same way. When
  only display output exists, capture it from a real system into the test
  fixture and say so.
- **The module path does not say what the major is.** A bare path holds
  v0, v1 and vN+incompatible; the installed version's major decides what
  "newer major" and "within major" mean, and v0 is a major like any other.
  Bitten three times.
- **Temporary files are exclusive and unpredictable** (`os.CreateTemp`,
  written through the handle), never a derived or PID-based name opened
  with `O_TRUNC`. Bitten by the build lockfile and the staging files.
- **Edge-case checklist for anything touching binaries, units or
  versions:** a symlinked target; an argument containing a space; several
  units running one binary; several `ExecStart` commands in one unit; a Go
  module registering several Caddy modules; v0, v1, `+incompatible` and
  `gopkg.in` paths; a first install versus an upgrade; a cancelled
  context; set-ID and sticky mode bits; macOS and FreeBSD, which have none
  of the Linux package managers; a stray positional argument; a unit with
  `User=` or `DynamicUser=`; a replaced module under `--upgrade` or
  `--with`; a check made before a build that takes minutes and acted on
  after it.

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
