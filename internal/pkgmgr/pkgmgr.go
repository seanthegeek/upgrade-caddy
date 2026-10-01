// Package pkgmgr finds out whether a file on disk is owned by a system
// package manager.
//
// Each manager has a thin runner that shells out and a pure classifier
// that turns the command's exit status and output into one of three
// answers: owned (by whom), not owned, or unknown. Unknown is an error, so
// callers that gate a safety decision on ownership fail closed. Commands
// run with LC_ALL=C so the messages the classifiers look for are stable.
package pkgmgr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Owner describes the package that installed a file.
type Owner struct {
	Manager string `json:"manager"`
	Package string `json:"package"`
	Version string `json:"version,omitempty"`
}

// ErrUnknown wraps a failure to determine ownership either way.
var ErrUnknown = errors.New("could not determine package ownership")

// Find returns the package owning path, nil when the installed package
// manager positively reports the file as not owned, and an error wrapping
// ErrUnknown when it could not tell. With no package manager present the
// answer is nil, nil.
func Find(ctx context.Context, path string) (*Owner, error) {
	type probe struct {
		bin string
		fn  func(ctx context.Context, path string) (*Owner, error)
	}
	for _, p := range []probe{
		{"dpkg", dpkg},
		{"rpm", rpm},
		{"pacman", pacman},
		{"apk", apk},
	} {
		if _, err := exec.LookPath(p.bin); err != nil {
			continue
		}
		o, err := p.fn(ctx, path)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrUnknown, p.bin, err)
		}
		if o != nil {
			return o, nil
		}
	}
	return nil, nil
}

// result is what a package manager command produced.
type result struct {
	stdout, stderr string
	code           int // -1 when the command could not run at all
}

func run(ctx context.Context, name string, args ...string) (result, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	r := result{stdout: strings.TrimSpace(out.String()), stderr: strings.TrimSpace(errb.String()), code: 0}
	if ctx.Err() != nil {
		return r, ctx.Err()
	}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		r.code = ee.ExitCode()
	default:
		return result{code: -1}, err
	}
	return r, nil
}

func dpkg(ctx context.Context, path string) (*Owner, error) {
	r, err := run(ctx, "dpkg", "-S", path)
	if err != nil {
		return nil, err
	}
	pkg, err := classifyDpkg(r)
	if err != nil || pkg == "" {
		return nil, err
	}
	ver, _ := run(ctx, "dpkg-query", "-W", "-f=${Version}", pkg)
	return &Owner{Manager: "dpkg", Package: pkg, Version: ver.stdout}, nil
}

// classifyDpkg reads `dpkg -S`: exit 0 with an owner line means owned, exit
// 1 with "no path found matching pattern" means not owned, anything else is
// unknown.
func classifyDpkg(r result) (pkg string, err error) {
	switch {
	case r.code == 0:
		if pkg = parseDpkgSearch(r.stdout); pkg == "" {
			return "", fmt.Errorf("unrecognised output: %q", r.stdout)
		}
		return pkg, nil
	case r.code == 1 && strings.Contains(r.stderr, "no path found matching pattern"):
		return "", nil
	}
	return "", fmt.Errorf("exit %d: %s", r.code, firstNonEmpty(r.stderr, r.stdout))
}

// parseDpkgSearch extracts the first package name from `dpkg -S` output.
// dpkg-query(1) documents the forms "pkgname1, pkgname2: pathname",
// "pkgname:arch: pathname", and the diversion lines "diversion by pkgname
// from: path", "diversion by pkgname to: path" and "local diversion ...",
// which are skipped since the file is then not the package's own copy.
func parseDpkgSearch(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "diversion by ") || strings.HasPrefix(line, "local diversion") {
			continue
		}
		pkgs, rest, ok := strings.Cut(line, ": ")
		if !ok || !strings.HasPrefix(rest, "/") {
			continue
		}
		pkg, _, _ := strings.Cut(pkgs, ", ") // first of several owners
		pkg, _, _ = strings.Cut(pkg, ":")    // drop ":amd64"
		pkg = strings.TrimSpace(pkg)
		if pkg != "" && !strings.ContainsAny(pkg, " \t") {
			return pkg
		}
	}
	return ""
}

func rpm(ctx context.Context, path string) (*Owner, error) {
	// rpm prints the format once per owning package, so end it with a
	// newline and take the first line.
	r, err := run(ctx, "rpm", "-qf", "--qf", "%{NAME} %{VERSION}-%{RELEASE}\n", path)
	if err != nil {
		return nil, err
	}
	return classifyRpm(r)
}

// classifyRpm reads `rpm -qf`: exit 0 means owned, exit 1 with "is not
// owned by any package" means not owned.
func classifyRpm(r result) (*Owner, error) {
	switch {
	case r.code == 0:
		if o := parseRpmQuery(r.stdout); o != nil {
			return o, nil
		}
		return nil, fmt.Errorf("unrecognised output: %q", r.stdout)
	case r.code == 1 && strings.Contains(r.stdout+r.stderr, "is not owned by any package"):
		return nil, nil
	}
	return nil, fmt.Errorf("exit %d: %s", r.code, firstNonEmpty(r.stderr, r.stdout))
}

// parseRpmQuery parses "caddy 2.8.4-1.fc40" as produced by the query format
// used in rpm above, taking only the first line when several packages own
// the file.
func parseRpmQuery(out string) *Owner {
	first, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	name, ver, _ := strings.Cut(strings.TrimSpace(first), " ")
	if name == "" {
		return nil
	}
	return &Owner{Manager: "rpm", Package: name, Version: ver}
}

func pacman(ctx context.Context, path string) (*Owner, error) {
	r, err := run(ctx, "pacman", "-Qo", path)
	if err != nil {
		return nil, err
	}
	return classifyPacman(r)
}

// classifyPacman reads `pacman -Qo`: exit 0 means owned, exit 1 with "No
// package owns" means not owned.
func classifyPacman(r result) (*Owner, error) {
	switch {
	case r.code == 0:
		if o := parsePacmanOwner(r.stdout); o != nil {
			return o, nil
		}
		return nil, fmt.Errorf("unrecognised output: %q", r.stdout)
	case r.code == 1 && strings.Contains(r.stdout+r.stderr, "No package owns"):
		return nil, nil
	}
	return nil, fmt.Errorf("exit %d: %s", r.code, firstNonEmpty(r.stderr, r.stdout))
}

// parsePacmanOwner parses "/usr/bin/caddy is owned by caddy 2.8.4-1".
func parsePacmanOwner(out string) *Owner {
	_, rest, found := strings.Cut(strings.TrimSpace(out), " is owned by ")
	if !found {
		return nil
	}
	name, ver, _ := strings.Cut(rest, " ")
	if name == "" {
		return nil
	}
	return &Owner{Manager: "pacman", Package: name, Version: ver}
}

func apk(ctx context.Context, path string) (*Owner, error) {
	r, err := run(ctx, "apk", "info", "-W", path)
	if err != nil {
		return nil, err
	}
	return classifyApk(r)
}

// classifyApk reads `apk info -W`: exit 0 means owned, exit 1 with "Could
// not find owner package" means not owned.
func classifyApk(r result) (*Owner, error) {
	switch {
	case r.code == 0:
		if o := parseApkOwner(r.stdout); o != nil {
			return o, nil
		}
		return nil, fmt.Errorf("unrecognised output: %q", r.stdout)
	case r.code == 1 && strings.Contains(r.stdout+r.stderr, "Could not find owner package"):
		return nil, nil
	}
	return nil, fmt.Errorf("exit %d: %s", r.code, firstNonEmpty(r.stderr, r.stdout))
}

// parseApkOwner parses "/usr/bin/caddy is owned by caddy-2.8.4-r0". Alpine
// joins name and version with "-", so the split is at the first "-" that
// is followed by a digit.
func parseApkOwner(out string) *Owner {
	_, rest, found := strings.Cut(strings.TrimSpace(out), " is owned by ")
	if !found || rest == "" {
		return nil
	}
	for i := 1; i < len(rest)-1; i++ {
		if rest[i] == '-' && rest[i+1] >= '0' && rest[i+1] <= '9' {
			return &Owner{Manager: "apk", Package: rest[:i], Version: rest[i+1:]}
		}
	}
	return &Owner{Manager: "apk", Package: rest}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
