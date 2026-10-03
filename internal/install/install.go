// Package install implements the `install` command: build (or take) a new
// Caddy, validate it against the live config, swap it over the installed
// binary without a gap, restart the service that runs it, and roll back if
// the service does not come up.
//
// This is the only package that changes system state. Resolve decides
// everything and has no side effects; Run does the work in a fixed order.
package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/seanthegeek/upgrade-caddy/internal/build"
	"github.com/seanthegeek/upgrade-caddy/internal/caddybin"
	"github.com/seanthegeek/upgrade-caddy/internal/fspath"
	"github.com/seanthegeek/upgrade-caddy/internal/pkgmgr"
	"github.com/seanthegeek/upgrade-caddy/internal/systemd"
)

// Options controls an install.
type Options struct {
	Target    string // binary to reproduce and replace; "" means the first caddy on PATH
	From      string // install this binary, produced by build, instead of building
	Fresh     bool   // nothing is installed yet; build from --with only (requires Target)
	Config    string // config to validate against; defaults to the service's --config
	NoRestart bool   // swap the binary but leave the service alone
	DryRun    bool
	Verbose   bool

	// Build carries the version and plugin selection (CaddyVersion, Upgrade,
	// UpgradeAll, With, Replace, AllowMajor), Proxy and TimeoutBuild. Its
	// Binary, Fresh, Output, DryRun, Verbose, Log and OnPlan fields are
	// overwritten from the fields above and from the target.
	Build build.Options

	Log         io.Writer          // progress; nil discards
	Control     systemd.Controller // nil means the real systemctl
	RestartWait time.Duration      // how long to wait for a restarted unit to become active
	OnPlan      func(*Plan)        // called with the resolved plan before anything changes
}

// Plan is everything install has decided, before it changes anything.
type Plan struct {
	Target       string         `json:"target"`                     // the real file that is replaced; symlinks are resolved
	Requested    string         `json:"requested_target,omitempty"` // the path given, when it was a symlink to Target
	TargetExists bool           `json:"target_exists"`
	Installed    *caddybin.Info `json:"installed,omitempty"` // what is at Target now
	Units        []systemd.Unit `json:"units"`
	NoSystemd    bool           `json:"no_systemd"` // systemctl is not on PATH, so no unit can be found or restarted
	From         string         `json:"from,omitempty"`
	FromInfo     *caddybin.Info `json:"from_info,omitempty"`
	BuildPlan    *build.Plan    `json:"build,omitempty"`

	Validations []Validation `json:"validations,omitempty"` // every distinct config the new binary is checked against
	RunAs       string       `json:"run_as,omitempty"`      // who the new binary is inspected as, and why; set only when running as root

	FileCapabilities string   `json:"file_capabilities,omitempty"` // human-readable, from getcap when available, else "present"
	NeedsRoot        bool     `json:"needs_root"`
	RootReasons      []string `json:"root_reasons,omitempty"`

	caps      []byte            // raw security.capability value to carry over, nil when none
	runAs     *caddybin.Account // account the new binary is inspected as; nil means the current user
	targetID  fileID            // the file Resolve inspected, so Run can tell if it was replaced meanwhile
	buildOpts build.Options
}

// fileID identifies one file on disk closely enough to notice it being
// replaced: a package upgrade or a rebuild lands a new inode, and an
// in-place rewrite changes size or modification time.
type fileID struct {
	dev, ino uint64
	size     int64
	mtime    time.Time
}

// still confirms the file at path is the one this identity was read from,
// and words a refusal with when the change happened otherwise.
func (id fileID) still(path, when string) error {
	now, err := identify(path)
	if err != nil {
		return fmt.Errorf("rechecking %s: %w", path, err)
	}
	if now != id {
		return fmt.Errorf("%s changed %s; nothing was changed, re-run install", path, when)
	}
	return nil
}

// pairIDs is the identity of a staged binary and its lockfile.
type pairIDs struct{ bin, lock fileID }

// pairID reads the identity of both staged files.
func pairID(binPath, lockPath string) (pairIDs, error) {
	bin, err := identify(binPath)
	if err != nil {
		return pairIDs{}, fmt.Errorf("staged binary: %w", err)
	}
	lock, err := identify(lockPath)
	if err != nil {
		return pairIDs{}, fmt.Errorf("staged lockfile: %w", err)
	}
	return pairIDs{bin: bin, lock: lock}, nil
}

// unchanged confirms the staged files are still the ones that were
// inspected and validated. A different inode, size or modification time
// means something else was put at the path, and installing it would
// install a file nobody checked.
func (p pairIDs) unchanged(binPath, lockPath string) error {
	now, err := pairID(binPath, lockPath)
	if err != nil {
		return fmt.Errorf("rechecking the staged files: %w", err)
	}
	if now.bin != p.bin {
		return fmt.Errorf("the staged binary %s changed after it was validated; nothing was changed, re-run install", binPath)
	}
	if now.lock != p.lock {
		return fmt.Errorf("the staged lockfile %s changed after it was checked; nothing was changed, re-run install", lockPath)
	}
	return nil
}

// lockTarget takes an exclusive, cross-process lock for installs of one
// target, a flock on a lock file beside it, and returns the function that
// releases it. Another install holding it is a refusal, not a wait: the
// other one may take minutes, and its outcome changes what this one should
// do. The lock file itself stays behind; it holds nothing.
func lockTarget(target string) (release func(), err error) {
	path := filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+".upgrade-caddy.lock")
	// Opened read-only: flock needs no write access, nothing is ever
	// written to the file, and so closing it can lose nothing.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening the install lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another upgrade-caddy install of %s is running (it holds %s); wait for it to finish", target, path)
		}
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck // closing releases it anyway
		f.Close()
	}, nil
}

// identify reads a file's identity.
func identify(path string) (fileID, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return fileID{}, err
	}
	id := fileID{size: fi.Size(), mtime: fi.ModTime()}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		id.dev, id.ino = uint64(st.Dev), uint64(st.Ino) // Dev's width differs by platform
	}
	return id, nil
}

// Validation is one config the new binary must accept before the swap:
// the unit's --config, --adapter and --envfile flags run in its working
// directory, or the --config flag of install itself.
type Validation struct {
	Config   string   `json:"config"`
	Adapter  string   `json:"adapter,omitempty"`
	EnvFiles []string `json:"env_files,omitempty"`
	WorkDir  string   `json:"work_dir,omitempty"`
	From     string   `json:"from"`           // "--config" or the unit name
	User     string   `json:"user,omitempty"` // who validate runs as; set only when running as root

	// The environment the unit gives its processes, applied to validate so
	// a config that reads {env.*} placeholders is checked with the values
	// the service sees: Environment=, EnvironmentFile= (read when validate
	// runs) and the PATH systemd would set. Unset for a --config
	// validation, which inherits install's own environment.
	UnitEnvironment []string          `json:"unit_environment,omitempty"`
	UnitEnvFiles    []systemd.EnvFile `json:"unit_environment_files,omitempty"`
	ExecSearchPath  []string          `json:"exec_search_path,omitempty"`

	account *caddybin.Account // nil means the current user
	unit    *systemd.Unit     // the unit this validation comes from, nil for --config
}

// Result is what Run did.
type Result struct {
	Target    string
	Previous  string // rollback copy, "" when there was nothing installed
	Lockfile  string
	Built     *caddybin.Info // the binary now at Target
	Validated []string       // configs the new binary was validated against
	Restarted []string
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
		how := "re-run with sudo, or build as your own user and run `sudo upgrade-caddy install --from <path>`"
		if plan.From != "" {
			how = "re-run with sudo"
		}
		return plan, nil, fmt.Errorf("root is required to %s; %s", strings.Join(plan.RootReasons, " and "), how)
	}
	logw := opts.Log
	if logw == nil {
		logw = io.Discard
	}

	// One install of a target at a time, from before anything is staged
	// until the restart or rollback is over: two at once would both pass
	// the rechecks and then overwrite each other's .previous copies.
	release, err := lockTarget(plan.Target)
	if err != nil {
		return plan, nil, err
	}
	defer release()

	// 1. Obtain the new binary in the target's directory, so the final
	// rename is atomic.
	var newPath, newLock string
	if plan.From != "" {
		newPath, newLock, err = stage(plan.From, plan.Target)
		if err != nil {
			return plan, nil, err
		}
	} else {
		res, err := plan.BuildPlan.Build(ctx, plan.buildOpts)
		if err != nil {
			return plan, nil, err
		}
		newPath, newLock = res.Output, res.Lockfile
	}
	cleanup := func() {
		os.Remove(newPath)
		os.Remove(newLock)
	}
	// The staged files are inspected and validated by path, then renamed
	// by path, so their identity is taken now and checked again right
	// before the rename: a file swapped in between would be installed
	// without having been checked.
	stagedID, err := pairID(newPath, newLock)
	if err != nil {
		cleanup()
		return plan, nil, err
	}
	newInfo, err := caddybin.InspectAs(ctx, newPath, plan.runAs)
	if err != nil {
		cleanup()
		return plan, nil, fmt.Errorf("inspecting the new binary: %w", err)
	}
	// The lockfile installed beside the target is its attestation, so the
	// pair checked is the pair that gets installed: the staged copies,
	// not the --from files Resolve looked at, which anyone able to write
	// the source directory could have replaced since.
	if err := checkStaged(newLock, newInfo); err != nil {
		cleanup()
		return plan, nil, err
	}
	// The plan's safety checks are minutes old by now if a build ran.
	// Confirm the world still matches it before anything is validated or
	// replaced.
	if err := recheck(ctx, plan); err != nil {
		cleanup()
		return plan, nil, err
	}
	result := &Result{Target: plan.Target, Built: newInfo}

	// 2. Validate with the new binary against every config in use.
	if len(plan.Validations) == 0 {
		fmt.Fprintln(logw, "==> no config known to validate against (no unit passes --config to this binary and --config was not given); skipping validation")
	}
	for _, v := range plan.Validations {
		fmt.Fprintf(logw, "==> validating %s with the new binary\n", v.Config)
		if err := validate(ctx, newPath, v, logw, opts.Verbose); err != nil {
			cleanup()
			return plan, nil, err
		}
		result.Validated = append(result.Validated, v.Config)
	}

	// 3. Swap the binary, then the lockfile, keeping both previous copies.
	// Validation can take a while too, so the world is checked once more
	// right before the swap: the mode, owner and capabilities about to be
	// applied were read from the file Resolve saw, and must not land on a
	// file that was replaced or became a package's since.
	if err := recheck(ctx, plan); err != nil {
		cleanup()
		return plan, nil, err
	}
	if err := stagedID.unchanged(newPath, newLock); err != nil {
		cleanup()
		return plan, nil, err
	}
	fmt.Fprintf(logw, "==> installing %s\n", plan.Target)
	previous, err := swap(plan.Target, newPath, plan.caps)
	if err != nil {
		cleanup()
		return plan, nil, err
	}
	result.Previous = previous
	result.Lockfile = plan.Target + ".lock.json"
	if err := commitLockfile(plan.Target, newLock, previous); err != nil {
		return plan, nil, err
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
	names := make([]string, len(plan.Units))
	for i, u := range plan.Units {
		names[i] = u.Name
	}
	result.Restarted, err = restartUnits(ctx, ctl, names, plan.Target, previous, wait, logw)
	return plan, result, err
}

// restartUnits restarts every unit and verifies each comes up. On the first
// failure it restores the previous binary and lockfile, restarts all units
// again under a fresh deadline (the caller's context may already be
// cancelled, which is often why the restart failed), and returns an error
// that says whether the rollback actually succeeded.
func restartUnits(ctx context.Context, ctl systemd.Controller, units []string, target, previous string, wait time.Duration, logw io.Writer) (restarted []string, err error) {
	for _, u := range units {
		fmt.Fprintf(logw, "==> restarting %s\n", u)
		rerr := restartAndVerify(ctx, ctl, u, target, wait)
		if rerr == nil {
			restarted = append(restarted, u)
			continue
		}
		fmt.Fprintf(logw, "    %v\n", rerr)
		if previous == "" {
			// First install: the state to restore is "nothing there", which
			// for every unit restarted on the new binary means stopped;
			// left running they would hold an unlinked executable and could
			// never restart from the removed path. That includes the unit
			// that just failed verification: Restart was issued to it too,
			// and a wrong main process or a failed settle check leaves it
			// active.
			rbCtx, cancel := context.WithTimeout(context.Background(), wait+15*time.Second)
			var stopErrs []error
			toStop := append(slices.Clone(restarted), u)
			for _, started := range toStop {
				fmt.Fprintf(logw, "==> stopping %s again\n", started)
				if e := ctl.Stop(rbCtx, started); e != nil {
					stopErrs = append(stopErrs, e)
				}
			}
			cancel()
			if len(stopErrs) > 0 {
				// A unit that could not be stopped may still be running;
				// removing its binary now would leave it on an unlinked
				// executable that can never restart, the very state this
				// cleanup exists to prevent. The files stay for the
				// operator to deal with.
				return restarted, errors.Join(fmt.Errorf("%s did not come up with the new binary: %w; and a unit could not be stopped, so the new binary and lockfile were left at %s rather than removed from under a running service: stop the unit, then remove them (there was nothing installed before)", u, rerr, target), errors.Join(stopErrs...))
			}
			fmt.Fprintf(logw, "==> removing %s again\n", target)
			rbErr := errors.Join(removeIfPresent(target), rollbackLockfile(target))
			if rbErr != nil {
				return restarted, errors.Join(fmt.Errorf("%s did not come up with the new binary: %w; and restoring the pre-install state of %s was incomplete", u, rerr, target), rbErr)
			}
			return restarted, fmt.Errorf("%s did not come up with the new binary: %w; the new binary and lockfile were removed from %s again and the %d restarted unit(s) stopped (there was nothing installed before)", u, rerr, target, len(toStop))
		}
		fmt.Fprintf(logw, "==> rolling back to %s\n", previous)
		restoreErr, keepErr := rollback(target, previous)
		summary := rolledBack(restoreErr, keepErr)
		var lockErr, recovery error
		if restoreErr != nil {
			// The target still holds the failed binary. Its lockfile stays
			// with it, so the pair still describes itself, and no unit is
			// restarted again: restarting them all on the failed binary
			// would take down the ones that came up before the failure.
			summary += "; the new lockfile was left beside it and no unit was restarted again"
		} else {
			// The lockfile follows the binary, and the restored binary is
			// restarted and verified the same way the new one was, under
			// a fresh deadline per unit: a restart that is accepted and
			// then fails is exactly the case being recovered from.
			lockErr = rollbackLockfile(target)
			var recoveryErrs []error
			for _, again := range units {
				rbCtx, cancel := context.WithTimeout(context.Background(), wait+settleDelay+15*time.Second)
				if e := restartAndVerify(rbCtx, ctl, again, target, wait); e != nil {
					recoveryErrs = append(recoveryErrs, fmt.Errorf("after rollback, %w", e))
				}
				cancel()
			}
			recovery = errors.Join(recoveryErrs...)
			if lockErr != nil {
				summary += ", but its lockfile could not be restored"
			}
			if recovery != nil {
				summary += ", but the service did not come back up on it"
			}
		}
		return restarted, errors.Join(
			fmt.Errorf("%s did not come up with the new binary: %w; %s", u, rerr, summary),
			restoreErr, lockErr, keepErr, recovery)
	}
	return restarted, nil
}

func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// rolledBack words the outcome of a rollback for an error message, so a
// failed restore is never reported as a success and the failed binary is
// said to be kept only when it was.
func rolledBack(restoreErr, keepErr error) string {
	if restoreErr != nil {
		return "ROLLBACK FAILED, the target may still hold the failed binary"
	}
	if keepErr != nil {
		return "rolled back to the previous binary (the failed one could not be kept as <target>.failed)"
	}
	return "rolled back to the previous binary (the failed one is kept as <target>.failed)"
}

// Resolve decides what install would do. It inspects the target, refuses
// system packages and distribution builds, finds the service, works out
// what to validate against and whether root is needed, and resolves the
// build plan. Nothing on disk changes beyond a temporary writability probe
// file in the target directory.
func Resolve(ctx context.Context, opts Options) (*Plan, error) {
	if opts.From != "" && opts.Fresh {
		return nil, errors.New("--from and --fresh cannot be combined")
	}
	if opts.From != "" {
		if sel := buildSelectionFlags(opts.Build); sel != "" {
			return nil, fmt.Errorf("--from installs an existing build, so %s cannot be combined with it; pass those to 'build' instead", sel)
		}
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
		return nil, errors.New("--fresh needs --target: --fresh ignores any installed binary, so there is no path to default to")
	default:
		path, err := caddybin.Find("")
		if err != nil {
			return nil, fmt.Errorf("%w (or --fresh --target PATH for a first install)", err)
		}
		p.Target = path
	}
	// A symlink (a common way to point /usr/local/bin/caddy at a versioned
	// file) is resolved once, and the real file is what gets replaced;
	// hard-linking and renaming the link itself would leave the referent,
	// and every service executing it, on the old binary.
	real, exists, err := resolveTarget(p.Target)
	if err != nil {
		return nil, err
	}
	if real != p.Target {
		p.Requested, p.Target = p.Target, real
	}
	p.TargetExists = exists

	// Never over a system package or a distribution build.
	if p.TargetExists {
		owner, err := pkgmgr.Find(ctx, p.Target)
		if err != nil {
			return nil, fmt.Errorf("refusing to continue: %w; install must know whether %s belongs to a system package before replacing it", err, p.Target)
		}
		if owner != nil {
			return nil, refuseSystemPackage(p.Target, owner)
		}
		// The identity is taken before the inspection and compared after
		// it, so the plan is known to describe the file whose identity the
		// later rechecks compare against; a binary replaced during the
		// inspection would otherwise be planned from one file and checked
		// against another.
		if p.targetID, err = identify(p.Target); err != nil {
			return nil, err
		}
		info, err := caddybin.Inspect(ctx, p.Target)
		if err != nil {
			return nil, err
		}
		if err := p.targetID.still(p.Target, "while it was being inspected"); err != nil {
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
	p.NoSystemd = !systemd.Available()
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
		p.Validations = []Validation{{Config: abs, From: "--config"}}
	} else {
		p.Validations = validationsFromUnits(units)
		if err := checkWorkDirs(p.Validations); err != nil {
			return nil, err
		}
		// A unit whose EnvironmentFile= cannot be read now could not be
		// validated the way it runs; better to know before the build.
		for _, v := range p.Validations {
			if _, err := systemd.LoadEnvironment(*v.unit); err != nil {
				return nil, fmt.Errorf("%s: %w; its config cannot be validated with the environment the service gets", v.From, err)
			}
		}
	}
	// Who runs the new binary before it is installed. Decided here, once,
	// so the plan shows it and Run cannot drift from it. Without root, a
	// validation that would have to switch accounts is a reason to need it.
	self, err := caddybin.Current()
	if err != nil {
		return nil, err
	}
	switchReasons, err := p.chooseAccounts(self, os.Getenv, realLookups)
	if err != nil {
		return nil, err
	}

	// File capabilities to carry over. Not being able to read them is an
	// error, not "none": a rename silently drops them.
	if p.TargetExists {
		caps, err := readCaps(p.Target)
		if err != nil {
			return nil, err
		}
		p.caps = caps
		if caps != nil {
			p.FileCapabilities = describeCaps(ctx, p.Target)
		}
		// The capabilities just read belong to the inspected file, or the
		// plan would carry one file's bits for another.
		if err := p.targetID.still(p.Target, "while its capabilities were being read"); err != nil {
			return nil, err
		}
	}

	// Privileges.
	restartCount := len(units)
	if opts.NoRestart {
		restartCount = 0
	}
	p.NeedsRoot, p.RootReasons = needsRoot(os.Geteuid(), dirWritable(filepath.Dir(p.Target)), filepath.Dir(p.Target), restartCount, unitNames(units), p.caps != nil, switchReasons)

	// Source of the new binary.
	if opts.From != "" {
		abs, err := filepath.Abs(opts.From)
		if err != nil {
			return nil, err
		}
		lf, err := build.ReadLockfile(abs + ".lock.json")
		if err != nil {
			return nil, fmt.Errorf("--from %s: no usable lockfile beside it (%v); install only takes binaries produced by `upgrade-caddy build`", abs, err)
		}
		info, err := caddybin.InspectAs(ctx, abs, p.runAs)
		if err != nil {
			return nil, fmt.Errorf("--from: %w", err)
		}
		if info.IsDistroBuild() {
			return nil, fmt.Errorf("--from %s: no Go module information; not a build output", abs)
		}
		// The lockfile is installed beside the binary as its attestation,
		// so it must actually describe this binary, not a stale or foreign one.
		if err := lf.Describes(info); err != nil {
			return nil, fmt.Errorf("--from %s: the lockfile beside it does not describe this binary: %w", abs, err)
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
	bopts.RunAs = p.runAs
	bplan, _, err := build.Run(ctx, bopts)
	if err != nil {
		return nil, err
	}
	bopts.DryRun = false
	p.BuildPlan, p.buildOpts = bplan, bopts
	return p, nil
}

// resolveTarget follows symlinks from the requested path to the file that
// will actually be replaced. A missing final path is a first install; a
// symlink whose referent is missing is followed too (fspath.Resolve), so
// the first install lands where the link points and the link then works,
// instead of the link itself being taken for the target and refused by
// swap after the build. systemd matches ExecStart paths the same way, so
// the unit running such a link is found.
func resolveTarget(path string) (real string, exists bool, err error) {
	real, exists, err = fspath.Resolve(path)
	if err != nil || !exists {
		return real, exists, err
	}
	fi, err := os.Stat(real)
	if err != nil {
		return "", false, err
	}
	if fi.IsDir() {
		return "", false, fmt.Errorf("%s is a directory", real)
	}
	return real, true, nil
}

// commitLockfile installs the new lockfile and undoes the binary swap when
// that fails: the previous binary is restored, or on a first install the
// just-placed binary is removed, so a failed install never leaves a
// changed target.
func commitLockfile(target, newLock, previous string) error {
	err := swapLockfile(target, newLock)
	if err == nil {
		return nil
	}
	os.Remove(newLock)
	if previous == "" {
		if rmErr := os.Remove(target); rmErr != nil {
			return errors.Join(fmt.Errorf("installing the lockfile: %w; and the new binary could not be removed from %s", err, target), rmErr)
		}
		return fmt.Errorf("installing the lockfile: %w; the new binary was removed from %s again", err, target)
	}
	restoreErr, keepErr := rollback(target, previous)
	return errors.Join(fmt.Errorf("installing the lockfile: %w; %s", err, rolledBack(restoreErr, keepErr)), restoreErr, keepErr)
}

// validationsFromUnits collects every distinct config the units run the
// binary with, so a binary serving several services is checked against
// all of them before any is restarted.
func validationsFromUnits(units []systemd.Unit) []Validation {
	var out []Validation
	seen := map[string]bool{}
	for i, u := range units {
		cfg, adapter, env := u.ConfigArgs()
		if cfg == "" {
			continue
		}
		// Two units reading one config as different users are two
		// validations: what the file looks like depends on who opens it.
		// DynamicUser= changes the account too (such a unit is validated
		// as the inspection account), and the unit's environment changes
		// what {env.*} placeholders expand to, so both are in the key.
		key := strings.Join(append([]string{cfg, adapter, u.WorkingDirectory, u.User, u.Group, strings.Join(u.SupplementaryGroups, " "), strconv.FormatBool(u.DynamicUser), envKey(u)}, env...), "\x00")
		if seen[key] {
			continue
		}
		seen[key] = true
		unit := units[i]
		out = append(out, Validation{Config: cfg, Adapter: adapter, EnvFiles: env, WorkDir: u.WorkingDirectory, From: u.Name,
			UnitEnvironment: u.Environment, UnitEnvFiles: u.EnvironmentFiles, ExecSearchPath: u.ExecSearchPath, unit: &unit})
	}
	return out
}

// envKey renders a unit's environment settings for comparison.
func envKey(u systemd.Unit) string {
	var files []string
	for _, f := range u.EnvironmentFiles {
		files = append(files, fmt.Sprintf("%s:%t", f.Path, f.Optional))
	}
	return strings.Join(u.Environment, "\x01") + "\x02" + strings.Join(files, "\x01") + "\x02" + strings.Join(u.ExecSearchPath, "\x01")
}

// lookups are the account and group lookups chooseAccounts uses, injected
// so the decision can be tested without the host's user database.
type lookups struct {
	account func(name, group string, extra []string) (*caddybin.Account, error)
	group   func(name string) (uint32, error)
}

var realLookups = lookups{account: caddybin.LookupAccount, group: caddybin.LookupGroup}

// chooseAccounts decides who runs the new binary for inspection and
// validation. Only root can switch accounts, and when install runs as root
// a binary it has not installed yet should run as anyone else who is known
// (see caddybin.Account for why):
//
//   - a unit's config is validated as the unit's User=, with its Group= and
//     SupplementaryGroups=, which is exactly who opens the config when the
//     service runs. A unit without User= runs as root, so validating as
//     root is no more than the service itself does; if it sets Group= or
//     SupplementaryGroups= those still apply, since a root service with a
//     restricted capability set reads files by its groups like anyone
//     else. A DynamicUser= account exists only while its unit runs, so
//     that unit is validated as the inspection account instead.
//   - inspection (`version`, `list-modules`), and a --config validation
//     with no unit behind it, run as the user who invoked sudo, else as the
//     first unit's non-root user, else as root; the plan says which.
//
// A unit user that cannot be looked up is a refusal, not a fall back to
// root: validating as the wrong account answers a different question.
//
// Without root nothing can be switched, so a validation whose unit runs as
// another identity (another user, or the same user with other groups, root
// included) is returned as a reason to need root, which Resolve reports
// before any build starts. `install --no-restart` by a normal user would
// otherwise validate a service's config as that user and install a binary
// whose config was never checked with the service's permissions. A
// --config with no unit behind it, and a DynamicUser= unit, are validated
// as the caller, which is what root would do too. self is the caller's
// own identity, from caddybin.Current.
func (p *Plan) chooseAccounts(self *caddybin.Account, getenv func(string) string, lk lookups) (rootReasons []string, err error) {
	if self.UID != 0 {
		for i := range p.Validations {
			v := &p.Validations[i]
			u, ok := unitNamed(p.Units, v.From)
			if !ok || u.DynamicUser {
				continue
			}
			a, err := unitAccount(u, lk)
			if err != nil {
				return nil, fmt.Errorf("%s runs as an account that could not be looked up, so the new binary cannot be validated the way the service would run it: %w", u.Name, err)
			}
			if a == nil {
				if a, err = lk.account("0", "", nil); err != nil {
					return nil, err
				}
			}
			// The account is kept even when the caller already has that
			// identity, since the unit's environment (USER, LOGNAME, HOME)
			// is composed from it; only a switch needs root.
			v.account, v.User = a, a.Name
			if a.UID == 0 {
				v.User = "root"
			}
			if a.SameIdentity(self) {
				continue
			}
			rootReasons = append(rootReasons, fmt.Sprintf("validate %s as %s, the account %s runs as", v.Config, v.User, v.From))
		}
		return rootReasons, nil
	}
	byUnit := map[string]*caddybin.Account{}
	dynamic := map[string]bool{}
	var firstUser *caddybin.Account
	for _, u := range p.Units {
		if u.DynamicUser {
			dynamic[u.Name] = true
			continue
		}
		a, err := unitAccount(u, lk)
		if err != nil {
			return nil, fmt.Errorf("%s runs as an account that could not be looked up, so the new binary cannot be validated the way the service would run it: %w", u.Name, err)
		}
		if a == nil {
			continue // plain root: the identity install already has
		}
		byUnit[u.Name] = a
		if a.UID != 0 && firstUser == nil {
			firstUser = a
		}
	}
	inspect, how := invoker(getenv, lk.account)
	if inspect == nil && firstUser != nil {
		inspect, how = firstUser, "the service user"
	}
	p.runAs = inspect
	if inspect == nil {
		p.RunAs = "root (install was not run through sudo and no service user is known)"
	} else {
		p.RunAs = inspect.String() + ", " + how
	}
	for i := range p.Validations {
		v := &p.Validations[i]
		switch a, ok := byUnit[v.From]; {
		case ok:
			v.account, v.User = a, a.Name
			if a.UID == 0 {
				v.User = "root"
			}
		case v.From == "--config" || dynamic[v.From]:
			v.account, v.User = inspect, "root"
			if inspect != nil {
				v.User = inspect.Name
			}
		default:
			v.User = "root" // the unit runs as root
		}
	}
	return nil, nil
}

func unitNamed(units []systemd.Unit, name string) (systemd.Unit, bool) {
	for _, u := range units {
		if u.Name == name {
			return u, true
		}
	}
	return systemd.Unit{}, false
}

// unitAccount resolves the account a unit's processes run as, the way
// systemd does (src/core/exec-invoke.c, get_supplementary_groups, v255):
// with User= set, that account, root included, with its primary group (or
// Group=), SupplementaryGroups=, and its groups from the group database
// unless the effective group is 0, for which systemd skips initgroups;
// without User=, root with Group= when set and exactly
// SupplementaryGroups=, nothing from the group database. nil means plain
// root with nothing set, which needs no switching. An explicit User=root
// is therefore not nil: systemd sets HOME and LOGNAME for it, since
// set_user_login_env is true whenever User= is set.
func unitAccount(u systemd.Unit, lk lookups) (*caddybin.Account, error) {
	if u.User != "" {
		a, err := lk.account(u.User, u.Group, u.SupplementaryGroups)
		if err != nil {
			return nil, err
		}
		if a.UID == 0 && a.GID == 0 {
			if a.Groups, err = groupIDs(u.SupplementaryGroups, lk); err != nil {
				return nil, err
			}
		}
		return a, nil
	}
	if u.Group == "" && len(u.SupplementaryGroups) == 0 {
		return nil, nil
	}
	a, err := lk.account("0", u.Group, nil)
	if err != nil {
		return nil, err
	}
	if a.Groups, err = groupIDs(u.SupplementaryGroups, lk); err != nil {
		return nil, err
	}
	return a, nil
}

// groupIDs resolves SupplementaryGroups= names to IDs.
func groupIDs(names []string, lk lookups) ([]uint32, error) {
	var ids []uint32
	for _, g := range names {
		gid, err := lk.group(g)
		if err != nil {
			return nil, err
		}
		ids = append(ids, gid)
	}
	return ids, nil
}

// invoker is the user who ran sudo, from the SUDO_UID, SUDO_GID and
// SUDO_USER variables sudo sets, or nil when install was not run through
// sudo, or was run through sudo by root. The account is looked up for its
// groups and home; when that fails (a user known only to a directory
// service, which os/user built without cgo cannot ask) the IDs sudo gave
// are used on their own.
func invoker(getenv func(string) string, lookup func(name, group string, extra []string) (*caddybin.Account, error)) (*caddybin.Account, string) {
	uid, err := strconv.ParseUint(getenv("SUDO_UID"), 10, 32)
	gid, gerr := strconv.ParseUint(getenv("SUDO_GID"), 10, 32)
	if err != nil || gerr != nil || uid == 0 {
		return nil, ""
	}
	a, err := lookup(strconv.FormatUint(uid, 10), "", nil)
	if err != nil {
		name := getenv("SUDO_USER")
		if name == "" {
			name = strconv.FormatUint(uid, 10)
		}
		a = &caddybin.Account{Name: name, UID: uint32(uid), GID: uint32(gid)}
	}
	return a, "the user who ran sudo"
}

// checkWorkDirs refuses a validation whose working directory is the unit
// user's home but could not be resolved (systemd left it as "~" and the
// user lookup failed): run anywhere else, a relative config path or a
// relative path inside the config would be checked against the wrong
// files, and validate would then be an answer to a different question.
func checkWorkDirs(vals []Validation) error {
	for _, v := range vals {
		if v.WorkDir == systemd.UnresolvedHome {
			return fmt.Errorf("%s runs in the home directory of its user, which could not be looked up, so %s cannot be validated where the service would read it; pass --config with an absolute path to validate explicitly", v.From, v.Config)
		}
	}
	return nil
}

// buildSelectionFlags names the build-selection options that are set, for
// the error that rejects them alongside --from.
func buildSelectionFlags(b build.Options) string {
	var set []string
	if b.CaddyVersion != "" {
		set = append(set, "--caddy-version")
	}
	if len(b.Upgrade) > 0 {
		set = append(set, "--upgrade")
	}
	if b.UpgradeAll {
		set = append(set, "--upgrade-all")
	}
	if len(b.With) > 0 {
		set = append(set, "--with")
	}
	if len(b.Replace) > 0 {
		set = append(set, "--replace")
	}
	if len(b.DropReplace) > 0 {
		set = append(set, "--drop-replace")
	}
	if b.AllowMajor {
		set = append(set, "--allow-major")
	}
	return strings.Join(set, ", ")
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
	leftovers := "check what the package manager left behind (unit file, service user, config)"
	if owner != nil && owner.Manager == "dpkg" {
		// Verified against Ubuntu's caddy maintainer scripts: remove masks the
		// unit, never deletes the user, and only purge deletes /etc/caddy.
		leftovers = "on Debian and Ubuntu this leaves " + owner.Package + ".service masked and keeps the caddy user and /etc/caddy; purge would delete /etc/caddy"
	}
	return fmt.Errorf(`%s. install never writes over a system package: the next package upgrade would silently undo it.

To switch to a custom build:
  1. uninstall the system package first: %s
     (%s)
  2. systemctl unmask the unit if it was masked, then recreate the unit per
     https://caddyserver.com/docs/running#manual-installation, skipping any
     step that already exists (the service user usually does)
  3. run: sudo upgrade-caddy install --fresh --target %s --with <plugin> ...`, what, remove, leftovers, target)
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
// restartCount is how many units will be restarted and unitNames their
// comma-joined names for the message.
func needsRoot(euid int, canWriteDir bool, dir string, restartCount int, unitNames string, hasCaps bool, extra []string) (bool, []string) {
	var reasons []string
	if !canWriteDir {
		reasons = append(reasons, "write to "+dir)
	}
	if restartCount > 0 {
		reasons = append(reasons, "restart "+unitNames)
	}
	if hasCaps {
		reasons = append(reasons, "re-apply file capabilities")
	}
	reasons = append(reasons, extra...)
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

// describeCaps renders the binary's file capabilities for the plan, using
// getcap when it is installed and "present" otherwise. The raw value that is
// actually carried over comes from readCaps and does not depend on getcap.
func describeCaps(ctx context.Context, path string) string {
	if _, err := exec.LookPath("getcap"); err == nil {
		if out, err := exec.CommandContext(ctx, "getcap", path).Output(); err == nil {
			if text := parseCaps(string(out)); text != "" {
				return text
			}
		}
	}
	return "present"
}

// parseCaps extracts the capability text from getcap output. Newer libcap
// prints "/usr/bin/caddy cap_net_bind_service=ep", older prints
// "/usr/bin/caddy = cap_net_bind_service+ep".
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

// recheck repeats the plan's safety checks right before the install acts
// on it. Resolve ran before the build, possibly minutes ago; in between a
// package could have put its own Caddy at the target, the file could have
// been replaced, its capabilities changed, or a unit added. Any difference
// from the plan is a refusal: the user re-runs install with a fresh plan.
func recheck(ctx context.Context, plan *Plan) error {
	requested := plan.Requested
	if requested == "" {
		requested = plan.Target
	}
	real, exists, err := resolveTarget(requested)
	if err != nil {
		return fmt.Errorf("rechecking the target: %w", err)
	}
	changed := func(what string) error {
		return fmt.Errorf("%s changed while the plan was being carried out; nothing was changed, re-run install", what)
	}
	if real != plan.Target || exists != plan.TargetExists {
		return changed(fmt.Sprintf("the target %s", requested))
	}
	if exists {
		owner, err := pkgmgr.Find(ctx, plan.Target)
		if err != nil {
			return fmt.Errorf("refusing to continue: %w; install must know whether %s belongs to a system package before replacing it", err, plan.Target)
		}
		if owner != nil {
			return refuseSystemPackage(plan.Target, owner)
		}
		id, err := identify(plan.Target)
		if err != nil {
			return fmt.Errorf("rechecking the target: %w", err)
		}
		if id != plan.targetID {
			return changed(fmt.Sprintf("the file at %s", plan.Target))
		}
		caps, err := readCaps(plan.Target)
		if err != nil {
			return err
		}
		if !bytes.Equal(caps, plan.caps) {
			return changed(fmt.Sprintf("the file capabilities of %s", plan.Target))
		}
	}
	units, err := systemd.UnitsUsing(ctx, plan.Target)
	if err != nil {
		return fmt.Errorf("querying systemd: %w", err)
	}
	if !sameUnits(plan.Units, units) {
		return changed(fmt.Sprintf("the units running %s, or their config flags, working directory or user (planned: %s; now: %s)", plan.Target, unitNamesOrNone(plan.Units), unitNamesOrNone(units)))
	}
	return nil
}

// sameUnits reports whether two unit lists name the same units in order
// with the same validation inputs: working directory, the account the unit
// runs as, its Environment=, EnvironmentFile= and ExecSearchPath=, and the
// --config, --adapter and --envfile flags. A unit whose
// config moved while the build ran would otherwise be validated against the
// old one and restarted on the new one unvalidated, and one whose User=
// changed would be validated as the wrong account.
func sameUnits(a, b []systemd.Unit) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ca, aa, ea := a[i].ConfigArgs()
		cb, ab, eb := b[i].ConfigArgs()
		if a[i].Name != b[i].Name || a[i].WorkingDirectory != b[i].WorkingDirectory ||
			a[i].User != b[i].User || a[i].Group != b[i].Group || a[i].DynamicUser != b[i].DynamicUser ||
			!slices.Equal(a[i].SupplementaryGroups, b[i].SupplementaryGroups) ||
			envKey(a[i]) != envKey(b[i]) ||
			ca != cb || aa != ab || !slices.Equal(ea, eb) {
			return false
		}
	}
	return true
}

func unitNamesOrNone(units []systemd.Unit) string {
	if len(units) == 0 {
		return "none"
	}
	return unitNames(units)
}

// checkStaged repeats on the staged pair every check Resolve made on the
// --from pair: a distribution build is refused, since a lockfile with empty
// identities would otherwise describe a binary with no module information,
// and the lockfile must describe the binary.
func checkStaged(lockPath string, info *caddybin.Info) error {
	if info.IsDistroBuild() {
		return fmt.Errorf("the staged binary %s has no Go module information; not a build output", info.Path)
	}
	if err := attest(lockPath, info); err != nil {
		return fmt.Errorf("the staged lockfile does not describe the staged binary: %w", err)
	}
	return nil
}

// attest checks that the lockfile at lockPath describes the inspected
// binary, so the attestation installed beside the target is for the file
// actually installed.
func attest(lockPath string, info *caddybin.Info) error {
	lf, err := build.ReadLockfile(lockPath)
	if err != nil {
		return err
	}
	return lf.Describes(info)
}

// stage copies a --from binary and its lockfile into the target's
// directory so the final rename cannot cross filesystems. Both files are
// created with unpredictable names and O_EXCL and written through the open
// handles, so a path planted in a shared directory beforehand (a symlink,
// say) is never followed by a privileged install.
func stage(from, target string) (binPath, lockPath string, err error) {
	dir := filepath.Dir(target)
	base := filepath.Base(target)
	binPath, err = copyToTemp(from, dir, "."+base+"-install-*", 0o755)
	if err != nil {
		return "", "", fmt.Errorf("staging %s: %w", from, err)
	}
	lockPath, err = copyToTemp(from+".lock.json", dir, "."+base+"-install-*.lock.json", 0o644)
	if err != nil {
		os.Remove(binPath)
		return "", "", fmt.Errorf("staging lockfile: %w", err)
	}
	return binPath, lockPath, nil
}

// copyToTemp copies src into a new exclusively created file in dir and
// returns its path.
func copyToTemp(src, dir, pattern string, mode os.FileMode) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	_, copyErr := io.Copy(out, in)
	modeErr := out.Chmod(mode)
	// Close can report a write failure of its own, so its error matters on
	// both paths, not just the successful one.
	if err := errors.Join(copyErr, modeErr, out.Close()); err != nil {
		os.Remove(out.Name())
		return "", err
	}
	return out.Name(), nil
}

// validate runs `<new binary> validate` against one config.
func validate(ctx context.Context, bin string, v Validation, logw io.Writer, verbose bool) error {
	args := []string{"validate", "--config", v.Config}
	if v.Adapter != "" {
		args = append(args, "--adapter", v.Adapter)
	}
	for _, f := range v.EnvFiles {
		args = append(args, "--envfile", f)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = v.WorkDir
	if v.unit != nil {
		env, err := unitEnviron(ctx, *v.unit, v.account)
		if err != nil {
			return fmt.Errorf("%s: %w; its config cannot be validated with the environment the service gets", v.From, err)
		}
		cmd.Env = env
	}
	if err := v.account.Apply(cmd); err != nil {
		return err
	}
	out, err := cmd.CombinedOutput()
	if verbose || err != nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			fmt.Fprintf(logw, "    %s\n", line)
		}
	}
	if err != nil {
		return fmt.Errorf("the new binary rejected %s: %w; nothing was changed", v.Config, err)
	}
	return nil
}

// unitEnviron builds the environment validate runs in for a unit's config:
// what systemd would give the service rather than what install inherited
// from the operator's shell, so {env.*} placeholders expand to the values
// the service sees. In systemd's order (build_environment and the
// strv_env_merge call in src/core/exec-invoke.c, v255, later entries
// winning): first what systemd sets itself, PATH (ExecSearchPath= when set,
// else systemd's default search path), USER (always; root when the unit
// sets no User=) and, for a unit with User= or DynamicUser=, LOGNAME and
// HOME; then Environment= and every EnvironmentFile= (systemd.LoadEnvironment),
// so an explicit assignment of any of those wins, as it does for the
// service. The files are read now rather than at Resolve so an edit
// during the build is seen. Not applied: SHELL (os/user does not expose
// it), the manager's own environment, PassEnvironment= and
// UnsetEnvironment=, which are rare on a web server unit and documented
// as such.
func unitEnviron(ctx context.Context, u systemd.Unit, account *caddybin.Account) ([]string, error) {
	path := u.ExecSearchPath
	if len(path) == 0 {
		path = systemd.DefaultSearchPath(ctx)
	}
	user, home, err := envIdentity(u, account)
	if err != nil {
		return nil, err
	}
	env := []string{"PATH=" + strings.Join(path, ":"), "USER=" + user}
	if u.User != "" || u.DynamicUser {
		env = append(env, "LOGNAME="+user)
		if home != "" {
			env = append(env, "HOME="+home)
		}
	}
	vars, err := systemd.LoadEnvironment(u)
	if err != nil {
		return nil, err
	}
	return systemd.MergeEnv(env, vars), nil
}

// envIdentity is the user name and home directory systemd puts in a
// service's USER, LOGNAME and HOME, which is not always the account the
// validation runs as. A DynamicUser= unit is validated as the inspection
// account, because its own account exists only while it runs, but its
// processes see the dynamic user: User= when set, else the unit name's
// prefix when that is a valid user name (user_from_unit_name in
// src/core/unit.c, v255), with home "/" (build_user_json in
// src/core/core-varlink.c). A prefix that is not a valid name gets a hash
// of itself as name, which is not reproduced here; that unit is refused
// rather than validated under a made-up name. A unit with User= takes the
// account's name and home, and one without runs as root.
func envIdentity(u systemd.Unit, account *caddybin.Account) (user, home string, err error) {
	switch {
	case u.DynamicUser:
		name := u.User
		if name == "" {
			name = systemd.UnitPrefix(u.Name)
			if !systemd.ValidUserName(name) {
				return "", "", fmt.Errorf("%s runs as a dynamic user whose name systemd derives by hashing %q, which is not reproduced here; set User= on the unit to validate its config with the environment the service gets", u.Name, name)
			}
		}
		return name, "/", nil
	case u.User != "" && account != nil:
		return account.Name, account.Home, nil
	case u.User != "":
		return u.User, "", nil
	}
	return "root", "", nil
}

// swapLockfile installs newLock as <target>.lock.json, keeping any existing
// lockfile as <target>.lock.json.previous so a rollback can restore it.
func swapLockfile(target, newLock string) error {
	lock := target + ".lock.json"
	prev := lock + ".previous"
	// A stale rollback copy that cannot be cleared would be taken for the
	// previous lockfile by a later rollback, so the install stops here,
	// before the live lockfile moves.
	if err := os.Remove(prev); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing stale %s: %w", prev, err)
	}
	// Only "not there" means there is nothing to keep; any other failure
	// to look would let the rename below overwrite a lockfile with no
	// previous copy, so it stops the install instead.
	hadOld := false
	switch _, err := os.Stat(lock); {
	case err == nil:
		if err := os.Rename(lock, prev); err != nil {
			return fmt.Errorf("keeping previous lockfile: %w", err)
		}
		hadOld = true
	case errors.Is(err, os.ErrNotExist):
	default:
		return fmt.Errorf("checking %s before replacing it: %w", lock, err)
	}
	if err := os.Rename(newLock, lock); err != nil {
		if hadOld {
			// Put the old one back, and say so if that fails too: the
			// live lockfile would then be stranded at .previous.
			if rbErr := os.Rename(prev, lock); rbErr != nil {
				return errors.Join(fmt.Errorf("installing the new lockfile: %w; and the previous one could not be put back, it is at %s", err, prev), rbErr)
			}
		}
		return err
	}
	return nil
}

// rollbackLockfile undoes swapLockfile: the previous lockfile comes back
// if there was one, otherwise the new one is removed.
func rollbackLockfile(target string) error {
	lock := target + ".lock.json"
	prev := lock + ".previous"
	switch _, err := os.Stat(prev); {
	case err == nil:
		return os.Rename(prev, lock)
	case errors.Is(err, os.ErrNotExist):
		// No previous lockfile: this was a first install, so the new one
		// is removed again.
	default:
		// Not knowing whether a previous lockfile exists must not turn
		// into deleting the one in place.
		return fmt.Errorf("checking for %s: %w", prev, err)
	}
	if err := os.Remove(lock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// swap puts newPath at target with no moment where nothing is there. The
// current file is hard-linked to <target>.previous first, so a rollback
// copy exists before the atomic rename replaces the target. Mode is copied
// from the old file, and ownership too when the caller is allowed to chown
// (root); the raw file-capability attribute is re-applied. It returns the
// rollback path, or "" when there was no previous file.
func swap(target, newPath string, caps []byte) (previous string, err error) {
	if fi, err := os.Lstat(target); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%s is a symlink; install replaces the file it points to, so the target must be resolved first", target)
	}
	// Only "not there" is a first install. Any other failure to look at
	// the target (permission, I/O) must not skip the rollback copy and
	// let the rename below replace a binary that cannot be restored.
	old, err := os.Stat(target)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("checking %s before replacing it: %w", target, err)
	}
	if err == nil {
		// Owner first: chown clears set-ID bits, so the mode goes on
		// afterwards, and with every bit os.Chmod accepts, not just rwx,
		// so setuid, setgid and sticky survive the swap.
		// Root must succeed: a failure there (a root-squashed export, a
		// restricted user namespace) would install a binary with a
		// different owner while reporting success. An unprivileged user
		// cannot hand a file to another owner at all, so for them a
		// permission error is the expected outcome, not a failure.
		if st, ok := old.Sys().(*syscall.Stat_t); ok {
			if err := os.Chown(newPath, int(st.Uid), int(st.Gid)); err != nil && (os.Geteuid() == 0 || !errors.Is(err, os.ErrPermission)) {
				return "", fmt.Errorf("setting owner of new binary: %w", err)
			}
		}
		if err := os.Chmod(newPath, old.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky)); err != nil {
			return "", err
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
	if caps != nil {
		if err := writeCaps(target, caps); err != nil {
			restoreErr, keepErr := rollback(target, previous)
			return "", errors.Join(fmt.Errorf("re-applying file capabilities: %w; %s", err, rolledBack(restoreErr, keepErr)), restoreErr, keepErr)
		}
	}
	return previous, nil
}

// rollback puts the previous binary back. The failed one is kept as
// <target>.failed for inspection when that is possible, but restoring the
// target never waits on it. The two outcomes are returned separately:
// restoreErr says whether the target holds the previous binary again,
// which is what an operator needs to know; keepErr only says the
// diagnostic copy could not be made.
func rollback(target, previous string) (restoreErr, keepErr error) {
	if previous == "" {
		return errors.New("no previous binary to roll back to"), nil
	}
	failed := target + ".failed"
	os.Remove(failed)
	if err := os.Link(target, failed); err != nil {
		keepErr = fmt.Errorf("could not keep the failed binary as %s: %w", failed, err)
	}
	if err := os.Rename(previous, target); err != nil {
		return fmt.Errorf("rollback: restoring %s: %w", target, err), keepErr
	}
	return nil, keepErr
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
	if err != nil {
		return fmt.Errorf("%s is active but its main process could not be determined: %w", unit, err)
	}
	if pid == 0 {
		return nil // no main process to inspect; active is the best we can confirm
	}
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if errors.Is(err, os.ErrPermission) {
		return nil // unprivileged; being active is the best we can confirm
	}
	if err != nil {
		// A vanished process (ENOENT) or anything else is not a healthy
		// restart, however active systemd said the unit was a moment ago.
		return fmt.Errorf("%s is active but its main process %d could not be inspected: %w", unit, pid, err)
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
	if p.Requested != "" {
		fmt.Fprintf(w, " (resolved from symlink %s)", p.Requested)
	}
	if p.Installed != nil {
		fmt.Fprintf(w, " (%s, %d plugins)", p.Installed.MainVersion, len(p.Installed.Plugins))
	} else if !p.TargetExists {
		fmt.Fprint(w, " (nothing installed yet)")
	}
	fmt.Fprintln(w)
	switch {
	case p.NoSystemd:
		fmt.Fprintln(w, "Service:  systemctl not found; no service can be found or restarted")
	case len(p.Units) == 0:
		fmt.Fprintln(w, "Service:  none runs this binary; nothing will be restarted")
	}
	for _, u := range p.Units {
		fmt.Fprintf(w, "Service:  %s (%s/%s)\n", u.Name, u.ActiveState, u.SubState)
	}
	if len(p.Validations) == 0 {
		fmt.Fprintln(w, "Validate: skipped, no config known; pass --config to validate")
	}
	for _, v := range p.Validations {
		from := v.From
		if v.User != "" {
			from += ", as " + v.User
		}
		if n := len(v.UnitEnvironment) + len(v.UnitEnvFiles); n > 0 {
			from += fmt.Sprintf(", with the unit's environment (%d Environment= and %d EnvironmentFile= settings)", len(v.UnitEnvironment), len(v.UnitEnvFiles))
		}
		fmt.Fprintf(w, "Validate: %s (from %s)\n", v.Config, from)
	}
	if p.RunAs != "" {
		fmt.Fprintf(w, "Run as:   %s, to inspect the new binary\n", p.RunAs)
	}
	if p.FileCapabilities != "" {
		fmt.Fprintf(w, "Caps:     %s (will be re-applied)\n", p.FileCapabilities)
	}
	if p.NeedsRoot {
		fmt.Fprintf(w, "Root:     required to %s\n", strings.Join(p.RootReasons, " and "))
	}
	if p.From != "" {
		fmt.Fprintf(w, "From:     %s (%s, %d plugins)\n", p.From, p.FromInfo.MainVersion, len(p.FromInfo.Plugins))
		return
	}
	fmt.Fprintln(w, "Build:")
	p.BuildPlan.WriteText(indent{w})
}

type indent struct{ w io.Writer }

func (i indent) Write(b []byte) (int, error) {
	s := strings.TrimRight(string(b), "\n")
	for _, line := range strings.Split(s, "\n") {
		fmt.Fprintf(i.w, "  %s\n", line)
	}
	return len(b), nil
}
