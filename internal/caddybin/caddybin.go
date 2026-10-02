// Package caddybin inspects an installed Caddy binary: its version, the Go
// module information embedded at build time, the non-standard plugins it
// carries, and whether a package manager owns it.
package caddybin

import (
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"

	"github.com/seanthegeek/upgrade-caddy/internal/goproxy"
	"github.com/seanthegeek/upgrade-caddy/internal/pkgmgr"
)

// CaddyModuleBase is Caddy's module path without a major-version suffix.
// Caddy v2 lives at CaddyModulePath; a future v3 would be CaddyModuleBase
// + "/v3". Caddy v1 lived on the bare path and is not supported.
const (
	CaddyModuleBase = "github.com/caddyserver/caddy"
	CaddyModulePath = CaddyModuleBase + "/v2"
)

// IsCaddyModule reports whether path is Caddy itself at any supported
// major version (v2 or later).
func IsCaddyModule(path string) bool {
	base, major := goproxy.SplitMajor(path)
	return base == CaddyModuleBase && major >= 2
}

// Plugin is a non-standard Caddy module compiled into the binary.
type Plugin struct {
	ModuleID       string `json:"module_id"`                 // e.g. dns.providers.cloudflare
	Package        string `json:"package,omitempty"`         // Go module path
	Version        string `json:"version,omitempty"`         // Go module version as built
	Replace        string `json:"replace,omitempty"`         // replacement path if a replace directive was used
	ReplaceVersion string `json:"replace_version,omitempty"` // version of the replacement, if it is a module
	Sum            string `json:"sum,omitempty"`             // h1: checksum from build info, of the replacement when replaced
	Error          string `json:"error,omitempty"`           // error Caddy reported for this module
}

// Info is everything the tool can learn about a Caddy binary.
type Info struct {
	Path           string            `json:"path"`
	ResolvedPath   string            `json:"resolved_path"`
	Version        string            `json:"version"` // output of `caddy version`, first field
	GoVersion      string            `json:"go_version"`
	MainPath       string            `json:"main_path,omitempty"` // Caddy's module path, e.g. .../caddy/v2; "" when absent
	MainVersion    string            `json:"main_version"`        // from build info; "" when absent
	MainSum        string            `json:"main_sum,omitempty"`
	MainReplace    string            `json:"main_replace,omitempty"`         // replacement path when Caddy itself was replaced
	MainReplaceVer string            `json:"main_replace_version,omitempty"` // version of that replacement, if it is a module
	HasModuleInfo  bool              `json:"has_module_info"`                // false for distro-style builds
	Replacements   map[string]string `json:"replacements,omitempty"`         // every replace directive in effect: module path -> replacement path, "@version" appended when it is a module
	Plugins        []Plugin          `json:"plugins"`                        // non-standard modules
	UnknownModules []Plugin          `json:"unknown_modules,omitempty"`
	StandardCount  int               `json:"standard_count"`
	Owner          *pkgmgr.Owner     `json:"owner,omitempty"`         // OS package that installed the file
	OwnerUnknown   string            `json:"owner_unknown,omitempty"` // why ownership could not be determined, if it could not
	BuildSettings  map[string]string `json:"build_settings,omitempty"`
}

// IsDistroBuild reports whether the binary lacks the Go module metadata that
// upstream and xcaddy builds always carry. Without it the plugin set and
// pinned versions cannot be reproduced.
func (i *Info) IsDistroBuild() bool { return !i.HasModuleInfo }

// Find returns the binary to inspect: explicit if non-empty, else the first
// "caddy" on $PATH.
func Find(explicit string) (string, error) {
	if explicit != "" {
		return filepath.Abs(explicit)
	}
	p, err := exec.LookPath("caddy")
	if err != nil {
		return "", errors.New("no caddy binary found on PATH; pass --binary")
	}
	return filepath.Abs(p)
}

// Inspect reads build info from the file and runs the binary to list its
// modules.
func Inspect(ctx context.Context, path string) (*Info, error) {
	return InspectAs(ctx, path, nil)
}

// InspectAs is Inspect with the binary's `version` and `list-modules`
// subprocesses run as the given account; nil means the current user. See
// Account for why a binary that is not installed yet should not run as
// root.
func InspectAs(ctx context.Context, path string, as *Account) (*Info, error) {
	info := &Info{Path: path, ResolvedPath: path}
	if r, err := filepath.EvalSymlinks(path); err == nil {
		info.ResolvedPath = r
	}

	bi, err := buildinfo.ReadFile(info.ResolvedPath)
	if err != nil {
		return nil, fmt.Errorf("reading Go build info from %s: %w", path, err)
	}
	info.GoVersion = bi.GoVersion
	info.BuildSettings = map[string]string{}
	for _, s := range bi.Settings {
		info.BuildSettings[s.Key] = s.Value
	}
	// Upstream release builds have Caddy as the main module. xcaddy builds
	// have a synthetic main package named "caddy" with Caddy itself as a
	// dependency, so the real path and version live in Deps. Either way,
	// any major from v2 up is recognised.
	if IsCaddyModule(bi.Main.Path) && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		info.HasModuleInfo = true
		info.MainPath = bi.Main.Path
		info.MainVersion = bi.Main.Version
		info.MainSum = bi.Main.Sum
	}
	// When a module is replaced, build info records the original under
	// Path/Version and the effective module under Replace; checksums come
	// from the replacement, which is what was actually compiled in.
	deps := map[string]*debug.Module{}
	for _, d := range bi.Deps {
		deps[d.Path] = d
		if d.Replace != nil {
			if info.Replacements == nil {
				info.Replacements = map[string]string{}
			}
			repl := d.Replace.Path
			if d.Replace.Version != "" {
				repl += "@" + d.Replace.Version
			}
			info.Replacements[d.Path] = repl
		}
		if IsCaddyModule(d.Path) {
			info.HasModuleInfo = true
			info.MainPath = d.Path
			info.MainVersion = d.Version
			info.MainSum = d.Sum
			// A local replacement has no checksum, so the replacement's
			// path and version are the only identity the lockfile can
			// record for it.
			if d.Replace != nil {
				info.MainSum = d.Replace.Sum
				info.MainReplace = d.Replace.Path
				info.MainReplaceVer = d.Replace.Version
			}
		}
	}

	cmd := exec.CommandContext(ctx, info.ResolvedPath, "version")
	if err := as.Apply(cmd); err != nil {
		return nil, err
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("running %s version: %w", path, err)
	}
	if f := strings.Fields(string(out)); len(f) > 0 {
		info.Version = f[0]
	}

	listOut, err := listModules(ctx, info.ResolvedPath, as)
	if err != nil {
		return nil, err
	}
	std, nonstd, unknown := ParseListModules(listOut)
	info.StandardCount = std
	// Build info is the source of truth for versions and checksums; the
	// version list-modules printed is only kept when build info has none.
	for i := range nonstd {
		if d, ok := deps[nonstd[i].Package]; ok {
			nonstd[i].Sum = d.Sum
			if d.Version != "" {
				nonstd[i].Version = d.Version
			}
			if d.Replace != nil {
				nonstd[i].Sum = d.Replace.Sum
				nonstd[i].ReplaceVersion = d.Replace.Version
				if nonstd[i].Replace == "" {
					nonstd[i].Replace = d.Replace.Path
				}
			}
		}
	}
	info.Plugins = nonstd
	info.UnknownModules = unknown
	owner, err := pkgmgr.Find(ctx, info.ResolvedPath)
	if err != nil {
		info.OwnerUnknown = err.Error()
	}
	info.Owner = owner
	return info, nil
}

func listModules(ctx context.Context, bin string, as *Account) (string, error) {
	cmd := exec.CommandContext(ctx, bin, "list-modules", "--packages", "--versions")
	if err := as.Apply(cmd); err != nil {
		return "", err
	}
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("running %s list-modules: %w: %s", bin, err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("running %s list-modules: %w", bin, err)
	}
	return string(out), nil
}

var sectionRe = regexp.MustCompile(`^\s*(Standard|Non-standard|Unknown) modules: (\d+)\s*$`)

// ParseListModules parses the text output of `caddy list-modules --packages
// --versions`. Lines are "id [version] [package [=> replace]] [[error]]",
// and each group is terminated by a "<Kind> modules: N" summary line.
func ParseListModules(out string) (standard int, nonstandard, unknown []Plugin) {
	var pending []Plugin
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if m := sectionRe.FindStringSubmatch(line); m != nil {
			switch m[1] {
			case "Standard":
				standard = len(pending)
				fmt.Sscan(m[2], &standard)
			case "Non-standard":
				nonstandard = pending
			case "Unknown":
				unknown = pending
			}
			pending = nil
			continue
		}
		pending = append(pending, parseModuleLine(line))
	}
	// Old Caddy versions that cannot resolve module info print bare IDs with
	// no summary lines at all; treat those as unknown.
	if len(pending) > 0 && nonstandard == nil && unknown == nil {
		unknown = pending
	}
	return standard, nonstandard, unknown
}

func parseModuleLine(line string) Plugin {
	var p Plugin
	line = strings.TrimSpace(line)
	// Caddy appends " [error]" after the fields; a line that is only an
	// error (no module ID) is not something Caddy prints, but is handled
	// rather than mistaken for a module called "[error".
	if i := strings.Index(line, " ["); i >= 0 {
		p.Error = strings.TrimSuffix(strings.TrimSpace(line[i+2:]), "]")
		line = line[:i]
	} else if strings.HasPrefix(line, "[") {
		p.Error = strings.TrimSuffix(line[1:], "]")
		line = ""
	}
	f := strings.Fields(line)
	if len(f) == 0 {
		return p
	}
	p.ModuleID = f[0]
	rest := f[1:]
	if len(rest) > 0 && strings.HasPrefix(rest[0], "v") && !strings.Contains(rest[0], "/") {
		p.Version = rest[0]
		rest = rest[1:]
	}
	if len(rest) > 0 {
		p.Package = rest[0]
		rest = rest[1:]
	}
	if len(rest) >= 2 && rest[0] == "=>" {
		p.Replace = rest[1]
	}
	return p
}
