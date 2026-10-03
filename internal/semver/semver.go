// Package semver is a thin layer over golang.org/x/mod, which is the Go
// team's own implementation of module version rules and what the go
// command itself uses. The helpers here only add the conveniences this
// tool needs: tolerating a missing "v" prefix and returning the major as a
// number.
package semver

import (
	"strconv"
	"strings"

	"golang.org/x/mod/module"
	xsemver "golang.org/x/mod/semver"
)

// Canonical returns v with a leading "v" and no surrounding whitespace.
func Canonical(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if v[0] != 'v' {
		v = "v" + v
	}
	return v
}

// IsValid reports whether v is a valid semantic version once a "v" prefix
// is supplied.
func IsValid(v string) bool {
	return xsemver.IsValid(Canonical(v))
}

// IsFull reports whether v is a valid semantic version spelled out in full
// (major, minor and patch), as module.CanonicalVersion would leave it. A
// short form such as "v1.2" is valid to the semver package but is a prefix
// query to the go command, not a version.
func IsFull(v string) bool {
	v = Canonical(v)
	return xsemver.IsValid(v) && module.CanonicalVersion(v) == v
}

// IsPseudo reports whether v is a Go pseudo-version (e.g.
// v0.0.0-20240814120000-0123456789ab), which means the module was pinned
// to an untagged commit. Build metadata such as +incompatible or Go 1.24's
// +dirty does not affect the answer.
func IsPseudo(v string) bool {
	return module.IsPseudoVersion(Canonical(v))
}

// Compare returns -1 if a < b, 0 if a == b, +1 if a > b, following
// semantic version precedence as the go command applies it. Build metadata
// is ignored. An invalid version sorts before any valid one, and invalid
// versions compare equal to each other.
func Compare(a, b string) int {
	return xsemver.Compare(Canonical(a), Canonical(b))
}

// Major returns the major version number of v, or -1 when v is not a valid
// semantic version (a branch name or commit hash, for example).
func Major(v string) int {
	m := xsemver.Major(Canonical(v))
	if m == "" {
		return -1
	}
	n, err := strconv.Atoi(m[1:])
	if err != nil {
		return -1
	}
	return n
}
