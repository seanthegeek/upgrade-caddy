// Package pkgmgr finds out whether a file on disk is owned by a system
// package manager.
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
	// "caddy: /usr/bin/caddy"
	out, ok := run(ctx, "dpkg", "-S", path)
	if !ok {
		return nil
	}
	pkg, _, found := strings.Cut(out, ":")
	if !found || pkg == "" {
		return nil
	}
	pkg = strings.TrimSpace(pkg)
	// Multi-arch packages print "caddy:amd64"; keep only the name.
	pkg, _, _ = strings.Cut(pkg, ":")
	ver, _ := run(ctx, "dpkg-query", "-W", "-f=${Version}", pkg)
	return &Owner{Manager: "dpkg", Package: pkg, Version: ver}
}

func rpm(ctx context.Context, path string) *Owner {
	out, ok := run(ctx, "rpm", "-qf", "--qf", "%{NAME} %{VERSION}-%{RELEASE}", path)
	if !ok {
		return nil
	}
	name, ver, _ := strings.Cut(out, " ")
	return &Owner{Manager: "rpm", Package: name, Version: ver}
}

func pacman(ctx context.Context, path string) *Owner {
	// "/usr/bin/caddy is owned by caddy 2.8.4-1"
	out, ok := run(ctx, "pacman", "-Qo", path)
	if !ok {
		return nil
	}
	_, rest, found := strings.Cut(out, " is owned by ")
	if !found {
		return nil
	}
	name, ver, _ := strings.Cut(rest, " ")
	return &Owner{Manager: "pacman", Package: name, Version: ver}
}

func apk(ctx context.Context, path string) *Owner {
	// "/usr/bin/caddy is owned by caddy-2.8.4-r0"
	out, ok := run(ctx, "apk", "info", "-W", path)
	if !ok {
		return nil
	}
	_, rest, found := strings.Cut(out, " is owned by ")
	if !found {
		return nil
	}
	// Alpine package-version strings are "name-ver-rN"; split at the first
	// "-" followed by a digit.
	for i := 1; i < len(rest)-1; i++ {
		if rest[i] == '-' && rest[i+1] >= '0' && rest[i+1] <= '9' {
			return &Owner{Manager: "apk", Package: rest[:i], Version: rest[i+1:]}
		}
	}
	return &Owner{Manager: "apk", Package: rest}
}
