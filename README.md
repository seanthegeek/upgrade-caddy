# upgrade-caddy

Keeps a custom-built Caddy current without pulling the plugin list out from
under you. It reads the plugin set and pinned versions from the binary that is
already installed, so a rebuild reproduces what you have with a newer Caddy.
Plugin versions are never bumped unless you ask.

## Commands

| Command   | Status          | What it does |
|-----------|-----------------|--------------|
| `check`   | implemented     | Reports whether Caddy or any compiled-in plugin is behind the latest version on the Go module proxy. Also shows which package owns the binary and which systemd services run it. |
| `build`   | not implemented | Builds a new Caddy with the same plugins at the same versions, via the xcaddy library. |
| `install` | not implemented | Builds, validates against the live config, swaps the binary atomically and restarts the service. |

`build` and `install` refuse to operate on binaries that carry no Go module
information (distribution packages), because the plugin set cannot be
reproduced from them. `check` still works on those and says so.

## check

```
upgrade-caddy check [--binary PATH] [--json] [--timeout 60s] [--include-major]
```

Exit status: `0` everything is current, `2` updates are available, `1` error.

Go keeps the major version in the module path (`.../caddy/v2`), so a plain
latest-version query never sees a new major. `check` also probes the next
major path for Caddy and every plugin and reports any it finds. A newer major
is shown but does not affect the exit status unless `--include-major` is
given, because `build` will never cross a major on its own: plugins built for
one major do not compile against the next, and Caddy has not historically
backported security fixes to a previous major.

Latest versions come from the first proxy in `$GOPROXY`, defaulting to
`proxy.golang.org`. Plugins pinned to an untagged commit are reported as
pseudo-versions and flagged as "newer commit on default branch" rather than
as a release.

## Layout

```
main.go                 command dispatch and flags
internal/check          the check command: gathers, compares, prints
internal/caddybin       inspect a caddy binary: build info, list-modules, version
internal/goproxy        @latest lookups against a Go module proxy
internal/semver         version comparison including pseudo-versions
internal/pkgmgr         dpkg / rpm / pacman / apk ownership of a file
internal/systemd        find service units whose ExecStart runs a binary
```

## Build

```
go build -o upgrade-caddy .
go test ./...

# or, once pushed:
go install github.com/seanthegeek/upgrade-caddy@latest
```
