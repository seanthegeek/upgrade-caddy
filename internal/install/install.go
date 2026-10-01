// Package install implements the `install` command: build (or take) a new
// Caddy, validate it against the live config, swap it over the installed
// binary without a gap, restart the service that runs it, and roll back if
// the service does not come up.
//
// This is the only package that changes system state. Resolve decides
// everything and has no side effects; Run does the work in a fixed order.
package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/seanthegeek/upgrade-caddy/internal/build"
	"github.com/seanthegeek/upgrade-caddy/internal/caddybin"
	"github.com/seanthegeek/upgrade-caddy/internal/pkgmgr"
	"github.com/seanthegeek/upgrade-caddy/internal/systemd"
)

// Options controls an install.
type Options struct {
	Binary    string // installed binary to reproduce and replace ("" searches PATH)
	Target    string // where to install; defaults to Binary
	From      string // install this binary, produced by build, instead of building
	Fresh     bool   // nothing is installed yet; build from --with only (requires Target)
	Config    string // config to validate against; defaults to the service's --config
	NoRestart bool   // swap the binary but leave the service alone
	DryRun    bool
	Verbose   bool

	Build build.Options // version and plugin selection, timeouts, proxy

	Log         io.Writer          // progress; nil discards
	Control     systemd.Controller // nil means the real systemctl
	RestartWait time.Duration      // how long to wait for a restarted unit to become active
	OnPlan      func(*Plan)        // called with the resolved plan before anything changes
}

// Plan is everything install has decided, before it changes anything.
type Plan struct {
	Target       string         `json:"target"`
	TargetExists bool           `json:"target_exists"`
	Installed    *caddybin.Info `json:"installed,omitempty"`
	Units        []systemd.Unit `json:"units"`
	From         string         `json:"from,omitempty"`
	FromInfo     *caddybin.Info `json:"from_info,omitempty"`
	Build        *build.Plan    `json:"build,omitempty"`

	Config     string   `json:"config,omitempty"`
	Adapter    string   `json:"adapter,omitempty"`
	EnvFiles   []string `json:"envfiles,omitempty"`
	WorkDir    string   `json:"workdir,omitempty"`
	ConfigFrom string   `json:"config_from,omitempty"` // "--config" or the unit name

	Caps        string   `json:"file_capabilities,omitempty"` // getcap output to re-apply
	NeedsRoot   bool     `json:"needs_root"`
	RootReasons []string `json:"root_reasons,omitempty"`

	buildOpts build.Options
}

// Result is what Run did.
type Result struct {
	Target     string
	Previous   string // rollback copy, "" when there was nothing installed
	Lockfile   string
	Installed  *caddybin.Info
	Validated  bool
	Restarted  []string
	RolledBack bool
}

// Run resolves, prints, checks privileges, obtains the new binary,
// validates, swaps, restarts and verifies.
func Run(ctx context.Context, opts Options) (*Plan, *Result, error) {
	plan, err := Resolve(ctx, opts)
	if err != nil {
		return nil, nil, err
	}
	if opts.OnPlan != nil {
		opts.OnPlan(plan)
	}
	if opts.DryRun {
		return plan, nil, nil
	}
	if plan.NeedsRoot {
		return plan, nil, fmt.Errorf("root is required to %s; re-run with sudo, or build as your own user and run `sudo upgrade-caddy install --from <path>`",
			strings.Join(plan.RootReasons, " and "))
	}
	logw := opts.Log
	if logw == nil {
		logw = io.Discard
	}

	// 1. Obtain the new binary in the target's directory, so the final
	// rename is atomic.
	var newPath, newLock string
	if plan.From != "" {
		newPath, newLock, err = stage(plan.From, plan.Target)
		if err != nil {
			return plan, nil, err
		}
	} else {
		res, err := plan.Build.Build(ctx, plan.buildOpts)
		if err != nil {
			return plan, nil, err
		}
		newPath, newLock = res.Output, res.Lockfile
	}
	cleanup := func() {
		os.Remove(newPath)
		os.Remove(newLock)
	}
	newInfo, err := caddybin.Inspect(ctx, newPath)
	if err != nil {
		cleanup()
		return plan, nil, fmt.Errorf("inspecting the new binary: %w", err)
	}
	result := &Result{Target: plan.Target, Installed: newInfo}

	// 2. Validate with the new binary against the real config.
	if plan.Config != "" {
		fmt.Fprintf(logw, "==> validating %s with the new binary\n", plan.Config)
		if err := validate(ctx, newPath, plan, logw, opts.Verbose); err != nil {
			cleanup()
			return plan, nil, err
		}
		result.Validated = true
	} else {
		fmt.Fprintln(logw, "==> no config found to validate against (no service runs this binary and --config was not given); skipping validation")
	}

	// 3. Swap.
	fmt.Fprintf(logw, "==> installing %s\n", plan.Target)
	previous, err := swap(plan.Target, newPath, plan.Caps)
	if err != nil {
		cleanup()
		return plan, nil, err
	}
	result.Previous = previous
	result.Lockfile = plan.Target + ".lock.json"
	if err := os.Rename(newLock, result.Lockfile); err != nil {
		fmt.Fprintf(logw, "    warning: could not move lockfile into place: %v\n", err)
		result.Lockfile = ""
	}

	// 4. Restart and verify, rolling back on failure.
	if opts.NoRestart || len(plan.Units) == 0 {
		if len(plan.Units) > 0 {
			fmt.Fprintf(logw, "==> --no-restart: %s still runs the previous binary until restarted\n", unitNames(plan.Units))
		}
		return plan, result, nil
	}
	ctl := opts.Control
	if ctl == nil {
		ctl = systemd.Systemctl{}
	}
	wait := opts.RestartWait
	if wait == 0 {
		wait = 15 * time.Second
	}
	for _, u := range plan.Units {
		fmt.Fprintf(logw, "==> restarting %s\n", u.Name)
		if err := restartAndVerify(ctx, ctl, u.Name, plan.Target, wait); err != nil {
			fmt.Fprintf(logw, "    %v\n", err)
			if previous == "" {
				return plan, result, fmt.Errorf("%s did not come up with the new binary and there is no previous binary to roll back to", u.Name)
			}
			fmt.Fprintf(logw, "==> rolling back to %s\n", previous)
			rbErr := rollback(plan.Target, previous)
			result.RolledBack = true
			var restartErrs []error
			for _, again := range plan.Units {
				if rerr := ctl.Restart(ctx, again.Name); rerr != nil {
					restartErrs = append(restartErrs, rerr)
				}
			}
			return plan, result, errors.Join(
				fmt.Errorf("%s did not come up with the new binary: %w; rolled back (the failed binary is kept at %s.failed)", u.Name, err, plan.Target),
				rbErr, errors.Join(restartErrs...))
		}
		result.Restarted = append(result.Restarted, u.Name)
	}
	return plan, result, nil
}

// Resolve decides what install would do. It inspects the target, refuses
// system packages and distribution builds, finds the service, works out
// what to validate against and whether root is needed, and resolves the
// build plan. Nothing on disk changes.
func Resolve(ctx context.Context, opts Options) (*Plan, error) {
	if opts.From != "" && opts.Fresh {
		return nil, errors.New("--from and --fresh cannot be combined")
	}
	p := &Plan{}

	// Target.
	switch {
	case opts.Target != "":
		abs, err := filepath.Abs(opts.Target)
		if err != nil {
			return nil, err
		}
		p.Target = abs
	case opts.Fresh:
		return nil, errors.New("--fresh needs --target: there is no installed binary to take the path from")
	default:
		path, err := caddybin.Find(opts.Binary)
		if err != nil {
			return nil, fmt.Errorf("%w (or --fresh --target PATH for a first install)", err)
		}
		p.Target = path
	}
	if fi, err := os.Stat(p.Target); err == nil {
		if fi.IsDir() {
			return nil, fmt.Errorf("%s is a directory", p.Target)
		}
		p.TargetExists = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	// Never over a system package or a distribution build.
	if p.TargetExists {
		resolved := p.Target
		if r, err := filepath.EvalSymlinks(p.Target); err == nil {
			resolved = r
		}
		if owner := pkgmgr.Find(ctx, resolved); owner != nil {
			return nil, refuseSystemPackage(p.Target, owner)
		}
		info, err := caddybin.Inspect(ctx, p.Target)
		if err != nil {
			return nil, err
		}
		if info.IsDistroBuild() {
			return nil, refuseSystemPackage(p.Target, nil)
		}
		p.Installed = info
	} else if !opts.Fresh && opts.From == "" {
		return nil, fmt.Errorf("nothing is installed at %s; use --fresh --target %s with --with for each plugin", p.Target, p.Target)
	}

	// Service and config.
	units, err := systemd.UnitsUsing(ctx, p.Target)
	if err != nil {
		return nil, fmt.Errorf("querying systemd: %w", err)
	}
	p.Units = units
	if opts.Config != "" {
		abs, err := filepath.Abs(opts.Config)
		if err != nil {
			return nil, err
		}
		p.Config, p.ConfigFrom = abs, "--config"
	} else {
		for _, u := range units {
			if cfg, adapter, env := u.ConfigArgs(); cfg != "" {
				p.Config, p.Adapter, p.EnvFiles, p.WorkDir, p.ConfigFrom = cfg, adapter, env, u.WorkingDirectory, u.Name
				break
			}
		}
	}

	// File capabilities to carry over.
	if p.TargetExists {
		p.Caps = fileCaps(ctx, p.Target)
	}

	// Privileges.
	restart := len(units)
	if opts.NoRestart {
		restart = 0
	}
	p.NeedsRoot, p.RootReasons = needsRoot(os.Geteuid(), dirWritable(filepath.Dir(p.Target)), filepath.Dir(p.Target), restart, unitNames(units), p.Caps != "")

	// Source of the new binary.
	if opts.From != "" {
		abs, err := filepath.Abs(opts.From)
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(abs + ".lock.json"); err != nil {
			return nil, fmt.Errorf("--from %s: no %s.lock.json beside it; install only takes binaries produced by `upgrade-caddy build`", abs, abs)
		}
		info, err := caddybin.Inspect(ctx, abs)
		if err != nil {
			return nil, fmt.Errorf("--from: %w", err)
		}
		if info.IsDistroBuild() {
			return nil, fmt.Errorf("--from %s: no Go module information; not a build output", abs)
		}
		p.From, p.FromInfo = abs, info
		return p, nil
	}
	bopts := opts.Build
	bopts.Fresh = opts.Fresh
	if !opts.Fresh {
		bopts.Binary = p.Target
	}
	bopts.Output = filepath.Join(filepath.Dir(p.Target), fmt.Sprintf(".%s-install-%d", filepath.Base(p.Target), os.Getpid()))
	bopts.DryRun = true
	bopts.Log = opts.Log
	bopts.Verbose = opts.Verbose
	bopts.OnPlan = nil
	bplan, _, err := build.Run(ctx, bopts)
	if err != nil {
		return nil, err
	}
	bopts.DryRun = false
	p.Build, p.buildOpts = bplan, bopts
	return p, nil
}

// refuseSystemPackage explains why install will not touch a system
// package's binary and what to do instead.
func refuseSystemPackage(target string, owner *pkgmgr.Owner) error {
	var what, remove string
	if owner != nil {
		what = fmt.Sprintf("%s is installed by the %s package %q", target, owner.Manager, owner.Package)
		remove = removeCommand(owner)
	} else {
		what = fmt.Sprintf("%s is a distribution-style build with no Go module information", target)
		remove = "uninstall it with the package manager that installed it"
	}
	return fmt.Errorf(`%s. install never writes over a system package: the next package upgrade would silently undo it.

To switch to a custom build:
  1. uninstall the system package first: %s
     (the package's systemd unit and caddy user go with it; /etc/caddy is kept)
  2. recreate the service and user per https://caddyserver.com/docs/running#manual-installation
  3. run: sudo upgrade-caddy install --fresh --target %s --with <plugin> ...`, what, remove, target)
}

func removeCommand(o *pkgmgr.Owner) string {
	switch o.Manager {
	case "dpkg":
		return "sudo apt remove " + o.Package
	case "rpm":
		return "sudo dnf remove " + o.Package
	case "pacman":
		return "sudo pacman -R " + o.Package
	case "apk":
		return "sudo apk del " + o.Package
	}
	return "remove package " + o.Package
}

// needsRoot decides whether the install must run as root, and why.
func needsRoot(euid int, dirWritable bool, dir string, restartUnits int, units string, hasCaps bool) (bool, []string) {
	var reasons []string
	if !dirWritable {
		reasons = append(reasons, "write to "+dir)
	}
	if restartUnits > 0 {
		reasons = append(reasons, "restart "+units)
	}
	if hasCaps {
		reasons = append(reasons, "re-apply file capabilities")
	}
	return euid != 0 && len(reasons) > 0, reasons
}

// dirWritable reports whether the current user can create files in dir,
// by trying to.
func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".upgrade-caddy-probe-*")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}

// fileCaps returns the binary's file capabilities as getcap prints them,
// or "" when there are none or getcap is unavailable.
func fileCaps(ctx context.Context, path string) string {
	if _, err := exec.LookPath("getcap"); err != nil {
		return ""
	}
	out, err := exec.CommandContext(ctx, "getcap", path).Output()
	if err != nil {
		return ""
	}
	return parseCaps(string(out))
}

// parseCaps extracts the capability text from getcap output. Newer libcap
// prints "/usr/bin/caddy cap_net_bind_service=ep", older prints
// "/usr/bin/caddy = cap_net_bind_service+ep". setcap accepts either form.
func parseCaps(out string) string {
	line := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	if line == "" {
		return ""
	}
	_, caps, ok := strings.Cut(line, " ")
	if !ok {
		return ""
	}
	caps = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(caps), "="))
	return caps
}

func unitNames(units []systemd.Unit) string {
	names := make([]string, len(units))
	for i, u := range units {
		names[i] = u.Name
	}
	return strings.Join(names, ", ")
}

// stage copies a --from binary and its lockfile into the target's
// directory so the final rename cannot cross filesystems.
func stage(from, target string) (binPath, lockPath string, err error) {
	dir := filepath.Dir(target)
	binPath = filepath.Join(dir, fmt.Sprintf(".%s-install-%d", filepath.Base(target), os.Getpid()))
	lockPath = binPath + ".lock.json"
	if err := copyFile(from, binPath, 0o755); err != nil {
		return "", "", fmt.Errorf("staging %s: %w", from, err)
	}
	if err := copyFile(from+".lock.json", lockPath, 0o644); err != nil {
		os.Remove(binPath)
		return "", "", fmt.Errorf("staging lockfile: %w", err)
	}
	return binPath, lockPath, nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	// Close can report a write failure of its own, so its error matters on
	// both paths, not just the successful one.
	if err := errors.Join(copyErr, out.Close()); err != nil {
		os.Remove(dst)
		return err
	}
	return nil
}

// validate runs `<new binary> validate` against the plan's config.
func validate(ctx context.Context, bin string, p *Plan, logw io.Writer, verbose bool) error {
	args := []string{"validate", "--config", p.Config}
	if p.Adapter != "" {
		args = append(args, "--adapter", p.Adapter)
	}
	for _, f := range p.EnvFiles {
		args = append(args, "--envfile", f)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = p.WorkDir
	out, err := cmd.CombinedOutput()
	if verbose || err != nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			fmt.Fprintf(logw, "    %s\n", line)
		}
	}
	if err != nil {
		return fmt.Errorf("the new binary rejected %s: %w; nothing was changed", p.Config, err)
	}
	return nil
}

// swap puts newPath at target with no moment where nothing is there. The
// current file is hard-linked to <target>.previous first, so a rollback
// copy exists before the atomic rename replaces the target. Mode and
// ownership are copied from the old file; file capabilities are re-applied.
// It returns the rollback path, or "" when there was no previous file.
func swap(target, newPath, caps string) (previous string, err error) {
	if old, err := os.Stat(target); err == nil {
		if err := os.Chmod(newPath, old.Mode().Perm()); err != nil {
			return "", err
		}
		if st, ok := old.Sys().(*syscall.Stat_t); ok {
			if err := os.Chown(newPath, int(st.Uid), int(st.Gid)); err != nil && !errors.Is(err, os.ErrPermission) {
				return "", fmt.Errorf("setting owner of new binary: %w", err)
			}
		}
		previous = target + ".previous"
		os.Remove(previous)
		if err := os.Link(target, previous); err != nil {
			return "", fmt.Errorf("keeping rollback copy: %w", err)
		}
	}
	if err := os.Rename(newPath, target); err != nil {
		if previous != "" {
			os.Remove(previous)
		}
		return "", fmt.Errorf("replacing %s: %w", target, err)
	}
	if caps != "" {
		if out, err := exec.Command("setcap", caps, target).CombinedOutput(); err != nil {
			rbErr := rollback(target, previous)
			return "", errors.Join(fmt.Errorf("re-applying file capabilities %q: %w: %s; rolled back", caps, err, strings.TrimSpace(string(out))), rbErr)
		}
	}
	return previous, nil
}

// rollback puts the previous binary back. The failed one is kept as
// <target>.failed for inspection.
func rollback(target, previous string) error {
	if previous == "" {
		return errors.New("no previous binary to roll back to")
	}
	failed := target + ".failed"
	os.Remove(failed)
	if err := os.Link(target, failed); err != nil {
		return fmt.Errorf("rollback: keeping failed binary: %w", err)
	}
	if err := os.Rename(previous, target); err != nil {
		return fmt.Errorf("rollback: restoring %s: %w", target, err)
	}
	return nil
}

// settleDelay is how long a unit must stay active before it counts as up.
// A Type=simple unit is "active" the instant its process forks, so a binary
// that dies on startup would otherwise look like a successful restart.
var settleDelay = time.Second

// restartAndVerify restarts the unit, waits for it to become active and stay
// active, and when it can, confirms the main process runs the target binary.
func restartAndVerify(ctx context.Context, ctl systemd.Controller, unit, target string, wait time.Duration) error {
	if err := ctl.Restart(ctx, unit); err != nil {
		return err
	}
	deadline := time.Now().Add(wait)
	for {
		active, err := ctl.IsActive(ctx, unit)
		if err != nil {
			return err
		}
		if active {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s is not active %s after restart", unit, wait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(settleDelay):
	}
	if active, err := ctl.IsActive(ctx, unit); err != nil {
		return err
	} else if !active {
		return fmt.Errorf("%s became active but did not stay active for %s", unit, settleDelay)
	}
	pid, err := ctl.MainPID(ctx, unit)
	if err != nil || pid == 0 {
		return nil // active is the best we can confirm
	}
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return nil // not readable unprivileged; being active is enough
	}
	want := target
	if r, err := filepath.EvalSymlinks(target); err == nil {
		want = r
	}
	if exe != want {
		return fmt.Errorf("%s is active but its main process runs %s, not %s", unit, exe, want)
	}
	return nil
}

// WriteText prints the plan for a human.
func (p *Plan) WriteText(w io.Writer) {
	fmt.Fprintf(w, "Target:   %s", p.Target)
	if p.Installed != nil {
		fmt.Fprintf(w, " (%s, %d plugins)", p.Installed.MainVersion, len(p.Installed.Plugins))
	} else if !p.TargetExists {
		fmt.Fprint(w, " (nothing installed yet)")
	}
	fmt.Fprintln(w)
	if len(p.Units) == 0 {
		fmt.Fprintln(w, "Service:  none runs this binary; nothing will be restarted")
	}
	for _, u := range p.Units {
		fmt.Fprintf(w, "Service:  %s (%s/%s)\n", u.Name, u.ActiveState, u.SubState)
	}
	if p.Config != "" {
		fmt.Fprintf(w, "Validate: %s (from %s)\n", p.Config, p.ConfigFrom)
	} else {
		fmt.Fprintln(w, "Validate: skipped, no config known; pass --config to validate")
	}
	if p.Caps != "" {
		fmt.Fprintf(w, "Caps:     %s (will be re-applied)\n", p.Caps)
	}
	if p.NeedsRoot {
		fmt.Fprintf(w, "Root:     required to %s\n", strings.Join(p.RootReasons, " and "))
	}
	if p.From != "" {
		fmt.Fprintf(w, "From:     %s (%s, %d plugins)\n", p.From, p.FromInfo.MainVersion, len(p.FromInfo.Plugins))
		return
	}
	fmt.Fprintln(w, "Build:")
	p.Build.WriteText(indent{w})
}

type indent struct{ w io.Writer }

func (i indent) Write(b []byte) (int, error) {
	s := strings.TrimRight(string(b), "\n")
	for _, line := range strings.Split(s, "\n") {
		fmt.Fprintf(i.w, "  %s\n", line)
	}
	return len(b), nil
}
