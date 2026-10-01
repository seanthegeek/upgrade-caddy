// Package check implements the `check` command: is the installed Caddy, or
// any plugin compiled into it, behind the latest published version?
package check

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
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

// Major describes the newest major version living at a different module
// path, and how many newer majors exist, counting it.
type Major struct {
	Package string `json:"package"`
	Version string `json:"version"`
	Behind  int    `json:"behind"` // number of newer majors, including this one
}

// Status of one versioned component.
type Status struct {
	Name           string `json:"name"`
	Package        string `json:"package,omitempty"` // Go module path
	Installed      string `json:"installed"`
	Latest         string `json:"latest,omitempty"` // latest within the installed major
	Outdated       bool   `json:"outdated"`         // a newer version exists, within the major or beyond it
	NewerInMajor   bool   `json:"newer_in_major"`   // Latest is ahead of Installed
	PseudoVersion  bool   `json:"pseudo_version"`   // installed is pinned to an untagged commit
	MajorAvailable *Major `json:"major_available,omitempty"`
	Note           string `json:"note,omitempty"`
	Error          string `json:"error,omitempty"`
}

// Report is the result of a check. UpdatesAvailable mirrors the exit
// status so JSON consumers do not have to re-derive it.
type Report struct {
	Binary           *caddybin.Info `json:"binary"`
	Caddy            Status         `json:"caddy"`
	Plugins          []Status       `json:"plugins"`
	Units            []systemd.Unit `json:"units"`
	Warnings         []string       `json:"warnings"`
	UpdatesAvailable bool           `json:"updates_available"`
	HasErrors        bool           `json:"has_errors"` // some component could not be checked because of a hard failure

	mu sync.Mutex // guards Warnings during concurrent lookups
}

// anyOutdated reports whether Caddy or any plugin is behind. A newer major
// version always counts: Caddy has never backported security fixes to a
// previous major, so staying on one is a risk even though build will not
// cross it without being told to.
func (r *Report) anyOutdated() bool {
	all := append([]Status{r.Caddy}, r.Plugins...)
	for _, s := range all {
		if s.Outdated {
			return true
		}
	}
	return false
}

func (r *Report) anyErrors() bool {
	all := append([]Status{r.Caddy}, r.Plugins...)
	for _, s := range all {
		if s.Error != "" {
			return true
		}
	}
	return false
}

// lookupAll resolves every row concurrently, but once per Go module: a
// plugin that registers several Caddy modules yields several rows with the
// same package and installed version, and they share one lookup rather
// than issuing identical requests.
func lookupAll(ctx context.Context, proxy *goproxy.Client, rows []*Status) {
	groups := map[string][]*Status{}
	var order []string
	for _, s := range rows {
		key := s.Package + "@" + s.Installed
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], s)
	}
	var wg sync.WaitGroup
	for _, key := range order {
		group := groups[key]
		wg.Add(1)
		go func() {
			defer wg.Done()
			first := group[0]
			resolveStatus(ctx, proxy, first)
			for _, other := range group[1:] {
				other.Latest, other.NewerInMajor, other.Outdated = first.Latest, first.NewerInMajor, first.Outdated
				other.MajorAvailable, other.Note, other.Error = first.MajorAvailable, first.Note, first.Error
			}
		}()
	}
	wg.Wait()
}

// resolveStatus fills in Latest, NewerInMajor, MajorAvailable and Outdated
// for one component. A lookup that cannot happen (GOPROXY=off, a private
// module) is a note; a lookup that fails, including the newer-major probe,
// is an error that makes check exit 1. The within-major answer is recorded
// before the major probe, so a failed probe cannot hide a newer version
// the proxy already reported.
func resolveStatus(ctx context.Context, proxy *goproxy.Client, s *Status) {
	info, err := proxy.Latest(ctx, s.Package)
	if err != nil {
		if reason, ok := goproxy.NotChecked(err); ok {
			s.Note = reason
			return
		}
		s.Error = err.Error()
		return
	}
	s.Latest = info.Version
	newer := semver.Compare(s.Installed, s.Latest) < 0
	// A bare module path holds v0, v1 and vN+incompatible alike, so the
	// latest version on the same path can be a different major. That is a
	// major change, not a within-major update.
	samePathMajor := newer && semver.Major(s.Installed) >= 0 && semver.Major(s.Latest) > semver.Major(s.Installed)
	s.NewerInMajor = newer && !samePathMajor
	s.Outdated = newer
	if samePathMajor {
		s.MajorAvailable = &Major{Package: s.Package, Version: s.Latest, Behind: 1}
	}
	if s.NewerInMajor && s.PseudoVersion && semver.IsPseudo(s.Latest) {
		s.Note = "untagged module: newer commit on default branch"
	}
	majors, err := proxy.NewerMajors(ctx, s.Package, s.Installed)
	if err != nil {
		s.Error = fmt.Sprintf("could not probe for newer major versions: %v", err)
		return
	}
	if len(majors) > 0 {
		newest := majors[len(majors)-1]
		behind := len(majors)
		if samePathMajor {
			behind++
		}
		s.MajorAvailable = &Major{Package: newest.Path, Version: newest.Version, Behind: behind}
		s.Outdated = true
	}
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
		r.Units = units
	}

	if bin.IsDistroBuild() {
		r.Warnings = append(r.Warnings,
			"this binary carries no Go module information, which is typical of distribution packages; "+
				"its plugin set and pinned versions cannot be reproduced, so 'build' and 'install' will refuse to operate on it")
	}
	if bin.Owner != nil {
		r.Warnings = append(r.Warnings, fmt.Sprintf(
			"this binary is owned by the %s package %q; a custom build written over it would be undone by the next package upgrade",
			bin.Owner.Manager, bin.Owner.Package))
	}

	// Caddy itself.
	installed := bin.MainVersion
	if installed == "" {
		installed = semver.Canonical(bin.Version)
	}
	caddyPath := bin.MainPath
	if caddyPath == "" {
		caddyPath = caddybin.CaddyModulePath
	}
	r.Caddy = Status{Name: "caddy", Package: caddyPath, Installed: installed, PseudoVersion: semver.IsPseudo(installed)}

	// Latest lookups: Caddy plus every plugin with a known package.
	r.Plugins = make([]Status, len(bin.Plugins))
	rows := []*Status{&r.Caddy}
	for i, p := range bin.Plugins {
		r.Plugins[i] = Status{Name: p.ModuleID, Package: p.Package, Installed: p.Version, PseudoVersion: semver.IsPseudo(p.Version)}
		switch {
		case p.Error != "":
			r.Plugins[i].Error = p.Error
		case p.Replace != "":
			r.Plugins[i].Note = "replaced by " + p.Replace + "; not checked"
		case p.Package == "" || p.Version == "":
			r.Plugins[i].Note = "no module info; not checked"
		default:
			rows = append(rows, &r.Plugins[i])
		}
	}
	lookupAll(ctx, proxy, rows)
	r.UpdatesAvailable = r.anyOutdated()
	r.HasErrors = r.anyErrors()

	if m := r.Caddy.MajorAvailable; m != nil {
		behind := "one major version"
		if m.Behind > 1 {
			behind = fmt.Sprintf("%d major versions", m.Behind)
		}
		r.Warnings = append(r.Warnings, fmt.Sprintf(
			"the installed Caddy is %s behind: %s is at %s. Caddy has not historically backported security fixes to a previous major version. "+
				"Plugins built for the current major will not compile against it, so 'build' will not cross majors without an explicit flag",
			behind, m.Version, m.Package))
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
	if b.Owner != nil {
		fmt.Fprintf(w, "Package:  %s %s (%s)\n", b.Owner.Package, b.Owner.Version, b.Owner.Manager)
	}
	if b.IsDistroBuild() {
		fmt.Fprintln(w, "Build:    no Go module information (distribution-style build)")
	} else {
		fmt.Fprintf(w, "Build:    Go module info present, %d standard modules, %d plugins\n", b.StandardCount, len(b.Plugins))
	}
	for _, u := range r.Units {
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
	if len(r.Plugins) == 0 && len(b.UnknownModules) == 0 {
		if b.IsDistroBuild() {
			fmt.Fprintln(w, "\nPlugins:  unknown (no module information in this binary)")
		} else {
			fmt.Fprintln(w, "\nPlugins:  none")
		}
	}
	for _, u := range b.UnknownModules {
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
	case s.Outdated:
		status = "OUTDATED"
	}
	if s.Note != "" {
		status += " (" + s.Note + ")"
	}
	if m := s.MajorAvailable; m != nil {
		switch {
		case m.Behind > 1:
			status += fmt.Sprintf(" (%d majors behind: %s at %s)", m.Behind, m.Version, m.Package)
		case m.Package == s.Package:
			status += " (new major " + m.Version + " on the same path)"
		default:
			status += " (major " + m.Version + " at " + m.Package + ")"
		}
	}
	fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", s.Name, s.Package, short(s.Installed), short(s.Latest), status)
}

// short trims pseudo-versions for display: v0.0.0-20240814120000-0123456789ab
// becomes v0.0.0-20240814-0123456789ab. Build metadata such as +dirty is
// kept after the shortened form.
func short(v string) string {
	if v == "" {
		return "-"
	}
	core, build, _ := strings.Cut(v, "+")
	if semver.IsPseudo(v) && len(core) > 27 {
		// keep date, drop time-of-day
		i := len(core) - 13 - 14 // start of timestamp
		if i > 0 {
			core = core[:i+8] + core[i+14:]
			if build != "" {
				return core + "+" + build
			}
			return core
		}
	}
	return v
}
