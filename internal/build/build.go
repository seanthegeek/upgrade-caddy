// Package build implements the `build` command: compile a new Caddy that
// carries the same plugins as the installed one, at the same pinned
// versions, using the xcaddy library.
//
// The work is split in two. Resolve turns the installed binary plus the
// user's flags into a Plan with every version decided, and has no side
// effects beyond module proxy lookups, so it can be tested with a fake
// proxy. Plan.Build then runs xcaddy, verifies the result and writes a
// lockfile.
package build

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/xcaddy"
	"golang.org/x/mod/module"

	"github.com/seanthegeek/upgrade-caddy/internal/caddybin"
	"github.com/seanthegeek/upgrade-caddy/internal/goproxy"
	"github.com/seanthegeek/upgrade-caddy/internal/semver"
	"github.com/seanthegeek/upgrade-caddy/internal/systemd"
)

// Options controls a build.
type Options struct {
	Binary       string   // installed binary to reproduce ("" searches PATH)
	Fresh        bool     // ignore any installed binary; the plugin set is only what With gives
	Output       string   // where to write the new binary
	CaddyVersion string   // "" means latest within the installed major
	Upgrade      []string // plugins (Go module path or Caddy module ID) to bump to latest within their major
	UpgradeAll   bool     // bump every plugin
	With         []string // module[@version] to add, or to override the version of
	Replace      []string // old=new module replacements passed through to xcaddy
	DropReplace  []string // installed modules (Go module path or Caddy module ID) whose replacement is dropped, so they come from the module proxy
	AllowMajor   bool     // permit CaddyVersion or With to change a major version
	DryRun       bool     // resolve and print the plan, do not build
	Verbose      bool     // stream all build output instead of just step names
	Proxy        *goproxy.Client
	Log          io.Writer         // progress output; nil discards
	OnPlan       func(*Plan)       // called with the resolved plan before building, if set
	TimeoutBuild time.Duration     // xcaddy's timeout for the compile step; the overall limit is the context's
	RunAs        *caddybin.Account // account the finished binary is executed as when it is inspected; nil means the current user
}

// Source says how a plugin ended up in the plan.
type Source string

// Plugin sources.
const (
	Pinned     Source = "pinned"     // same version as the installed binary
	Upgraded   Source = "upgraded"   // bumped to latest within its major by --upgrade or --upgrade-all
	Added      Source = "added"      // new via --with
	Overridden Source = "overridden" // an installed plugin whose version or major was changed by --with
	Transitive Source = "transitive" // not planned; pulled in as a dependency of a planned plugin
)

// Plugin is one module the new binary will carry.
type Plugin struct {
	ModuleID  string   `json:"module_id,omitempty"`  // the first Caddy module ID the Go module registers
	ModuleIDs []string `json:"module_ids,omitempty"` // every Caddy module ID it registers; any of them names it to --upgrade and --drop-replace
	Package   string   `json:"package"`
	Version   string   `json:"version"`
	Installed string   `json:"installed,omitempty"` // version in the source binary, if it was there
	Source    Source   `json:"source"`
	Note      string   `json:"note,omitempty"`

	replacedBy string // replacement recorded in the source binary; must be covered by --replace
}

// Plan is a fully resolved build.
type Plan struct {
	SourcePath     string           `json:"source_path,omitempty"`
	CaddyInstalled string           `json:"caddy_installed,omitempty"`
	CaddyVersion   string           `json:"caddy_version"`
	Plugins        []Plugin         `json:"plugins"`
	Replacements   []xcaddy.Replace `json:"replacements,omitempty"`
	CaddyNote      string           `json:"caddy_note,omitempty"`  // for example, that an installed replacement of Caddy is dropped
	Notes          []string         `json:"notes,omitempty"`       // about modules that are neither Caddy nor a plugin, such as a dropped dependency replacement
	AllowMajor     bool             `json:"allow_major,omitempty"` // --allow-major was given, so verify lets a branch or commit resolve to another major
	Output         string           `json:"output"`
	HostGoVersion  string           `json:"host_go_version,omitempty"` // the go on PATH; the build may auto-fetch a newer one
}

// Result is what Build produced.
type Result struct {
	Output   string
	Lockfile string
	Built    *caddybin.Info
}

// Run inspects the installed binary (unless Fresh), resolves a plan, and
// builds it unless DryRun is set. Once resolved, the plan is returned even
// when a later step fails, so the caller can print it.
func Run(ctx context.Context, opts Options) (*Plan, *Result, error) {
	if _, err := exec.LookPath("go"); err != nil {
		return nil, nil, errors.New("the Go toolchain is required to build Caddy and no 'go' was found on PATH; see https://go.dev/dl/")
	}
	var src *caddybin.Info
	if !opts.Fresh {
		path, err := caddybin.Find(opts.Binary)
		if err != nil {
			return nil, nil, fmt.Errorf("%w (or pass --fresh to build without an installed binary)", err)
		}
		src, err = caddybin.Inspect(ctx, path)
		if err != nil {
			return nil, nil, err
		}
	}
	plan, err := Resolve(ctx, src, opts)
	if err != nil {
		return nil, nil, err
	}
	if out, err := exec.CommandContext(ctx, "go", "version").Output(); err == nil {
		plan.HostGoVersion = strings.TrimPrefix(strings.TrimSpace(string(out)), "go version ")
	}
	if err := refuseLive(ctx, plan.Output, systemd.UnitsUsing); err != nil {
		return plan, nil, err
	}
	if opts.OnPlan != nil {
		opts.OnPlan(plan)
	}
	if opts.DryRun {
		return plan, nil, nil
	}
	res, err := plan.Build(ctx, opts)
	return plan, res, err
}

// fullVersion refuses a semantic version that is not spelled out in full.
// To go get, "v2.11" is a prefix query for the highest v2.11.x (Go modules
// reference, "Version queries"), which a plan cannot pin: the build would
// come back as some v2.11.x and verify would reject it against the literal
// "v2.11". Branch names and commit hashes are not semantic versions and
// pass through for go get to resolve.
func fullVersion(flag, v string) error {
	if semver.IsValid(v) && !semver.IsFull(v) {
		return fmt.Errorf("%s %s: a version must be spelled out in full (for example %s); go would treat %s as \"the highest %s.x\", which cannot be pinned in a plan", flag, v, module.CanonicalVersion(v), v, v)
	}
	return nil
}

// refuseLive stops a build whose output path is run by a service: build
// never replaces a live binary, that is install's job. If systemd cannot be
// asked, the guarantee cannot be kept, and that stops the build too. Run
// checks before the build, and Build again right before the output is
// moved into place, because a unit can start using the path during a build
// that takes minutes.
func refuseLive(ctx context.Context, output string, unitsUsing func(context.Context, string) ([]systemd.Unit, error)) error {
	units, err := unitsUsing(ctx, output)
	if err != nil {
		return fmt.Errorf("cannot confirm that no service runs %s: %w", output, err)
	}
	if len(units) > 0 {
		return fmt.Errorf("%s is run by %s; 'build' never replaces a live binary, use 'install'", output, units[0].Name)
	}
	return nil
}

// Resolve decides every version in the build from the source binary (nil
// when building fresh) and the options. It talks only to the module proxy.
func Resolve(ctx context.Context, src *caddybin.Info, opts Options) (*Plan, error) {
	proxy := opts.Proxy
	if proxy == nil {
		proxy = goproxy.New()
	}
	p := &Plan{}

	if src != nil {
		if src.IsDistroBuild() {
			return nil, fmt.Errorf("%s carries no Go module information (typical of distribution packages), so its plugin set cannot be reproduced; "+
				"pass --fresh, with --with for each plugin you want, to build from scratch", src.Path)
		}
		p.SourcePath = src.Path
		p.CaddyInstalled = src.MainVersion
	}

	// Caddy itself. Stay on the installed major unless told otherwise.
	caddyBase, _ := goproxy.SplitMajor(caddybin.CaddyModulePath)
	installedMajor := 2
	if m := semver.Major(p.CaddyInstalled); m > 0 {
		installedMajor = m
	}
	if opts.CaddyVersion == "" {
		info, err := proxy.Latest(ctx, goproxy.MajorPath(caddyBase, installedMajor))
		if err != nil {
			return nil, fmt.Errorf("looking up latest Caddy: %w", err)
		}
		p.CaddyVersion = info.Version
	} else {
		// A semantic version is canonicalised ("2.11.6" becomes "v2.11.6");
		// a branch name or commit hash is passed to xcaddy as given, since
		// prefixing it with "v" would name a ref that does not exist.
		v := opts.CaddyVersion
		if semver.IsValid(v) {
			v = semver.Canonical(v)
		}
		if err := fullVersion("--caddy-version", v); err != nil {
			return nil, err
		}
		// Major 0 is a major like any other: v0.x would make xcaddy build
		// from the bare Caddy module path.
		if m := semver.Major(v); m >= 0 && m != installedMajor && !opts.AllowMajor {
			than := fmt.Sprintf("the installed v%d", installedMajor)
			if src == nil {
				than = fmt.Sprintf("the v%d this tool builds by default", installedMajor)
			}
			return nil, fmt.Errorf("--caddy-version %s is a different major version than %s; plugins built for one major do not compile against another, pass --allow-major to do this deliberately", v, than)
		}
		p.CaddyVersion = v
	}

	// Start from the installed plugin set, pinned. Modules Caddy listed
	// without package metadata cannot be pinned, and dropping them would
	// be exactly the loss this tool exists to prevent, so refuse.
	if src != nil && len(src.UnknownModules) > 0 {
		ids := make([]string, len(src.UnknownModules))
		for i, u := range src.UnknownModules {
			ids[i] = u.ModuleID
		}
		return nil, fmt.Errorf("%s reports %d module(s) without package information (%s), so its plugin set cannot be reproduced; "+
			"pass --fresh with --with for each plugin to build from scratch", src.Path, len(ids), strings.Join(ids, ", "))
	}
	index := map[string]int{} // package path -> position in p.Plugins
	if src != nil {
		for _, ip := range src.Plugins {
			if ip.Error != "" {
				return nil, fmt.Errorf("plugin %s in %s reported an error: %s", ip.ModuleID, src.Path, ip.Error)
			}
			if ip.Package == "" || ip.Version == "" {
				return nil, fmt.Errorf("plugin %s in %s has no module path or version in build info; cannot pin it", ip.ModuleID, src.Path)
			}
			if i, dup := index[ip.Package]; dup {
				// Several Caddy modules from one Go module: pinned once,
				// but every ID still names it.
				p.Plugins[i].ModuleIDs = append(p.Plugins[i].ModuleIDs, ip.ModuleID)
				continue
			}
			index[ip.Package] = len(p.Plugins)
			p.Plugins = append(p.Plugins, Plugin{
				ModuleID: ip.ModuleID, ModuleIDs: []string{ip.ModuleID}, Package: ip.Package, Version: ip.Version,
				Installed: ip.Version, Source: Pinned, replacedBy: ip.Replace,
			})
		}
	}

	// Upgrades: latest within the plugin's own major.
	var toUpgrade []int
	if opts.UpgradeAll {
		for i := range p.Plugins {
			toUpgrade = append(toUpgrade, i)
		}
	}
	for _, name := range opts.Upgrade {
		i, ok := find(p.Plugins, name)
		if !ok {
			return nil, fmt.Errorf("--upgrade %s: not in the installed plugin set; use --with to add a plugin", name)
		}
		toUpgrade = append(toUpgrade, i)
	}
	sort.Ints(toUpgrade)
	for n, i := range toUpgrade {
		if n > 0 && toUpgrade[n-1] == i {
			continue
		}
		pl := &p.Plugins[i]
		info, err := proxy.Latest(ctx, pl.Package)
		if err != nil {
			return nil, fmt.Errorf("--upgrade %s: %w", pl.Package, err)
		}
		switch {
		case semver.Compare(info.Version, pl.Installed) <= 0:
			pl.Note = "already at latest"
		case crossesMajor(pl.Installed, info.Version) && !opts.AllowMajor:
			// v0 and v1 share a bare module path, so "latest" can be a
			// different major without the path changing.
			pl.Note = fmt.Sprintf("latest %s is a new major on the same path; pass --allow-major to take it", info.Version)
		default:
			pl.Version = info.Version
			pl.Source = Upgraded
		}
		if majors, err := proxy.NewerMajors(ctx, pl.Package, pl.Installed); err == nil && len(majors) > 0 {
			m := majors[len(majors)-1]
			pl.Note = join(pl.Note, fmt.Sprintf("newer major %s at %s; use --with %s@%s --allow-major to move to it", m.Version, m.Path, m.Path, m.Version))
		}
	}

	// Additions and overrides.
	for _, spec := range opts.With {
		path, version, _ := strings.Cut(spec, "@")
		path = strings.TrimSpace(path)
		if path == "" {
			return nil, fmt.Errorf("--with %q: empty module path", spec)
		}
		// A semantic version is passed to go get in its canonical form
		// ("1.2.3" becomes "v1.2.3"); branch names and commit hashes are
		// left for go get to resolve.
		if version != "" && semver.IsValid(version) {
			version = semver.Canonical(version)
			if err := fullVersion("--with "+path, version); err != nil {
				return nil, err
			}
		}
		if version == "" {
			info, err := proxy.Latest(ctx, path)
			if err != nil {
				return nil, fmt.Errorf("--with %s: %w", path, err)
			}
			version = info.Version
		}
		if i, ok := index[path]; ok {
			pl := &p.Plugins[i]
			if crossesMajor(pl.Installed, version) && !opts.AllowMajor {
				return nil, fmt.Errorf("--with %s would move %s from v%d to v%d on the same path; pass --allow-major to do this deliberately", spec, pl.Package, semver.Major(pl.Installed), semver.Major(version))
			}
			pl.Version = version
			if version != pl.Installed {
				pl.Source = Overridden
			}
			continue
		}
		base, major := goproxy.SplitMajor(path)
		if i, ok := sameBase(p.Plugins, base, path); ok {
			pl := &p.Plugins[i]
			// The installed version says what major the plugin is at, not
			// its path: a bare path holds v0, v1 and vN+incompatible alike,
			// so moving vN+incompatible to the /vN path is the same major.
			oldMajor := semver.Major(pl.Installed)
			if oldMajor < 0 {
				oldMajor = majorOfPath(pl.Package)
			}
			if oldMajor != major && !opts.AllowMajor {
				return nil, fmt.Errorf("--with %s would move %s from major v%d to v%d; pass --allow-major to do this deliberately", spec, pl.Package, oldMajor, major)
			}
			delete(index, pl.Package)
			if oldMajor == major {
				pl.Note = join(pl.Note, "module path change from "+pl.Package+"@"+pl.Installed+" within the same major")
			} else {
				pl.Note = join(pl.Note, "major version change from "+pl.Package+"@"+pl.Installed)
			}
			if pl.replacedBy != "" {
				// The user named a new module path, which is a new source;
				// the old path's replacement cannot apply to it.
				pl.Note = join(pl.Note, "the replacement of "+pl.Package+" by "+pl.replacedBy+" does not carry over")
				pl.replacedBy = ""
			}
			pl.Package, pl.Version, pl.Source = path, version, Overridden
			index[path] = i
			continue
		}
		index[path] = len(p.Plugins)
		p.Plugins = append(p.Plugins, Plugin{Package: path, Version: version, Source: Added})
	}

	// Replacements, and installed replacements that must be covered: kept
	// with --replace or dropped with --drop-replace, whatever happens to the
	// module's version. --upgrade and --with choose a version, not a
	// source, so neither drops a replacement on its own; a fork or local
	// checkout would otherwise be swapped for the module proxy's code
	// without anyone having asked for that.
	covered := map[string]bool{}
	for _, r := range opts.Replace {
		old, repl, ok := strings.Cut(r, "=")
		if !ok || old == "" || repl == "" {
			return nil, fmt.Errorf("--replace %q: want old=new", r)
		}
		covered[old] = true
		p.Replacements = append(p.Replacements, xcaddy.NewReplace(old, repl))
	}
	dropped := map[string]bool{}
	for _, name := range opts.DropReplace {
		if src == nil {
			return nil, fmt.Errorf("--drop-replace %s: there is no installed binary to drop a replacement from", name)
		}
		var pkg string
		if i, ok := find(p.Plugins, name); ok && p.Plugins[i].replacedBy != "" {
			pkg = p.Plugins[i].Package
			p.Plugins[i].Note = join(p.Plugins[i].Note, "replacement by "+p.Plugins[i].replacedBy+" dropped; built from the module proxy")
		} else if name == src.MainPath && src.MainReplace != "" {
			pkg = src.MainPath
			p.CaddyNote = "replacement by " + replacedBy(src.MainReplace, src.MainReplaceVer) + " dropped; built from the module proxy"
		} else if repl, ok := src.Replacements[name]; ok {
			// A dependency that registers no Caddy module.
			pkg = name
			p.Notes = append(p.Notes, name+": replacement by "+repl+" dropped; built from the module proxy")
		} else {
			return nil, fmt.Errorf("--drop-replace %s: %s was not built with a replacement for it", name, src.Path)
		}
		if covered[pkg] {
			return nil, fmt.Errorf("--drop-replace %s contradicts --replace %s=...", name, pkg)
		}
		dropped[pkg] = true
	}
	for _, pl := range p.Plugins {
		if pl.replacedBy != "" && !covered[pl.Package] && !dropped[pl.Package] {
			return nil, fmt.Errorf("%s was built with a module replacement (=> %s), which cannot be reproduced automatically; pass --replace %s=<path or module@version> to keep one, or --drop-replace %s to build it from the module proxy instead", pl.Package, pl.replacedBy, pl.Package, pl.Package)
		}
	}
	// Caddy itself can be replaced too (a fork, a local checkout), and a
	// default rebuild would otherwise quietly swap it for upstream Caddy.
	if src != nil && src.MainReplace != "" && !covered[src.MainPath] && !dropped[src.MainPath] {
		return nil, fmt.Errorf("%s was built with Caddy itself replaced (=> %s), which cannot be reproduced automatically; pass --replace %s=<path or module@version> to keep one, --drop-replace %s to build upstream Caddy instead, or --fresh to build without the installed binary", src.Path, replacedBy(src.MainReplace, src.MainReplaceVer), src.MainPath, src.MainPath)
	}
	// And so can any other dependency, one that registers no Caddy module
	// (a patched library, say): build info records every replace directive,
	// and a rebuild without it would compile different code under the same
	// module versions.
	if src != nil {
		for _, dep := range sortedKeys(src.Replacements) {
			if dep == src.MainPath || covered[dep] || dropped[dep] {
				continue
			}
			if _, isPlugin := index[dep]; isPlugin {
				continue // handled above, by the plugin's own replacedBy
			}
			return nil, fmt.Errorf("%s was built with its dependency %s replaced (=> %s), which cannot be reproduced automatically; pass --replace %s=<path or module@version> to keep one, or --drop-replace %s to build it from the module proxy instead", src.Path, dep, src.Replacements[dep], dep, dep)
		}
	}

	p.AllowMajor = opts.AllowMajor

	// Output path. Never the binary we are reproducing.
	out := opts.Output
	if out == "" {
		out = "caddy"
	}
	abs, err := filepath.Abs(out)
	if err != nil {
		return nil, err
	}
	p.Output = abs
	if src != nil {
		if r, err := filepath.EvalSymlinks(abs); err == nil && r == src.ResolvedPath {
			return nil, fmt.Errorf("output %s is the installed binary; 'build' never overwrites it, use 'install'", abs)
		}
	}
	return p, nil
}

// crossesMajor reports whether two versions of a module on one path have
// different semantic majors, which on a bare path happens between v0 and
// v1. Versions that are not semantic (branches, hashes) never cross.
func crossesMajor(installed, candidate string) bool {
	a, b := semver.Major(installed), semver.Major(candidate)
	return a >= 0 && b >= 0 && a != b
}

// find names a plugin by its Go module path or by any Caddy module ID it
// registers.
func find(plugins []Plugin, name string) (int, bool) {
	for i, pl := range plugins {
		if pl.Package == name || pl.ModuleID == name || slices.Contains(pl.ModuleIDs, name) {
			return i, true
		}
	}
	return 0, false
}

// sameBase finds a plugin on the same module path but a different major.
func sameBase(plugins []Plugin, base, path string) (int, bool) {
	for i, pl := range plugins {
		b, _ := goproxy.SplitMajor(pl.Package)
		if b == base && pl.Package != path {
			return i, true
		}
	}
	return 0, false
}

func majorOfPath(path string) int {
	_, m := goproxy.SplitMajor(path)
	return m
}

func join(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// WriteText prints the plan for a human.
func (p *Plan) WriteText(w io.Writer) {
	if p.SourcePath != "" {
		fmt.Fprintf(w, "Source:   %s (%s)\n", p.SourcePath, p.CaddyInstalled)
	} else {
		fmt.Fprintln(w, "Source:   none (fresh build)")
	}
	if p.CaddyInstalled != "" && p.CaddyInstalled != p.CaddyVersion {
		fmt.Fprintf(w, "Caddy:    %s -> %s\n", p.CaddyInstalled, p.CaddyVersion)
	} else {
		fmt.Fprintf(w, "Caddy:    %s\n", p.CaddyVersion)
	}
	if p.CaddyNote != "" {
		fmt.Fprintf(w, "          %s\n", p.CaddyNote)
	}
	for _, n := range p.Notes {
		fmt.Fprintf(w, "Note:     %s\n", n)
	}
	if p.HostGoVersion != "" {
		fmt.Fprintf(w, "Host Go:  %s (with GOTOOLCHAIN=auto a newer toolchain is fetched if Caddy requires it)\n", p.HostGoVersion)
	}
	fmt.Fprintf(w, "Output:   %s\n", p.Output)
	if len(p.Plugins) == 0 {
		fmt.Fprintln(w, "Plugins:  none")
	} else {
		fmt.Fprintln(w, "Plugins:")
		for _, pl := range p.Plugins {
			ver := pl.Version
			if pl.Installed != "" && pl.Installed != pl.Version {
				ver = pl.Installed + " -> " + pl.Version
			}
			line := fmt.Sprintf("  %-40s %s (%s)", pl.Package, ver, pl.Source)
			if ids := pl.ModuleIDs; len(ids) > 0 {
				line = fmt.Sprintf("  %-40s %s (%s, %s)", pl.Package, ver, pl.Source, strings.Join(ids, ", "))
			} else if pl.ModuleID != "" {
				line = fmt.Sprintf("  %-40s %s (%s, %s)", pl.Package, ver, pl.Source, pl.ModuleID)
			}
			if pl.Note != "" {
				line += "\n      " + pl.Note
			}
			fmt.Fprintln(w, line)
		}
	}
	for _, r := range p.Replacements {
		fmt.Fprintf(w, "Replace:  %s => %s\n", r.Old, r.New)
	}
}

// Build runs xcaddy for the plan, verifies the produced binary carries what
// the plan says, moves it into place and writes a lockfile next to it.
func (p *Plan) Build(ctx context.Context, opts Options) (*Result, error) {
	logw := opts.Log
	if logw == nil {
		logw = io.Discard
	}
	dir := filepath.Dir(p.Output)
	tmp, err := os.CreateTemp(dir, ".caddy-build-*")
	if err != nil {
		return nil, fmt.Errorf("creating build output in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath) // no-op once renamed into place
	// The lockfile is created exclusively now and written through this
	// handle later, so nobody can plant a symlink at its path during the
	// build and have a privileged build write through it.
	lockf, err := os.CreateTemp(dir, ".caddy-build-*.lock.json")
	if err != nil {
		return nil, fmt.Errorf("creating lockfile in %s: %w", dir, err)
	}
	tmpLock := lockf.Name()
	defer os.Remove(tmpLock) // no-op once renamed into place

	deps := make([]xcaddy.Dependency, 0, len(p.Plugins))
	for _, pl := range p.Plugins {
		deps = append(deps, xcaddy.Dependency{PackagePath: pl.Package, Version: pl.Version})
	}
	steps := &stepLog{w: logw, verbose: opts.Verbose}
	b := xcaddy.Builder{
		CaddyVersion: p.CaddyVersion,
		Plugins:      deps,
		Replacements: p.Replacements,
		TimeoutBuild: opts.TimeoutBuild,
		OnStep:       steps.onStep,
	}
	if err := b.Build(ctx, tmpPath); err != nil {
		if !opts.Verbose {
			steps.dumpFailed()
		}
		hint := ""
		if steps.failedStep() == xcaddy.StepCompile || steps.failedStep() == xcaddy.StepPinVersions {
			hint = " (if a plugin failed against the newer Caddy, try --upgrade <plugin>)"
		}
		return nil, fmt.Errorf("xcaddy: %w%s", err, hint)
	}

	// Executable before it is inspected, since the inspection may run it
	// as another account (see Options.RunAs).
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		lockf.Close()
		return nil, err
	}
	built, err := caddybin.InspectAs(ctx, tmpPath, opts.RunAs)
	if err != nil {
		lockf.Close()
		return nil, fmt.Errorf("inspecting the new binary: %w", err)
	}
	if err := p.verify(built); err != nil {
		lockf.Close()
		return nil, fmt.Errorf("the new binary does not match the plan: %w", err)
	}
	if extra := p.Unplanned(built); len(extra) > 0 {
		fmt.Fprintf(logw, "==> also compiled in, pulled in by the plugins above: %s\n", strings.Join(extra, ", "))
	}
	// Write the lockfile through the reserved handle first, so a lockfile
	// failure leaves the requested output untouched; then move both.
	built.Path, built.ResolvedPath = p.Output, p.Output
	if err := writeLockfile(lockf, p, built); err != nil {
		return nil, err
	}
	// The check Run made before the build, repeated now that the build is
	// done: a unit may have started using the output path since.
	if err := refuseLive(ctx, p.Output, systemd.UnitsUsing); err != nil {
		return nil, err
	}
	lockPath := p.Output + ".lock.json"
	if err := commitOutput(tmpPath, tmpLock, p.Output); err != nil {
		return nil, err
	}
	return &Result{Output: p.Output, Lockfile: lockPath, Built: built}, nil
}

// commitOutput moves the built binary and its lockfile into place as a
// pair. Whatever was at the output path before (an earlier build and its
// lockfile) is moved aside first and put back if either rename fails, so
// the output never holds a binary beside another build's lockfile, and a
// failed commit leaves what was there before. The output is never a live
// binary (refuseLive), so moving it aside is safe.
func commitOutput(tmpBin, tmpLock, output string) (err error) {
	lockPath := output + ".lock.json"
	oldBin, err := moveAside(output)
	if err != nil {
		return fmt.Errorf("moving the previous output aside: %w", err)
	}
	oldLock, err := moveAside(lockPath)
	if err != nil {
		return restored(fmt.Errorf("moving the previous lockfile aside: %w", err), oldBin, output, "", lockPath)
	}
	if err := os.Rename(tmpBin, output); err != nil {
		return restored(fmt.Errorf("moving the new binary into place: %w", err), oldBin, output, oldLock, lockPath)
	}
	if err := os.Rename(tmpLock, lockPath); err != nil {
		// Undo the binary too, so the pair at the output stays a pair.
		if rmErr := os.Remove(output); rmErr != nil {
			return errors.Join(fmt.Errorf("moving the new lockfile into place: %w; and the new binary could not be removed from %s again, so it sits there without its lockfile; %s", err, output, asideNote(oldBin, oldLock)), rmErr)
		}
		return restored(fmt.Errorf("moving the new lockfile into place: %w", err), oldBin, output, oldLock, lockPath)
	}
	if oldBin != "" {
		os.Remove(oldBin)
	}
	if oldLock != "" {
		os.Remove(oldLock)
	}
	return nil
}

// moveAside renames an existing regular file at path to an unpredictable
// name in its directory and returns that name, or "" when nothing is
// there. Anything else at the path (a directory, say) is an error, found
// before the output is touched.
func moveAside(path string) (string, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s exists and is not a regular file", path)
	}
	aside, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-previous-*")
	if err != nil {
		return "", err
	}
	aside.Close()
	if err := os.Rename(path, aside.Name()); err != nil {
		os.Remove(aside.Name())
		return "", err
	}
	return aside.Name(), nil
}

// asideNote says where the previous output pair is while it is moved
// aside, for an error in which it could not be put back automatically.
func asideNote(oldBin, oldLock string) string {
	switch {
	case oldBin == "" && oldLock == "":
		return "nothing was at the output before"
	case oldLock == "":
		return "the previous binary is at " + oldBin + " (it had no lockfile)"
	case oldBin == "":
		return "the previous lockfile is at " + oldLock + " (there was no previous binary)"
	}
	return "the previous binary is at " + oldBin + " and its lockfile at " + oldLock
}

// restored puts the previous output pair back after a failed commit and
// words the error from what actually happened: "the previous output was
// restored" only when both files came back, otherwise where each one is.
func restored(err error, oldBin, output, oldLock, lockPath string) error {
	binErr := putBack(oldBin, output)
	lockErr := putBack(oldLock, lockPath)
	if binErr == nil && lockErr == nil {
		if oldBin == "" && oldLock == "" {
			return fmt.Errorf("%w; nothing was at %s before and nothing is there now", err, output)
		}
		return fmt.Errorf("%w; the previous output was restored", err)
	}
	return errors.Join(fmt.Errorf("%w; and the previous output could not be fully restored, see below", err), binErr, lockErr)
}

// putBack restores a file moved aside by moveAside; a "" means nothing was
// there. A failure says where the file is, so nothing is lost silently.
func putBack(aside, path string) error {
	if aside == "" {
		return nil
	}
	if err := os.Rename(aside, path); err != nil {
		return fmt.Errorf("the previous %s is at %s: %w", filepath.Base(path), aside, err)
	}
	return nil
}

// Unplanned lists non-standard Go modules compiled into the binary that the
// plan did not name. A requested plugin can depend on another module that
// registers Caddy modules of its own, so these are expected rather than an
// error; they are reported and recorded in the lockfile as "transitive".
func (p *Plan) Unplanned(built *caddybin.Info) []string {
	planned := map[string]bool{}
	for _, pl := range p.Plugins {
		planned[pl.Package] = true
	}
	var extra []string
	seen := map[string]bool{}
	for _, bp := range built.Plugins {
		if !planned[bp.Package] && !seen[bp.Package] {
			seen[bp.Package] = true
			extra = append(extra, bp.Package+"@"+bp.Version)
		}
	}
	return extra
}

// verify checks the built binary against the plan: right Caddy version, and
// every planned plugin present at the planned version. Versions that are not
// semantic versions (branch names, commit hashes) are resolved by go get,
// so for those only presence is checked.
func (p *Plan) verify(built *caddybin.Info) error {
	if !built.HasModuleInfo {
		return errors.New("no Go module information in the output")
	}
	if semver.Major(p.CaddyVersion) >= 0 && built.MainVersion != p.CaddyVersion {
		return fmt.Errorf("caddy is %s, wanted %s", built.MainVersion, p.CaddyVersion)
	}
	have := map[string]string{}
	for _, bp := range built.Plugins {
		have[bp.Package] = bp.Version
	}
	for _, pl := range p.Plugins {
		got, ok := have[pl.Package]
		if !ok {
			return fmt.Errorf("plugin %s is missing from the output", pl.Package)
		}
		if semver.Major(pl.Version) >= 0 && got != pl.Version {
			return fmt.Errorf("plugin %s is %s, wanted %s", pl.Package, got, pl.Version)
		}
		// A branch or commit is resolved by go get, and on a bare module
		// path (v0, v1, +incompatible) it can resolve to another major,
		// which Resolve could not see. The same --allow-major rule applies
		// to the resolved version.
		if semver.Major(pl.Version) < 0 && pl.Installed != "" && !p.AllowMajor && crossesMajor(pl.Installed, got) {
			return fmt.Errorf("plugin %s: %s resolved to %s, a different major from the installed %s; pass --allow-major to do this deliberately", pl.Package, pl.Version, got, pl.Installed)
		}
	}
	// Replacements: every one requested must be in effect, and nothing may
	// be replaced that was not requested. Only this build's go.mod can
	// introduce a replacement (Go ignores replace directives in
	// dependencies), so the two lists must agree exactly, across every
	// module in the output: a --replace for a module that registers no
	// Caddy module, or for a mistyped path, is checked like any other.
	want := map[string]string{}
	for _, r := range p.Replacements {
		want[string(r.Old)] = string(r.New)
	}
	checked := map[string]bool{}
	check := func(pkg, got string) error {
		checked[pkg] = true
		return replacementMatches(pkg, want[pkg], got)
	}
	if err := check(built.MainPath, replacedBy(built.MainReplace, built.MainReplaceVer)); err != nil {
		return err
	}
	for _, bp := range built.Plugins {
		if err := check(bp.Package, replacedBy(bp.Replace, bp.ReplaceVersion)); err != nil {
			return err
		}
	}
	for _, path := range sortedKeys(built.Replacements) {
		if !checked[path] {
			if err := check(path, built.Replacements[path]); err != nil {
				return err
			}
		}
	}
	for _, old := range sortedKeys(want) {
		if !checked[old] {
			return fmt.Errorf("%s is not replaced in the output; --replace %s=%s did not take effect (no module by that path is in this build; check the spelling)", old, old, want[old])
		}
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// replacementMatches compares the replacement asked for (as passed to
// --replace) with the one the output's build info records. A requested
// version that is not a semantic version (a branch or commit) is resolved
// by go get, so for those only the path is compared.
func replacementMatches(pkg, want, got string) error {
	if want == got {
		return nil
	}
	wantPath, wantVer, _ := strings.Cut(want, "@")
	gotPath, gotVer, _ := strings.Cut(got, "@")
	if want != "" && got != "" && wantPath == gotPath && semver.Major(wantVer) < 0 && gotVer != "" {
		return nil
	}
	if want == "" {
		return fmt.Errorf("%s is replaced by %s in the output, which was not asked for", pkg, got)
	}
	if got == "" {
		return fmt.Errorf("%s is not replaced in the output; --replace %s=%s did not take effect", pkg, pkg, want)
	}
	return fmt.Errorf("%s is replaced by %s in the output, wanted %s", pkg, got, want)
}

// Lockfile records exactly what went into a build, with checksums, so an
// installed binary can be traced to its inputs. install reads it back: a
// --from binary is accepted only when the lockfile beside it describes that
// exact binary (Describes), and the lockfile is then installed beside the
// target as its attestation.
type Lockfile struct {
	Schema       int               `json:"schema"`
	BuiltAt      time.Time         `json:"built_at"`
	GoVersion    string            `json:"go_version"` // the toolchain that compiled the binary
	Caddy        LockModule        `json:"caddy"`
	Plugins      []LockModule      `json:"plugins"`
	Replacements map[string]string `json:"replacements,omitempty"` // every replace directive in the build, module path -> replacement, dependencies that register no Caddy module included
	SourceBinary *LockSource       `json:"source_binary,omitempty"`
}

// ReadLockfile parses a lockfile from disk.
func ReadLockfile(path string) (*Lockfile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var lf Lockfile
	if err := json.Unmarshal(data, &lf); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if lf.Schema != 1 {
		return nil, fmt.Errorf("%s: unsupported lockfile schema %d", path, lf.Schema)
	}
	return &lf, nil
}

// Describes reports whether the lockfile matches the inspected binary:
// same Caddy path, version, checksum and replacement, and the same list of
// plugin registrations (module ID, package, version, checksum and
// replacement), each present the same number of times. Everything must be
// exactly equal, empty included: a lockfile is written from the same
// build info Inspect reads, so a genuine one never differs from its
// binary, and a missing or surplus field means the file was edited or
// belongs to another binary. The replacement matters because a module
// replaced by a local directory has no checksum at all, so the directory
// is the only thing telling two such builds apart. A lockfile that does
// not describe the binary beside it is refused by install.
func (lf *Lockfile) Describes(built *caddybin.Info) error {
	if lf.Caddy.Package != built.MainPath || lf.Caddy.Version != built.MainVersion {
		return fmt.Errorf("lockfile says Caddy %s %s, binary is %s %s", lf.Caddy.Package, lf.Caddy.Version, built.MainPath, built.MainVersion)
	}
	if lf.Caddy.Sum != built.MainSum {
		return fmt.Errorf("lockfile Caddy checksum %q does not match the binary's %q", lf.Caddy.Sum, built.MainSum)
	}
	if r := replacedBy(built.MainReplace, built.MainReplaceVer); lf.Caddy.ReplacedBy != r {
		return fmt.Errorf("lockfile Caddy replacement %q does not match the binary's %q", lf.Caddy.ReplacedBy, r)
	}
	key := func(id, pkg, ver, sum, repl string) string {
		k := id + " " + pkg + "@" + ver + " " + sum
		if repl != "" {
			k += " => " + repl
		}
		return k
	}
	want := map[string]int{}
	for _, m := range lf.Plugins {
		want[key(m.ModuleID, m.Package, m.Version, m.Sum, m.ReplacedBy)]++
	}
	have := map[string]int{}
	for _, p := range built.Plugins {
		have[key(p.ModuleID, p.Package, p.Version, p.Sum, replacedBy(p.Replace, p.ReplaceVersion))]++
	}
	for k, n := range want {
		if have[k] != n {
			return fmt.Errorf("lockfile lists %s %d time(s), the binary carries it %d time(s)", k, n, have[k])
		}
	}
	for k, n := range have {
		if want[k] != n {
			return fmt.Errorf("binary carries %s %d time(s), the lockfile lists it %d time(s)", k, n, want[k])
		}
	}
	// Every replace directive, those of plain dependencies included: a
	// replaced library changes the compiled code under the same module
	// versions, so two binaries differing only there are different builds.
	if !maps.Equal(lf.Replacements, built.Replacements) && (len(lf.Replacements) > 0 || len(built.Replacements) > 0) {
		return fmt.Errorf("lockfile replacements %v do not match the binary's %v", lf.Replacements, built.Replacements)
	}
	return nil
}

// replacedBy renders a replace directive's target as "path@version", or
// just the path for a local directory, which has no version.
func replacedBy(path, version string) string {
	if path == "" {
		return ""
	}
	if version != "" {
		return path + "@" + version
	}
	return path
}

// LockModule is one module in a Lockfile.
type LockModule struct {
	ModuleID   string `json:"module_id,omitempty"`
	Package    string `json:"package"`
	Version    string `json:"version"`
	Sum        string `json:"sum,omitempty"`
	ReplacedBy string `json:"replaced_by,omitempty"` // "path@version" of a replace directive, whose checksum Sum then is
	Source     Source `json:"source,omitempty"`
}

// LockSource is the binary a build reproduced.
type LockSource struct {
	Path    string `json:"path"`
	Version string `json:"version"`
}

// writeLockfile writes the lockfile through an already-open, exclusively
// created file and closes it. Close can report a write failure of its own,
// so its error is returned too.
func writeLockfile(f *os.File, p *Plan, built *caddybin.Info) (err error) {
	defer func() { err = errors.Join(err, f.Close()) }()
	lf := Lockfile{
		Schema:    1,
		BuiltAt:   time.Now().UTC().Truncate(time.Second),
		GoVersion: built.GoVersion,
		Caddy:     LockModule{Package: built.MainPath, Version: built.MainVersion, Sum: built.MainSum, ReplacedBy: replacedBy(built.MainReplace, built.MainReplaceVer)},
		Plugins:   []LockModule{},
	}
	if len(built.Replacements) > 0 {
		lf.Replacements = maps.Clone(built.Replacements)
	}
	source := map[string]Source{}
	for _, pl := range p.Plugins {
		source[pl.Package] = pl.Source
	}
	for _, bp := range built.Plugins {
		src, ok := source[bp.Package]
		if !ok {
			src = Transitive
		}
		lm := LockModule{ModuleID: bp.ModuleID, Package: bp.Package, Version: bp.Version, Sum: bp.Sum, Source: src,
			ReplacedBy: replacedBy(bp.Replace, bp.ReplaceVersion)}
		lf.Plugins = append(lf.Plugins, lm)
	}
	if p.SourcePath != "" {
		lf.SourceBinary = &LockSource{Path: p.SourcePath, Version: p.CaddyInstalled}
	}
	data, err := json.MarshalIndent(lf, "", "  ")
	if err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := f.Chmod(0o644); err != nil {
		return err
	}
	return f.Sync()
}

// stepLog prints xcaddy's progress. In verbose mode every line is written
// as it arrives; otherwise only step names are, and each step's output is
// kept so the failed step's can be shown on error.
type stepLog struct {
	w       io.Writer
	verbose bool

	mu    sync.Mutex
	order []xcaddy.Step
	lines map[xcaddy.Step][]string
}

var stepNames = map[xcaddy.Step]string{
	xcaddy.StepCreateEnvironment: "creating build environment",
	xcaddy.StepInitializeModule:  "initializing Go module",
	xcaddy.StepPinVersions:       "downloading Caddy and plugins at pinned versions",
	xcaddy.StepWindowsResources:  "generating Windows resources",
	xcaddy.StepTidyModule:        "tidying Go module",
	xcaddy.StepCompile:           "compiling",
	xcaddy.StepCleanup:           "cleaning up",
}

func (l *stepLog) onStep(e *xcaddy.StepEvent) error {
	name := stepNames[e.Step]
	if name == "" {
		name = string(e.Step)
	}
	fmt.Fprintf(l.w, "==> %s\n", name)
	l.mu.Lock()
	if l.lines == nil {
		l.lines = map[xcaddy.Step][]string{}
	}
	l.order = append(l.order, e.Step)
	l.mu.Unlock()

	sc := bufio.NewScanner(e.Output)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if l.verbose {
			fmt.Fprintf(l.w, "    %s\n", line)
			continue
		}
		l.mu.Lock()
		l.lines[e.Step] = append(l.lines[e.Step], line)
		l.mu.Unlock()
	}
	return sc.Err()
}

// failedStep is the last step that ran before cleanup.
func (l *stepLog) failedStep() xcaddy.Step {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.order) - 1; i >= 0; i-- {
		if l.order[i] != xcaddy.StepCleanup {
			return l.order[i]
		}
	}
	return ""
}

func (l *stepLog) dumpFailed() {
	step := l.failedStep()
	l.mu.Lock()
	lines := l.lines[step]
	l.mu.Unlock()
	if len(lines) == 0 {
		return
	}
	fmt.Fprintf(l.w, "--- output of step %q ---\n", stepNames[step])
	for _, line := range lines {
		fmt.Fprintf(l.w, "    %s\n", line)
	}
	fmt.Fprintln(l.w, "---")
}
