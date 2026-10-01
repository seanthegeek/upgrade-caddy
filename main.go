// Command upgrade-caddy checks, builds and installs custom Caddy binaries
// using the plugin set already compiled into the installed one.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

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
  build     build an updated Caddy with the same plugins (not implemented)
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
	case "build", "install":
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
