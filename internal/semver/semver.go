// Package semver implements the small subset of semantic version handling
// needed to compare Go module versions, including pseudo-versions.
package semver

import (
	"regexp"
	"strconv"
	"strings"
)

var pseudoRe = regexp.MustCompile(`(^|[.-])\d{14}-[0-9a-f]{12}$`)

// IsPseudo reports whether v looks like a Go pseudo-version
// (e.g. v0.0.0-20240814120000-0123456789ab), which means the module
// was pinned to an untagged commit.
func IsPseudo(v string) bool {
	return pseudoRe.MatchString(strings.TrimSuffix(v, "+incompatible"))
}

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

type parsed struct {
	nums [3]int
	pre  string
	ok   bool
}

func parse(v string) parsed {
	v = strings.TrimPrefix(Canonical(v), "v")
	v = strings.TrimSuffix(v, "+incompatible")
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	var p parsed
	if i := strings.IndexByte(v, '-'); i >= 0 {
		p.pre = v[i+1:]
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return p
	}
	for i, s := range parts {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return p
		}
		p.nums[i] = n
	}
	p.ok = true
	return p
}

// Compare returns -1 if a < b, 0 if a == b, +1 if a > b, following
// semver precedence rules. Unparseable versions sort before parseable ones
// and are compared as plain strings against each other.
func Compare(a, b string) int {
	pa, pb := parse(a), parse(b)
	switch {
	case !pa.ok && !pb.ok:
		return strings.Compare(a, b)
	case !pa.ok:
		return -1
	case !pb.ok:
		return 1
	}
	for i := 0; i < 3; i++ {
		if pa.nums[i] != pb.nums[i] {
			if pa.nums[i] < pb.nums[i] {
				return -1
			}
			return 1
		}
	}
	return comparePre(pa.pre, pb.pre)
}

func comparePre(a, b string) int {
	switch {
	case a == b:
		return 0
	case a == "":
		return 1 // release > prerelease
	case b == "":
		return -1
	}
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if as[i] == bs[i] {
			continue
		}
		an, aerr := strconv.Atoi(as[i])
		bn, berr := strconv.Atoi(bs[i])
		switch {
		case aerr == nil && berr == nil:
			if an < bn {
				return -1
			}
			return 1
		case aerr == nil:
			return -1 // numeric < alphanumeric
		case berr == nil:
			return 1
		default:
			return strings.Compare(as[i], bs[i])
		}
	}
	if len(as) < len(bs) {
		return -1
	}
	return 1
}
