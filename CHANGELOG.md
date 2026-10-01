# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `check` command: reports whether the installed Caddy, or any plugin
  compiled into it, is behind the latest version on the Go module proxy.
  Detects distribution builds and package ownership, finds the systemd
  unit that runs the binary, and always counts a newer major version as an
  available update, since Caddy does not backport fixes across majors.
- `build` command: rebuilds the installed binary's plugin set at the same
  pinned versions with the latest Caddy, through the xcaddy library. Plugin
  versions change only with `--upgrade`, `--upgrade-all` or `--with`, and a
  major version is never crossed without `--allow-major`. Writes a lockfile
  with the version and checksum of Caddy and every plugin.
- `install` command: builds or takes a prior build, validates it against
  the live config, swaps it over the installed binary with no gap and a
  rollback copy kept, restarts the service and rolls back if it does not
  come up. Never installs over a system package.
- `version` command.
- GitHub Actions workflows: unit tests and lint, a privileged integration
  test on throwaway runners, CodeQL, and a release workflow that builds
  archives for Linux, macOS and FreeBSD with GoReleaser on every `v*` tag.
- Apache License 2.0.

[Unreleased]: https://github.com/seanthegeek/upgrade-caddy/commits/main
