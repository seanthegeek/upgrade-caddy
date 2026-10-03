# upgrade-caddy

Keeps a custom-built Caddy current without pulling the plugin list out from
under you. It reads the plugin set and pinned versions from the binary that is
already installed, so a rebuild reproduces what you have with a newer Caddy.
Plugin versions are never bumped unless you ask.

## Commands

- `check` reports whether Caddy or any compiled-in plugin is
  behind the latest version on the Go module proxy. Also shows which package
  owns the binary and which systemd services run it.
- `build` builds a new Caddy with the same plugins at the
  same versions, via the xcaddy library, and writes a lockfile recording
  the version and checksum of Caddy and every plugin.
- `install` builds, validates against the live config, swaps
  the binary atomically with a rollback copy kept, restarts the service and
  rolls back if it does not come up.
- `version` prints the tool's version, commit and Go toolchain.

`build` refuses binaries that carry no Go module information (distribution
packages), or that list modules without package information, because the
plugin set cannot be reproduced from them, unless `--fresh` is given.
`install` never writes over a system package at all,
whether detected by package ownership or by missing module information. See
"Coming from a distribution package" below. `check` works on everything and
says what it found.

## check

```text
upgrade-caddy check [--binary PATH] [--json] [--timeout 60s]
```

Exit status: `0` everything is current, `2` updates are available, `1` error,
including when some component could not be checked because the proxy
failed, for the latest-version lookup or the newer-major probe (the JSON
report then has `has_errors: true`). A component that
cannot be looked up at all (`GOPROXY=off`, a private module) is "not
checked" and does not affect the status.

Go keeps the major version in the module path (`.../caddy/v2`), so a plain
latest-version query never sees a new major. `check` also probes the next
major path for Caddy and every plugin and reports any it finds, giving up
after two consecutive major numbers that do not exist. A bare module path
can hold v0, v1 and `vN+incompatible` releases alike, so a newer major can
also appear on the same path (a v0 plugin whose latest is v1); that is
reported as a major change too, and probing starts above the installed
major, not the path's. A newer major
counts as an available update, because Caddy has never backported security
fixes to a previous major: staying on the old one means going unpatched.
`build` will still not cross a major on its own, since plugins built for one
major do not compile against the next, so a major upgrade is a deliberate
step with explicit flags.

Latest versions come from the Go module proxy, following the same
`GOPROXY` rules as the `go` command: proxies (http, https, a bare host
which gets `https://`, or `file://`) are tried in order, a comma falls
through to the next source after a 404 or 410, a pipe after any error.
`GOPROXY`, `GONOPROXY` and `GOPRIVATE` are read from the process
environment first and then from the file `go env -w` writes (`GOENV`, or
the user config directory's `go/env`; `GOENV=off` disables it), so values
persisted that way are honoured. `GOPROXY=off` and `GOPROXY=direct` mean
nothing can be looked up, as does a `direct` reached after a failure that a
pipe let through; a `GOPROXY` that is set but lists no entries is a
configuration error, as it is for the go command; and
the component is reported as "not checked", as is any module matching
`GONOPROXY` or `GOPRIVATE`, which is never sent to a proxy. A module a proxy
answers 404 for is reported as not found even when `direct` follows, since
this tool does not consult version control. Resolution uses the proxy's
`@latest` endpoint, which the protocol marks optional but every mainstream
proxy implements; retractions are not applied.

Plugins pinned to an untagged commit are reported as pseudo-versions and
flagged as "newer commit on default branch" rather than as a release.

## build

```text
upgrade-caddy build [--binary PATH] [--output caddy] [--caddy-version vX.Y.Z]
                    [--upgrade MODULE]... [--upgrade-all] [--with MODULE[@VERSION]]...
                    [--replace OLD=NEW]... [--drop-replace MODULE]... [--allow-major]
                    [--fresh] [--dry-run] [--verbose] [--timeout 20m]
```

By default `build` reads the plugin set and pinned versions from the
installed binary and builds the latest Caddy within the same major with
exactly those plugins at exactly those versions. Nothing about a plugin
changes unless you say so:

- `--upgrade MODULE` bumps one plugin to its latest version within its
  major. `MODULE` is a Go module path or a Caddy module ID such as
  `dns.providers.cloudflare` (any of the IDs a Go module registers names
  it). Repeatable.
- `--upgrade-all` bumps every plugin.
- `--with MODULE[@VERSION]` adds a plugin, or overrides the version of one
  already present. Without a version the latest is used. A version must be
  spelled out in full (`v0.2.4`, not `v0.2`): to `go get` a prefix means
  "the highest matching version", which a plan cannot pin. The same goes
  for `--caddy-version`. Branch names and commit hashes are passed through
  for `go get` to resolve.
- `--replace OLD=NEW` passes a module replacement (a fork, a local checkout)
  to xcaddy. A replacement recorded in the installed binary, for a plugin
  or for Caddy itself, must be kept with `--replace` or dropped with
  `--drop-replace MODULE`, whatever `--upgrade` or `--with` say about
  versions; otherwise the build refuses, because quietly swapping a fork
  for the module proxy's code is a change nobody reviewed.
- `--allow-major` is required for `--caddy-version` or `--with` to move
  anything to a different major version, and for `--upgrade` or
  `--upgrade-all` to take a new major published on the same module path
  (v0 to v1, or a `+incompatible` release); without it those report the
  newer major as a note and stay put. Plugins built for one major do not
  compile against the next, so this is always a deliberate step. A branch
  or commit given to `--with` is resolved by `go get` during the build,
  so the resolved version is checked against the same rule afterwards.
  Moving a plugin installed at `vN+incompatible` on a bare module path to
  its `/vN` path is the same major and needs no flag.
- `--fresh` ignores the installed binary. The plugin set is then only what
  `--with` gives, which is how you build a first custom binary on a machine
  running a distribution package.
- `--dry-run` prints the resolved plan and stops.

The Go toolchain must be installed. xcaddy drives `go` from `PATH`, and
with the default `GOTOOLCHAIN=auto` a Go release new enough for the Caddy
being built is downloaded automatically.

The new binary is written to `--output` and never over the installed
binary or one a service runs; that is `install`'s job, and the check is
made before the build and again right before the finished binary is moved
into place, since a build takes minutes. Before it is moved
into place the binary is inspected to confirm it carries exactly the planned
Caddy and plugin versions. A plugin can depend on another module that
registers Caddy modules of its own; such modules are reported after the
build and recorded in the lockfile with source `transitive`. A lockfile at
`<output>.lock.json` records the resolved version and checksum of Caddy and
every plugin.

## install

```text
upgrade-caddy install [--target PATH] [--config PATH] [--no-restart]
                      [--from PATH | build flags...] [--fresh]
                      [--restart-wait 15s] [--timeout 20m] [--dry-run] [--verbose]
```

`install` does, in order:

1. Resolves the target (the first `caddy` on `PATH` by default; a symlink
   is followed and the file it points to is what gets replaced, even when
   that file does not exist yet), the systemd units
   whose `ExecStart` runs it (a bare executable name there is looked up
   the way systemd does, in the unit's `ExecSearchPath=` or systemd's
   default search path), and the configs to validate against (every
   distinct set of `--config`, `--adapter` and `--envfile` flags across
   those units, read from the command that runs the target via D-Bus so an
   argument containing a space survives, or the `--config` flag).
2. Refuses if the target belongs to a system package (dpkg, rpm, pacman,
   apk, Homebrew, or FreeBSD `pkg`), or if ownership cannot be established,
   which on macOS and FreeBSD includes finding no package manager at all,
   and checks up front
   whether root is needed (to write the directory, restart the unit,
   re-apply file capabilities, or validate as the account the service
   runs as) so a long build never ends in "permission denied". These
   checks are repeated right before step 4 and again right before step 5:
   if the target, its package ownership, its capabilities, the set of
   units running it, or those units' config flags, working directory or
   user changed while the build or the validation ran, nothing is touched
   and `install` asks to be re-run.
3. Builds the new binary into the target's directory with the same rules
   and flags as `build`, or stages one from `--from PATH` (a binary that
   `build` produced, with its lockfile beside it; the lockfile must
   describe that exact binary, versions, checksums and every module
   replacement included, or it is refused, and every check, the refusal
   of a binary without module information included, is repeated on the
   staged copies so the pair that gets installed is the pair that was
   checked, and the staged files' identity is checked once more right
   before the swap). Only one `install` of a target runs at a time: a
   lock file beside the target, `.<name>.upgrade-caddy.lock`, is held
   from here to the end, and a second install is refused while it is
   held. This lets the build run
   as your own user and only the swap run as root:

   ```bash
   upgrade-caddy build --output /tmp/caddy
   sudo upgrade-caddy install --from /tmp/caddy
   ```

   Running as root, `install` never executes the new binary as root
   before it is installed: a plugin's initialisation code runs the moment
   the binary starts, before the service's own user and sandbox would
   apply. Its `version` and `list-modules` run as the user who invoked
   `sudo`, or failing that as the service's user, and the plan says which.
4. Runs `validate` with the new binary against each real config, in the
   directory the service runs in (`/` when the unit sets none, the unit
   user's home for `~`) and as the account the service runs as (its
   `User=`, `Group=` and `SupplementaryGroups=`) and with the environment
   the service gets (`PATH`, `USER` and, for a `User=` unit, `HOME` and
   `LOGNAME` as systemd sets them, then `Environment=` and every
   `EnvironmentFile=` in systemd's order, explicit assignments winning;
   an optional `-` file is skipped on any failure, a required one that
   fails stops the install), so the config is read exactly as the service
   will read it and `{env.*}` placeholders expand to the service's values
   rather than the operator's. `SHELL`, the manager's own environment,
   `PassEnvironment=` and `UnsetEnvironment=` are not applied. A unit without `User=` runs as
   root and is validated as root, which is no more than the service itself
   does, with its `Group=` and `SupplementaryGroups=` still applied when
   it sets them; a `--config` given on the command line is validated as
   the user who invoked `sudo`, in that user's environment. A rejected config stops everything before anything
   changes.
5. Hard-links the current binary to `<target>.previous`, then renames the
   new one over the target. There is never a moment with no binary at the
   path. Mode (including setuid, setgid and sticky bits) and file
   capabilities are carried over (the raw
   `security.capability` attribute is copied, so no `getcap`/`setcap` is
   needed, and an unreadable attribute stops the install rather than
   silently dropping it); owner is too when running as root. The lockfile
   is installed the same way, with
   the old one kept as `<target>.lock.json.previous`; if it cannot be, the
   binary swap is undone (a first install removes the new binary again).
6. Restarts each unit, waits up to `--restart-wait` (default 15s) for it to
   become active and one more second to be sure it stays up, and where
   `/proc/<pid>/exe` is readable (root, or the same user) confirms the main
   process executes the new binary. If that fails, the previous binary and
   lockfile are restored, the units are restarted and verified again on
   the restored binary, the failed binary is kept as `<target>.failed`, and
   `install` exits 1 with a message that says whether the rollback itself
   succeeded and whether the service came back up on it. If the previous
   binary could not be put back, the new lockfile stays with the new
   binary and no unit is restarted again. On a first
   install the pre-install state is restored instead: every unit that was
   restarted, the failing one included, is stopped and the new binary and
   lockfile removed. SIGTERM,
   like Ctrl-C, cancels the run and lets this rollback happen.

`--no-restart` stops after step 5. `--dry-run` prints the plan after step 2.

### Coming from a distribution package

If `/usr/bin/caddy` came from your distribution (Ubuntu's `caddy` package,
for example), `install` refuses to replace it: the next package upgrade
would silently put the old binary back. Uninstall the package first with
`sudo apt remove caddy` or your distribution's equivalent, then check what
it left behind. On Debian and Ubuntu, `remove` deletes the unit file but
leaves `caddy.service` masked (a symlink to `/dev/null` that would swallow
a new unit file), keeps the `caddy` user, and keeps `/etc/caddy`; `purge`
would delete `/etc/caddy`. So run `sudo systemctl unmask caddy.service`,
recreate the unit following
[Caddy's manual installation docs](https://caddyserver.com/docs/running#manual-installation)
while skipping the `useradd` step if the user already exists, then do a
first install from scratch:

```bash
sudo upgrade-caddy install --fresh --target /usr/bin/caddy --with github.com/caddy-dns/cloudflare
```

After that, plain `sudo upgrade-caddy install` keeps it current.

## Layout

```text
main.go                 command dispatch and flags
internal/check          the check command: gathers, compares, prints
internal/build          the build command: resolves a plan, runs xcaddy, verifies, writes the lockfile
internal/install        the install command: validate, swap with rollback copy, restart, roll back
internal/caddybin       inspect a caddy binary: build info, list-modules, version
internal/goproxy        @latest lookups against a Go module proxy
internal/semver         version comparison including pseudo-versions
internal/pkgmgr         dpkg / rpm / pacman / apk ownership of a file
internal/systemd        find service units whose ExecStart runs a binary
```

## Install

Download the archive for your platform from the
[releases page](https://github.com/seanthegeek/upgrade-caddy/releases),
verify it against `checksums.txt`, and put `upgrade-caddy` on your `PATH`.
Builds are provided for Linux (amd64, arm64, armv7), macOS (amd64, arm64)
and FreeBSD (amd64).
Or, with Go installed, run
`go install github.com/seanthegeek/upgrade-caddy@latest`.

The Go toolchain must be installed wherever a binary is compiled: for
`build`, and for `install` without `--from`. `install --from` only stages
a binary that `build` already produced, so it runs without Go, and
`check` needs nothing else either.

## Build

```bash
go build -o upgrade-caddy .
go test ./...
```

CI runs the same checks plus `ci/integration.sh`, which exercises the
privileged paths (install, restart, rollback, package refusal) on throwaway
GitHub Actions runners. The script changes the system it runs on and
refuses to start without `UPGRADE_CADDY_CI=1`.

## License

Copyright 2026 Sean Whalen. Licensed under the
[Apache License, Version 2.0](LICENSE).
