// Package check implements the `check` command: is the installed Caddy, or
// any plugin compiled into it, behind the latest published version?
package check

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"text/tabwriter"

	"github.com/seanthegeek/upgrade-caddy/internal/caddybin"
	"github.com/seanthegeek/upgrade-caddy/internal/goproxy"
	"github.com/seanthegeek/upgrade-caddy/internal/semver"
	"github.com/seanthegeek/upgrade-caddy/internal/systemd"
)

// Options controls a check run.
type Options struct {
	Binary string // explicit path, or "" to search PATH
	Proxy  *goproxy.Client
}

// Major describes a newer major version living at a different module path.
type Major struct {
	Package string `json:"package"`
	Version string `json:"version"`
}

// Status of one versioned component.
type Status struct {
	Name      string `json:"name"`
	Package   string `json:"package,omitempty"`
	Installed string `json:"installed"`
	Latest    string `json:"latest,omitempty"`
	Outdated  bool   `json:"outdated"`
	Pseudo    bool   `json:"pseudo_version"` // installed is pinned to an untagged commit
	Major     *Major `json:"major_available,omitempty"`
	Note      string `json:"note,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Report is the result of a check.
type Report struct {
	Binary   *caddybin.Info `json:"binary"`
	Caddy    Status         `json:"caddy"`
	Plugins  []Status       `json:"plugins"`
	Services []systemd.Unit `json:"services"`
	Warnings []string       `json:"warnings"`

	mu sync.Mutex // guards Warnings during concurrent lookups
}

// UpdatesAvailable reports whether Caddy or any plugin is behind. A newer
// major version always counts: Caddy has never backported security fixes
// to a previous major, so staying on one is a risk even though build will
// not cross it without being told to.
func (r *Report) UpdatesAvailable() bool {
	all := append([]Status{r.Caddy}, r.Plugins...)
	for _, s := range all {
		if s.Outdated || s.Major != nil {
			return true
		}
	}
	return false
}

func (r *Report) warn(msg string) {
	r.mu.Lock()
	r.Warnings = append(r.Warnings, msg)
	r.mu.Unlock()
}

// Run inspects the binary and looks up latest versions.
func Run(ctx context.Context, opts Options) (*Report, error) {
	path, err := caddybin.Find(opts.Binary)
	if err != nil {
		return nil, err
	}
	bin, err := caddybin.Inspect(ctx, path)
	if err != nil {
		return nil, err
	}
	proxy := opts.Proxy
	if proxy == nil {
		proxy = goproxy.New()
	}
	r := &Report{Binary: bin}

	if units, err := systemd.UnitsUsing(ctx, bin.ResolvedPath); err != nil {
		r.Warnings = append(r.Warnings, "could not query systemd: "+err.Error())
	} else {
		r.Services = units
	}

	if bin.IsDistroBuild() {
		r.Warnings = append(r.Warnings,
			"this binary carries no Go module information, which is typical of distribution packages; "+
				"its plugin set and pinned versions cannot be reproduced, so 'build' and 'install' will refuse to operate on it")
	}
	if bin.Package != nil {
		r.Warnings = append(r.Warnings, fmt.Sprintf(
			"this binary is owned by the %s package %q; a custom build written over it would be undone by the next package upgrade",
			bin.Package.Manager, bin.Package.Package))
	}

	// Caddy itself.
	installed := bin.MainVersion
	if installed == "" {
		installed = semver.Canonical(bin.Version)
	}
	r.Caddy = Status{Name: "caddy", Package: caddybin.CaddyModulePath, Installed: installed, Pseudo: semver.IsPseudo(installed)}

	// Latest lookups in parallel: Caddy plus every plugin with a known package.
	r.Plugins = make([]Status, len(bin.Plugins))
	var wg sync.WaitGroup
	lookup := func(s *Status) {
		defer wg.Done()
		info, err := proxy.Latest(ctx, s.Package)
		if err != nil {
			s.Error = err.Error()
			return
		}
		s.Latest = info.Version
		s.Outdated = semver.Compare(s.Installed, s.Latest) < 0
		if s.Outdated && s.Pseudo && semver.IsPseudo(s.Latest) {
			s.Note = "untagged module: newer commit on default branch"
		}
		path, minfo, ok, err := proxy.NewerMajor(ctx, s.Package)
		if err != nil {
			r.warn(fmt.Sprintf("%s: could not probe for newer major versions: %v", s.Name, err))
			return
		}
		if ok {
			s.Major = &Major{Package: path, Version: minfo.Version}
		}
	}
	wg.Add(1)
	go lookup(&r.Caddy)
	for i, p := range bin.Plugins {
		s := Status{Name: p.ModuleID, Package: p.Package, Installed: p.Version, Pseudo: semver.IsPseudo(p.Version)}
		r.Plugins[i] = s
		switch {
		case p.Err != "":
			r.Plugins[i].Error = p.Err
		case p.Replace != "":
			r.Plugins[i].Note = "replaced by " + p.Replace + "; not checked"
		case p.Package == "" || p.Version == "":
			r.Plugins[i].Note = "no module info; not checked"
		default:
			wg.Add(1)
			go lookup(&r.Plugins[i])
		}
	}
	wg.Wait()

	if r.Caddy.Major != nil {
		r.Warnings = append(r.Warnings, fmt.Sprintf(
			"Caddy %s exists at %s; Caddy has not historically backported security fixes to a previous major version. "+
				"Plugins built for the current major will not compile against it, so 'build' will not cross majors without an explicit flag",
			r.Caddy.Major.Version, r.Caddy.Major.Package))
	}
	return r, nil
}

// WriteJSON prints the report as indented JSON.
func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteText prints a human-readable report.
func (r *Report) WriteText(w io.Writer) {
	b := r.Binary
	fmt.Fprintf(w, "Binary:   %s", b.Path)
	if b.ResolvedPath != b.Path {
		fmt.Fprintf(w, " -> %s", b.ResolvedPath)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Version:  %s (%s)\n", b.Version, b.GoVersion)
	if b.Package != nil {
		fmt.Fprintf(w, "Package:  %s %s (%s)\n", b.Package.Package, b.Package.Version, b.Package.Manager)
	}
	if b.IsDistroBuild() {
		fmt.Fprintln(w, "Build:    no Go module information (distribution-style build)")
	} else {
		fmt.Fprintf(w, "Build:    Go module info present, %d standard modules, %d plugins\n", b.StandardCount, len(b.Plugins))
	}
	for _, u := range r.Services {
		fmt.Fprintf(w, "Service:  %s (%s/%s)\n", u.Name, u.ActiveState, u.SubState)
	}
	fmt.Fprintln(w)

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "COMPONENT\tPACKAGE\tINSTALLED\tLATEST\tSTATUS")
	writeRow(tw, r.Caddy)
	for _, p := range r.Plugins {
		writeRow(tw, p)
	}
	tw.Flush()
	if len(r.Plugins) == 0 && len(b.Unknown) == 0 {
		if b.IsDistroBuild() {
			fmt.Fprintln(w, "\nPlugins:  unknown (no module information in this binary)")
		} else {
			fmt.Fprintln(w, "\nPlugins:  none")
		}
	}
	for _, u := range b.Unknown {
		fmt.Fprintf(w, "Unknown module: %s (no package information)\n", u.ModuleID)
	}

	if len(r.Warnings) > 0 {
		fmt.Fprintln(w, "\nWarnings:")
		for _, wmsg := range r.Warnings {
			fmt.Fprintf(w, "  - %s\n", wmsg)
		}
	}
}

func writeRow(w io.Writer, s Status) {
	status := "current"
	switch {
	case s.Error != "":
		status = "error: " + s.Error
	case s.Latest == "":
		status = "not checked"
	case s.Outdated || s.Major != nil:
		status = "OUTDATED"
	}
	if s.Note != "" {
		status += " (" + s.Note + ")"
	}
	if s.Major != nil {
		status += " (major " + s.Major.Version + " at " + s.Major.Package + ")"
	}
	fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", s.Name, s.Package, short(s.Installed), short(s.Latest), status)
}

// short trims pseudo-versions for display: v0.0.0-20240814120000-0123456789ab
// becomes v0.0.0-20240814-0123456789ab.
func short(v string) string {
	if v == "" {
		return "-"
	}
	if semver.IsPseudo(v) && len(v) > 27 {
		// keep date, drop time-of-day
		i := len(v) - 13 - 14 // start of timestamp
		if i > 0 {
			return v[:i+8] + v[i+14:]
		}
	}
	return v
}
