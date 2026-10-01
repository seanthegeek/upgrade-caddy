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
- `install` (not implemented) builds, validates against the live config,
  swaps the binary atomically and restarts the service.

`build` and `install` refuse to operate on binaries that carry no Go module
information (distribution packages), because the plugin set cannot be
reproduced from them. `check` still works on those and says so.

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

## Layout

```text
main.go                 command dispatch and flags
internal/check          the check command: gathers, compares, prints
internal/build          the build command: resolves a plan, runs xcaddy, verifies, writes the lockfile
internal/caddybin       inspect a caddy binary: build info, list-modules, version
internal/goproxy        @latest lookups against a Go module proxy
internal/semver         version comparison including pseudo-versions
internal/pkgmgr         dpkg / rpm / pacman / apk ownership of a file
internal/systemd        find service units whose ExecStart runs a binary
```

## Build

```bash
go build -o upgrade-caddy .
go test ./...

# or, once pushed:
go install github.com/seanthegeek/upgrade-caddy@latest
```
