# upgrade-caddy

Keeps a custom-built Caddy current without pulling the plugin list out from
under you. It reads the plugin set and pinned versions from the binary that is
already installed, so a rebuild reproduces what you have with a newer Caddy.
Plugin versions are never bumped unless you ask.

## Commands

- `check` (implemented) reports whether Caddy or any compiled-in plugin is
  behind the latest version on the Go module proxy. Also shows which package
  owns the binary and which systemd services run it.
- `build` (implemented) builds a new Caddy with the same plugins at the
  same versions, via the xcaddy library, and writes a lockfile recording
  every module version and checksum.
- `install` (implemented) builds, validates against the live config, swaps
  the binary atomically with a rollback copy kept, restarts the service and
  rolls back if it does not come up.
- `version` prints the tool's version, commit and Go toolchain.

`build` refuses binaries that carry no Go module information (distribution
packages), because the plugin set cannot be reproduced from them, unless
`--fresh` is given. `install` never writes over a system package at all,
whether detected by package ownership or by missing module information. See
"Coming from a distribution package" below. `check` works on everything and
says what it found.

## check

```text
upgrade-caddy check [--binary PATH] [--json] [--timeout 60s]
```

Exit status: `0` everything is current, `2` updates are available, `1` error.

Go keeps the major version in the module path (`.../caddy/v2`), so a plain
latest-version query never sees a new major. `check` also probes the next
major path for Caddy and every plugin and reports any it finds. A newer major
counts as an available update, because Caddy has never backported security
fixes to a previous major: staying on the old one means going unpatched.
`build` will still not cross a major on its own, since plugins built for one
major do not compile against the next, so a major upgrade is a deliberate
step with explicit flags.

Latest versions come from the first proxy in `$GOPROXY`, defaulting to
`proxy.golang.org`. Plugins pinned to an untagged commit are reported as
pseudo-versions and flagged as "newer commit on default branch" rather than
as a release.

## build

```text
upgrade-caddy build [--binary PATH] [--output caddy] [--caddy-version vX.Y.Z]
                    [--upgrade MODULE]... [--upgrade-all] [--with MODULE[@VERSION]]...
                    [--replace OLD=NEW]... [--allow-major] [--fresh]
                    [--dry-run] [--verbose] [--timeout 20m]
```

By default `build` reads the plugin set and pinned versions from the
installed binary and builds the latest Caddy within the same major with
exactly those plugins at exactly those versions. Nothing about a plugin
changes unless you say so:

- `--upgrade MODULE` bumps one plugin to its latest version within its
  major. `MODULE` is a Go module path or a Caddy module ID such as
  `dns.providers.cloudflare`. Repeatable.
- `--upgrade-all` bumps every plugin.
- `--with MODULE[@VERSION]` adds a plugin, or overrides the version of one
  already present. Without a version the latest is used.
- `--allow-major` is required for `--caddy-version` or `--with` to move
  anything to a different major version. Plugins built for one major do not
  compile against the next, so this is always a deliberate step.
- `--fresh` ignores the installed binary. The plugin set is then only what
  `--with` gives, which is how you build a first custom binary on a machine
  running a distribution package.
- `--dry-run` prints the resolved plan and stops.

The Go toolchain must be installed. xcaddy drives `go` from `PATH`, and
with the default `GOTOOLCHAIN=auto` a Go release new enough for the Caddy
being built is downloaded automatically.

The new binary is written to `--output` and never over the installed
binary or one a service runs; that is `install`'s job. Before it is moved
into place the binary is inspected to confirm it carries exactly the planned
Caddy and plugin versions. A lockfile at `<output>.lock.json` records the
resolved version and checksum of Caddy and every plugin.

## install

```text
upgrade-caddy install [--binary PATH] [--target PATH] [--config PATH] [--no-restart]
                      [--from PATH | build flags...] [--fresh] [--dry-run] [--verbose]
```

`install` does, in order:

1. Resolves the target (the installed binary by default), the systemd units
   whose `ExecStart` runs it, and the config to validate against (the
   unit's `--config`, `--adapter` and `--envfile` flags, or `--config`).
2. Refuses if the target belongs to a system package, and checks up front
   whether root is needed (to write the directory, restart the unit, or
   re-apply file capabilities) so a long build never ends in "permission
   denied".
3. Builds the new binary into the target's directory with the same rules
   and flags as `build`, or stages one from `--from PATH` (a binary that
   `build` produced, with its lockfile beside it). This lets the build run
   as your own user and only the swap run as root:

   ```bash
   upgrade-caddy build --output /tmp/caddy
   sudo upgrade-caddy install --from /tmp/caddy
   ```

4. Runs `validate` with the new binary against the real config. A rejected
   config stops everything before anything changes.
5. Hard-links the current binary to `<target>.previous`, then renames the
   new one over the target. There is never a moment with no binary at the
   path. Mode, owner and file capabilities (`getcap`) are carried over.
6. Restarts each unit, waits for it to become active, and when running as
   root confirms the main process executes the new binary. If that fails,
   the previous binary is restored, the unit is restarted again, the failed
   binary is kept as `<target>.failed`, and `install` exits 1.

`--no-restart` stops after step 5. `--dry-run` prints the plan after step 2.

### Coming from a distribution package

If `/usr/bin/caddy` came from your distribution (Ubuntu's `caddy` package,
for example), `install` refuses to replace it: the next package upgrade
would silently put the old binary back. Uninstall the package first
(`sudo apt remove caddy` and so on). That also removes the package's systemd
unit and `caddy` user, while `/etc/caddy` stays. Recreate the unit and user
following [Caddy's manual installation docs](https://caddyserver.com/docs/running#manual-installation),
then do a first install from scratch:

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
Or run `go install github.com/seanthegeek/upgrade-caddy@latest`
The Go toolchain must be installed separately for
`build` and `install`; `check` needs nothing else.

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
