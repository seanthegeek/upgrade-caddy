// Package pkgmgr finds out whether a file on disk is owned by a system
// package manager.
//
// Each manager has a thin runner that shells out and a pure parser that
// turns the command's output into an Owner. The parsers are what the tests
// cover, using output captured from real systems.
package pkgmgr

import (
	"context"
	"os/exec"
	"strings"
)

// Owner describes the package that installed a file.
type Owner struct {
	Manager string `json:"manager"`
	Package string `json:"package"`
	Version string `json:"version,omitempty"`
}

// Find returns the package owning path, or nil if no installed package
// manager claims it.
func Find(ctx context.Context, path string) *Owner {
	type probe struct {
		bin string
		fn  func(ctx context.Context, path string) *Owner
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
		if o := p.fn(ctx, path); o != nil {
			return o
		}
	}
	return nil
}

func run(ctx context.Context, name string, args ...string) (string, bool) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

func dpkg(ctx context.Context, path string) *Owner {
	out, ok := run(ctx, "dpkg", "-S", path)
	if !ok {
		return nil
	}
	pkg := parseDpkgSearch(out)
	if pkg == "" {
		return nil
	}
	ver, _ := run(ctx, "dpkg-query", "-W", "-f=${Version}", pkg)
	return &Owner{Manager: "dpkg", Package: pkg, Version: ver}
}

// parseDpkgSearch extracts the package name from `dpkg -S` output such as
// "caddy: /usr/bin/caddy" or "caddy:amd64: /usr/bin/caddy". Diversion
// lines ("diversion by caddy from: /usr/bin/caddy") are skipped, since the
// file is then not the package's own copy.
func parseDpkgSearch(out string) string {
	for _, line := range strings.Split(out, "\n") {
		pkg, rest, ok := strings.Cut(line, ": ")
		if !ok || !strings.HasPrefix(rest, "/") || strings.ContainsAny(pkg, " \t") {
			continue
		}
		pkg, _, _ = strings.Cut(pkg, ":") // drop ":amd64"
		if pkg != "" {
			return pkg
		}
	}
	return ""
}

func rpm(ctx context.Context, path string) *Owner {
	out, ok := run(ctx, "rpm", "-qf", "--qf", "%{NAME} %{VERSION}-%{RELEASE}", path)
	if !ok {
		return nil
	}
	return parseRpmQuery(out)
}

// parseRpmQuery parses "caddy 2.8.4-1.fc40" as produced by the query format
// used in rpm above.
func parseRpmQuery(out string) *Owner {
	name, ver, _ := strings.Cut(strings.TrimSpace(out), " ")
	if name == "" {
		return nil
	}
	return &Owner{Manager: "rpm", Package: name, Version: ver}
}

func pacman(ctx context.Context, path string) *Owner {
	out, ok := run(ctx, "pacman", "-Qo", path)
	if !ok {
		return nil
	}
	return parsePacmanOwner(out)
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

func apk(ctx context.Context, path string) *Owner {
	out, ok := run(ctx, "apk", "info", "-W", path)
	if !ok {
		return nil
	}
	return parseApkOwner(out)
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
