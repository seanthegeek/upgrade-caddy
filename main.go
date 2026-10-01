// Command upgrade-caddy checks, builds and installs custom Caddy binaries
// using the plugin set already compiled into the installed one.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/seanthegeek/upgrade-caddy/internal/build"
	"github.com/seanthegeek/upgrade-caddy/internal/check"
)

// Exit codes.
const (
	ExitOK      = 0
	ExitError   = 1
	ExitUpdates = 2 // check: at least one component is outdated
)

const usage = `usage: upgrade-caddy <command> [flags]

Commands:
  check     report whether Caddy or any compiled-in plugin is out of date
  build     build an updated Caddy with the same plugins at the same versions
  install   build, validate, swap the binary and restart the service (not implemented)

Run "upgrade-caddy <command> -h" for flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(ExitError)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var code int
	switch os.Args[1] {
	case "check":
		code = runCheck(ctx, os.Args[2:])
	case "build":
		code = runBuild(ctx, os.Args[2:])
	case "install":
		fmt.Fprintf(os.Stderr, "upgrade-caddy %s: not implemented yet\n", os.Args[1])
		code = ExitError
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		code = ExitError
	}
	os.Exit(code)
}

func runCheck(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	binary := fs.String("binary", "", "path to the caddy binary (default: first caddy on PATH)")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	timeout := fs.Duration("timeout", 60*time.Second, "overall timeout for version lookups")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: upgrade-caddy check [flags]")
		fmt.Fprintln(fs.Output(), "\nExit status is 0 when everything is current, 2 when updates are available, 1 on error.")
		fmt.Fprintln(fs.Output(), "A newer major version counts as an available update: Caddy does not backport fixes to older majors.")
		fs.PrintDefaults()
	}
	fs.Parse(args)

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	report, err := check.Run(ctx, check.Options{Binary: *binary})
	if err != nil {
		fmt.Fprintln(os.Stderr, "upgrade-caddy check:", err)
		return ExitError
	}
	if *asJSON {
		if err := report.WriteJSON(os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "upgrade-caddy check:", err)
			return ExitError
		}
	} else {
		report.WriteText(os.Stdout)
	}
	if report.UpdatesAvailable() {
		return ExitUpdates
	}
	return ExitOK
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func runBuild(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
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
		fmt.Fprintln(fs.Output(), "Writes <output>.lock.json recording every module version and checksum in the build.")
		fs.PrintDefaults()
	}
	fs.Parse(args)
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
		return ExitError
	}
	if opts.DryRun {
		fmt.Fprintln(os.Stdout, "Dry run: nothing built.")
		return ExitOK
	}
	fmt.Fprintf(os.Stdout, "Built %s (%s, %d plugins)\nLockfile %s\n", res.Output, res.Built.MainVersion, len(res.Built.Plugins), res.Lockfile)
	return ExitOK
}
