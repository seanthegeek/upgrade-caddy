// Command upgrade-caddy checks, builds and installs custom Caddy binaries
// using the plugin set already compiled into the installed one.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/seanthegeek/upgrade-caddy/internal/build"
	"github.com/seanthegeek/upgrade-caddy/internal/check"
	"github.com/seanthegeek/upgrade-caddy/internal/install"
)

// version is the release version. Release builds may override it with
// `-ldflags "-X main.version=X.Y.Z"`; a `go install ...@vX.Y.Z` build
// reports the module version from build info instead.
var version = "0.1.0"

// Exit codes.
const (
	exitOK      = 0
	exitError   = 1
	exitUpdates = 2 // check: at least one component is outdated
)

const usage = `usage: upgrade-caddy <command> [flags]

Commands:
  check     report whether Caddy or any compiled-in plugin is out of date
  build     build an updated Caddy with the same plugins at the same versions
  install   build, validate against the live config, swap the binary and restart the service
  version   print the tool's version

Run "upgrade-caddy <command> -h" for flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(exitError)
	}
	// SIGTERM is what service managers, CI cancellation and timeout(1)
	// send; it must cancel the context like Ctrl-C so install can roll back
	// instead of dying mid-swap.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var code int
	switch os.Args[1] {
	case "check":
		code = runCheck(ctx, os.Args[2:])
	case "build":
		code = runBuild(ctx, os.Args[2:])
	case "install":
		code = runInstall(ctx, os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println(versionString())
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		code = exitError
	}
	os.Exit(code)
}

func runCheck(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	binary := fs.String("binary", "", "path to the caddy binary (default: first caddy on PATH)")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	timeout := fs.Duration("timeout", 60*time.Second, "overall timeout for version lookups")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: upgrade-caddy check [flags]")
		fmt.Fprintln(fs.Output(), "\nExit status is 0 when everything is current, 2 when updates are available, 1 on error.")
		fmt.Fprintln(fs.Output(), "A newer major version counts as an available update: Caddy does not backport fixes to older majors.")
		fs.PrintDefaults()
	}
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	report, err := check.Run(ctx, check.Options{Binary: *binary})
	if err != nil {
		fmt.Fprintln(os.Stderr, "upgrade-caddy check:", err)
		return exitError
	}
	if *asJSON {
		if err := report.WriteJSON(os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "upgrade-caddy check:", err)
			return exitError
		}
	} else {
		report.WriteText(os.Stdout)
	}
	if report.HasErrors {
		fmt.Fprintln(os.Stderr, "upgrade-caddy check: some components could not be checked; see the STATUS column")
		return exitError
	}
	if report.UpdatesAvailable {
		return exitUpdates
	}
	return exitOK
}

// parseFlags parses args and maps the outcome to an exit code: ok is false
// when the command should stop, with code 0 after -h and 1 after a usage
// error, including leftover positional arguments. flag.ExitOnError would
// exit 2, which check uses to mean "updates available".
func parseFlags(fs *flag.FlagSet, args []string) (code int, ok bool) {
	switch err := fs.Parse(args); {
	case err == nil:
	case errors.Is(err, flag.ErrHelp):
		return exitOK, false
	default:
		return exitError, false
	}
	// No command takes positional arguments; a stray one is most likely a
	// path meant for a flag, and silently ignoring it would make the
	// command act on the wrong binary.
	if fs.NArg() > 0 {
		fmt.Fprintf(fs.Output(), "unexpected argument(s): %s (every option is a flag, e.g. --target PATH)\n", strings.Join(fs.Args(), " "))
		fs.Usage()
		return exitError, false
	}
	return exitOK, true
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func runBuild(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	var opts build.Options
	var upgrade, with, replace multiFlag
	fs.StringVar(&opts.Binary, "binary", "", "installed caddy binary to reproduce (default: first caddy on PATH)")
	fs.BoolVar(&opts.Fresh, "fresh", false, "ignore any installed binary; the plugin set is only what --with gives")
	fs.StringVar(&opts.Output, "output", "caddy", "where to write the new binary")
	fs.StringVar(&opts.CaddyVersion, "caddy-version", "", "Caddy version to build (default: latest within the installed major)")
	fs.Var(&upgrade, "upgrade", "plugin to bump to its latest version within its major, by Go module path or Caddy module ID (repeatable)")
	fs.BoolVar(&opts.UpgradeAll, "upgrade-all", false, "bump every plugin to its latest version within its major")
	fs.Var(&with, "with", "module[@version] to add, or whose version to override (repeatable)")
	fs.Var(&replace, "replace", "old=new module replacement passed to xcaddy (repeatable)")
	fs.BoolVar(&opts.AllowMajor, "allow-major", false, "permit --caddy-version or --with to change a major version")
	fs.BoolVar(&opts.DryRun, "dry-run", false, "resolve and print the plan without building")
	fs.BoolVar(&opts.Verbose, "verbose", false, "stream all xcaddy and go output")
	timeout := fs.Duration("timeout", 20*time.Minute, "overall timeout for the build")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: upgrade-caddy build [flags]")
		fmt.Fprintln(fs.Output(), "\nReproduces the installed binary's plugin set at the same versions with the latest Caddy.")
		fmt.Fprintln(fs.Output(), "Plugin versions only change with --upgrade, --upgrade-all or --with. Major versions are never crossed without --allow-major.")
		fmt.Fprintln(fs.Output(), "Writes <output>.lock.json recording the version and checksum of Caddy and every plugin.")
		fs.PrintDefaults()
	}
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	opts.Upgrade, opts.With, opts.Replace = upgrade, with, replace
	opts.Log = os.Stderr
	opts.TimeoutBuild = *timeout
	opts.OnPlan = func(p *build.Plan) {
		p.WriteText(os.Stdout)
		fmt.Fprintln(os.Stdout)
	}

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	_, res, err := build.Run(ctx, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "upgrade-caddy build:", err)
		return exitError
	}
	if opts.DryRun {
		fmt.Fprintln(os.Stdout, "Dry run: nothing built.")
		return exitOK
	}
	fmt.Fprintf(os.Stdout, "Built %s (%s, %d plugins)\nLockfile %s\n", res.Output, res.Built.MainVersion, len(res.Built.Plugins), res.Lockfile)
	return exitOK
}

func runInstall(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	var opts install.Options
	var upgrade, with, replace multiFlag
	fs.StringVar(&opts.Target, "target", "", "binary to reproduce and replace (default: first caddy on PATH)")
	fs.StringVar(&opts.From, "from", "", "install this binary, produced by 'upgrade-caddy build', instead of building")
	fs.BoolVar(&opts.Fresh, "fresh", false, "nothing is installed yet; build from --with only (requires --target)")
	fs.StringVar(&opts.Config, "config", "", "config to validate against (default: the service's --config)")
	fs.BoolVar(&opts.NoRestart, "no-restart", false, "swap the binary but do not restart the service")
	fs.StringVar(&opts.Build.CaddyVersion, "caddy-version", "", "Caddy version to build (default: latest within the installed major)")
	fs.Var(&upgrade, "upgrade", "plugin to bump to its latest version within its major, by Go module path or Caddy module ID (repeatable)")
	fs.BoolVar(&opts.Build.UpgradeAll, "upgrade-all", false, "bump every plugin to its latest version within its major")
	fs.Var(&with, "with", "module[@version] to add, or whose version to override (repeatable)")
	fs.Var(&replace, "replace", "old=new module replacement passed to xcaddy (repeatable)")
	fs.BoolVar(&opts.Build.AllowMajor, "allow-major", false, "permit --caddy-version or --with to change a major version")
	fs.BoolVar(&opts.DryRun, "dry-run", false, "resolve and print the plan without changing anything")
	fs.BoolVar(&opts.Verbose, "verbose", false, "stream all build and validation output")
	timeout := fs.Duration("timeout", 20*time.Minute, "overall timeout")
	fs.DurationVar(&opts.RestartWait, "restart-wait", 15*time.Second, "how long to wait for the service to become active after restart")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: upgrade-caddy install [flags]")
		fmt.Fprintln(fs.Output(), "\nBuilds (or takes --from) a new Caddy, validates it against the live config, keeps the current")
		fmt.Fprintln(fs.Output(), "binary as <target>.previous, swaps atomically, restarts the systemd unit that runs it and rolls")
		fmt.Fprintln(fs.Output(), "back if the unit does not come up. Never installs over a system package.")
		fs.PrintDefaults()
	}
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	opts.Build.Upgrade, opts.Build.With, opts.Build.Replace = upgrade, with, replace
	opts.Build.TimeoutBuild = *timeout
	opts.Log = os.Stderr
	opts.OnPlan = func(p *install.Plan) {
		p.WriteText(os.Stdout)
		fmt.Fprintln(os.Stdout)
	}

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	_, res, err := install.Run(ctx, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "upgrade-caddy install:", err)
		return exitError
	}
	if opts.DryRun {
		fmt.Fprintln(os.Stdout, "Dry run: nothing changed.")
		return exitOK
	}
	fmt.Fprintf(os.Stdout, "Installed %s (%s, %d plugins)\n", res.Target, res.Built.MainVersion, len(res.Built.Plugins))
	if len(res.Validated) > 0 {
		fmt.Fprintf(os.Stdout, "Validated %s\n", strings.Join(res.Validated, ", "))
	}
	if res.Previous != "" {
		fmt.Fprintf(os.Stdout, "Previous  %s\n", res.Previous)
	}
	if res.Lockfile != "" {
		fmt.Fprintf(os.Stdout, "Lockfile  %s\n", res.Lockfile)
	}
	if len(res.Restarted) > 0 {
		fmt.Fprintf(os.Stdout, "Restarted %s\n", strings.Join(res.Restarted, ", "))
	}
	return exitOK
}

// versionString is "upgrade-caddy 0.1.0 (go1.22.2 linux/amd64)", with the
// commit and a "modified" marker added when the build was stamped with
// version-control information.
func versionString() string {
	v := version
	var extra []string
	if bi, ok := debug.ReadBuildInfo(); ok {
		if mv := bi.Main.Version; mv != "" && mv != "(devel)" {
			v = strings.TrimPrefix(mv, "v")
		}
		var rev, modified string
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				modified = s.Value
			}
		}
		if len(rev) > 12 {
			rev = rev[:12]
		}
		if rev != "" {
			if modified == "true" {
				rev += "-modified"
			}
			extra = append(extra, "commit "+rev)
		}
	}
	extra = append(extra, runtime.Version()+" "+runtime.GOOS+"/"+runtime.GOARCH)
	return fmt.Sprintf("upgrade-caddy %s (%s)", v, strings.Join(extra, ", "))
}
