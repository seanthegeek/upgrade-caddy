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
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/xcaddy"

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
	AllowMajor   bool     // permit CaddyVersion or With to change a major version
	DryRun       bool     // resolve and print the plan, do not build
	Verbose      bool     // stream all build output instead of just step names
	Proxy        *goproxy.Client
	Log          io.Writer     // progress output; nil discards
	OnPlan       func(*Plan)   // called with the resolved plan before building, if set
	TimeoutBuild time.Duration // xcaddy's timeout for the compile step; the overall limit is the context's
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
	ModuleID  string `json:"module_id,omitempty"`
	Package   string `json:"package"`
	Version   string `json:"version"`
	Installed string `json:"installed,omitempty"` // version in the source binary, if it was there
	Source    Source `json:"source"`
	Note      string `json:"note,omitempty"`

	replacedBy string // replacement recorded in the source binary; must be covered by --replace
}

// Plan is a fully resolved build.
type Plan struct {
	SourcePath     string           `json:"source_path,omitempty"`
	CaddyInstalled string           `json:"caddy_installed,omitempty"`
	CaddyVersion   string           `json:"caddy_version"`
	Plugins        []Plugin         `json:"plugins"`
	Replacements   []xcaddy.Replace `json:"replacements,omitempty"`
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
	// build never replaces a binary a service is running; that is install's
	// job. If systemd cannot be asked, the guarantee cannot be kept, so stop.
	units, err := systemd.UnitsUsing(ctx, plan.Output)
	if err != nil {
		return plan, nil, fmt.Errorf("cannot confirm that no service runs %s: %w", plan.Output, err)
	}
	if len(units) > 0 {
		return plan, nil, fmt.Errorf("%s is run by %s; 'build' never replaces a live binary, use 'install'", plan.Output, units[0].Name)
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
		v := semver.Canonical(opts.CaddyVersion)
		if m := semver.Major(v); m > 0 && m != installedMajor && !opts.AllowMajor {
			than := fmt.Sprintf("the installed v%d", installedMajor)
			if src == nil {
				than = fmt.Sprintf("the v%d this tool builds by default", installedMajor)
			}
			return nil, fmt.Errorf("--caddy-version %s is a different major version than %s; plugins built for one major do not compile against another, pass --allow-major to do this deliberately", v, than)
		}
		p.CaddyVersion = v
	}

	// Start from the installed plugin set, pinned.
	index := map[string]int{} // package path -> position in p.Plugins
	if src != nil {
		for _, ip := range src.Plugins {
			if ip.Error != "" {
				return nil, fmt.Errorf("plugin %s in %s reported an error: %s", ip.ModuleID, src.Path, ip.Error)
			}
			if ip.Package == "" || ip.Version == "" {
				return nil, fmt.Errorf("plugin %s in %s has no module path or version in build info; cannot pin it", ip.ModuleID, src.Path)
			}
			if _, dup := index[ip.Package]; dup {
				continue // several Caddy modules from one Go module
			}
			index[ip.Package] = len(p.Plugins)
			p.Plugins = append(p.Plugins, Plugin{
				ModuleID: ip.ModuleID, Package: ip.Package, Version: ip.Version,
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
			if !opts.AllowMajor {
				return nil, fmt.Errorf("--with %s would move %s from major v%d to v%d; pass --allow-major to do this deliberately", spec, pl.Package, majorOfPath(pl.Package), major)
			}
			delete(index, pl.Package)
			pl.Note = join(pl.Note, "major version change from "+pl.Package+"@"+pl.Installed)
			pl.Package, pl.Version, pl.Source = path, version, Overridden
			index[path] = i
			continue
		}
		index[path] = len(p.Plugins)
		p.Plugins = append(p.Plugins, Plugin{Package: path, Version: version, Source: Added})
	}

	// Replacements, and installed replacements that must be covered.
	covered := map[string]bool{}
	for _, r := range opts.Replace {
		old, repl, ok := strings.Cut(r, "=")
		if !ok || old == "" || repl == "" {
			return nil, fmt.Errorf("--replace %q: want old=new", r)
		}
		covered[old] = true
		p.Replacements = append(p.Replacements, xcaddy.NewReplace(old, repl))
	}
	for _, pl := range p.Plugins {
		if pl.replacedBy != "" && !covered[pl.Package] && pl.Source == Pinned {
			return nil, fmt.Errorf("%s was built with a module replacement (=> %s), which cannot be reproduced automatically; pass --replace %s=<path or module@version>", pl.Package, pl.replacedBy, pl.Package)
		}
	}

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

func find(plugins []Plugin, name string) (int, bool) {
	for i, pl := range plugins {
		if pl.Package == name || pl.ModuleID == name {
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
			if pl.ModuleID != "" {
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

	built, err := caddybin.Inspect(ctx, tmpPath)
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
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		lockf.Close()
		return nil, err
	}
	// Write the lockfile through the reserved handle first, so a lockfile
	// failure leaves the requested output untouched; then move both.
	built.Path, built.ResolvedPath = p.Output, p.Output
	if err := writeLockfile(lockf, p, built); err != nil {
		return nil, err
	}
	if err := os.Rename(tmpPath, p.Output); err != nil {
		os.Remove(tmpLock)
		return nil, fmt.Errorf("moving the new binary into place: %w", err)
	}
	lockPath := p.Output + ".lock.json"
	if err := os.Rename(tmpLock, lockPath); err != nil {
		return nil, fmt.Errorf("the binary is at %s but its lockfile could not be moved into place: %w", p.Output, err)
	}
	return &Result{Output: p.Output, Lockfile: lockPath, Built: built}, nil
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
	}
	return nil
}

// Lockfile records exactly what went into a build, with checksums, so an
// installed binary can be traced to its inputs. Nothing reads it back yet;
// install only requires that one exists beside a --from binary.
type Lockfile struct {
	Schema       int          `json:"schema"`
	BuiltAt      time.Time    `json:"built_at"`
	GoVersion    string       `json:"go_version"` // the toolchain that compiled the binary
	Caddy        LockModule   `json:"caddy"`
	Plugins      []LockModule `json:"plugins"`
	SourceBinary *LockSource  `json:"source_binary,omitempty"`
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
// created file and closes it.
func writeLockfile(f *os.File, p *Plan, built *caddybin.Info) error {
	defer f.Close()
	lf := Lockfile{
		Schema:    1,
		BuiltAt:   time.Now().UTC().Truncate(time.Second),
		GoVersion: built.GoVersion,
		Caddy:     LockModule{Package: built.MainPath, Version: built.MainVersion, Sum: built.MainSum},
		Plugins:   []LockModule{},
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
		lm := LockModule{ModuleID: bp.ModuleID, Package: bp.Package, Version: bp.Version, Sum: bp.Sum, Source: src}
		if bp.Replace != "" {
			lm.ReplacedBy = bp.Replace
			if bp.ReplaceVersion != "" {
				lm.ReplacedBy += "@" + bp.ReplaceVersion
			}
		}
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
