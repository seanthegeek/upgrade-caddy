// Package systemd discovers service units that execute a given binary and
// drives them through systemctl.
package systemd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/seanthegeek/upgrade-caddy/internal/fspath"
)

// UnresolvedHome is what WorkingDirectory holds when the unit runs in its
// user's home directory ("WorkingDirectory=~") and that user could not be
// looked up. Callers that need the real directory must treat it as unknown.
const UnresolvedHome = "~"

// Command is one ExecStart= command of a unit.
type Command struct {
	Path string   `json:"path"`
	Args []string `json:"args"`
}

// Unit is a systemd service that runs the binary of interest.
type Unit struct {
	Name                string    `json:"name"`
	ExecStart           string    `json:"exec_start"`                     // every ExecStart= line, newline-joined
	Commands            []Command `json:"commands,omitempty"`             // every ExecStart= command, in order
	Args                []string  `json:"args,omitempty"`                 // argv of the command that runs the binary of interest (the first, until matched)
	WorkingDirectory    string    `json:"working_directory,omitempty"`    // resolved by UnitsUsing: "/" when unset, the user's home for "~"
	User                string    `json:"user,omitempty"`                 // User=, as given (name or numeric ID); "" is root
	Group               string    `json:"group,omitempty"`                // Group=, as given; "" is the user's primary group
	SupplementaryGroups []string  `json:"supplementary_groups,omitempty"` // SupplementaryGroups=
	DynamicUser         bool      `json:"dynamic_user,omitempty"`         // DynamicUser=yes: the account exists only while the unit runs
	Environment         []string  `json:"environment,omitempty"`          // Environment=, KEY=VALUE each
	EnvironmentFiles    []EnvFile `json:"environment_files,omitempty"`    // EnvironmentFile=, in order
	ExecSearchPath      []string  `json:"exec_search_path,omitempty"`     // ExecSearchPath=, the directories a bare executable name is looked up in
	MainPID             int       `json:"main_pid,omitempty"`
	ActiveState         string    `json:"active_state"`
	SubState            string    `json:"sub_state"`
}

// ConfigArgs reads the Caddy config flags out of the unit's command line:
// --config/-c, --adapter/-a and --envfile (repeatable, comma-separable),
// each in either "--flag value" or "--flag=value" form.
func (u Unit) ConfigArgs() (config, adapter string, envfiles []string) {
	args := u.Args
	for i := 0; i < len(args); i++ {
		name, value, hasValue := strings.Cut(args[i], "=")
		if !hasValue && i+1 < len(args) {
			value = args[i+1]
		}
		switch name {
		case "--config", "-c":
			config = value
		case "--adapter", "-a":
			adapter = value
		case "--envfile":
			for _, f := range strings.Split(value, ",") {
				if f = strings.TrimSpace(f); f != "" {
					envfiles = append(envfiles, f)
				}
			}
		default:
			continue
		}
		if !hasValue {
			i++
		}
	}
	return config, adapter, envfiles
}

const showProps = "-p Id -p ExecStart -p ActiveState -p SubState -p WorkingDirectory -p User -p Group -p SupplementaryGroups -p DynamicUser -p Environment -p EnvironmentFiles -p ExecSearchPath -p MainPID"

// defaultSearchPathCompat is systemd's compiled-in executable search path
// on a split-/usr system (DEFAULT_PATH_COMPAT in src/basic/path-util.h);
// the merged-/usr one is its first four entries. It is the fallback when
// `systemd-path search-binaries-default` cannot report the real value;
// the extra entries are harmless on merged /usr, where they are symlinks
// to the first four and come after them.
const defaultSearchPathCompat = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// Available reports whether systemctl is on PATH.
func Available() bool {
	_, err := exec.LookPath("systemctl")
	return err == nil
}

// UnitsUsing lists loaded service units whose ExecStart executable resolves
// to binary. It returns nil, nil when systemctl is not available.
func UnitsUsing(ctx context.Context, binary string) ([]Unit, error) {
	if !Available() {
		return nil, nil
	}
	// Links are followed even to a referent that does not exist yet, as
	// install resolves its target, so a first install through a dangling
	// link still finds the unit whose ExecStart is that link.
	want, _, err := fspath.Resolve(binary)
	if err != nil {
		want = binary
	}
	args := append([]string{"show", "--no-pager"}, strings.Fields(showProps)...)
	out, err := exec.CommandContext(ctx, "systemctl", append(args, "*.service")...).Output()
	if err != nil {
		return nil, err
	}
	var units []Unit
	var defaultPath []string // systemd's own search path, asked for only when a bare name needs it
	for _, u := range parseShow(string(out)) {
		var search []string
		if hasBareName(u.Commands) {
			search = searchPathFor(ctx, &u, execSearchPathFromBus)
			if len(search) == 0 {
				if defaultPath == nil {
					defaultPath = defaultSearchPath(ctx)
				}
				search = defaultPath
			}
		}
		matched := false
		for _, c := range u.Commands {
			if resolves(c.Path, want, search) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		// systemctl show flattens argv with spaces, so an argument that
		// contains one cannot be recovered from it. The D-Bus property keeps
		// the array; take the commands from there when it can be read.
		if cmds, err := execStartFromBus(ctx, u.Name); err == nil && len(cmds) > 0 {
			u.Commands = cmds
		}
		// The same goes for Environment= (a value with a space) and
		// EnvironmentFiles= (a path with one): the D-Bus properties keep
		// them whole, and when they cannot be read the lossy rendering is
		// not good enough to validate a config with.
		if err := preciseEnvironment(&u, func() ([]string, error) { return environmentFromBus(ctx, u.Name) },
			func() ([]EnvFile, error) { return environmentFilesFromBus(ctx, u.Name) }); err != nil {
			return nil, err
		}
		for _, c := range u.Commands {
			if resolves(c.Path, want, search) {
				u.Args = c.Args // the command that runs this binary, not necessarily the first
				break
			}
		}
		resolveWorkingDirectory(&u, homeOf)
		units = append(units, u)
	}
	return units, nil
}

// preciseEnvironment replaces the environment settings parsed from
// systemctl show's rendering with the D-Bus properties, which keep each
// assignment and path whole. If a property cannot be read and the rendering
// showed settings, that is an error rather than a fall back to the
// rendering: a value split at a space would validate a config with a
// different environment from the service's. With nothing rendered there is
// nothing to get wrong, and the bus is not needed.
func preciseEnvironment(u *Unit, env func() ([]string, error), files func() ([]EnvFile, error)) error {
	if len(u.Environment) > 0 {
		exact, err := env()
		if err != nil {
			return fmt.Errorf("%s: reading Environment= over D-Bus: %w (the systemctl show rendering cannot be trusted for values with spaces)", u.Name, err)
		}
		u.Environment = exact
	}
	if len(u.EnvironmentFiles) > 0 {
		exact, err := files()
		if err != nil {
			return fmt.Errorf("%s: reading EnvironmentFiles= over D-Bus: %w (the systemctl show rendering cannot be trusted for paths with spaces)", u.Name, err)
		}
		u.EnvironmentFiles = exact
	}
	return nil
}

// UnitPrefix is the unit name without its type suffix and, for a template
// instance, without the instance: "caddy@site.service" gives "caddy", as
// unit_name_to_prefix does.
func UnitPrefix(name string) string {
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[:i]
	}
	if i := strings.IndexByte(name, '@'); i >= 0 {
		name = name[:i]
	}
	return name
}

// ValidUserName applies systemd's strict user name rule
// (valid_user_group_name in src/basic/user-util.c, v255, without the
// relaxed flags): an ASCII letter or underscore first, then ASCII letters,
// digits, underscores and dashes, at most 31 characters (UT_NAMESIZE - 1).
// systemd uses it to decide whether a unit's prefix can serve as its
// dynamic user's name.
func ValidUserName(name string) bool {
	if name == "" || len(name) > 31 {
		return false
	}
	for i, c := range []byte(name) {
		letter := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		switch {
		case letter || c == '_':
		case i > 0 && ((c >= '0' && c <= '9') || c == '-'):
		default:
			return false
		}
	}
	return true
}

// resolveWorkingDirectory turns systemd's rendering of WorkingDirectory
// into the directory the service actually starts in, the way
// apply_working_directory in src/core/exec-invoke.c does: unset means "/"
// (empty_to_root), and "~" means the home of the unit's user, root when
// User= is not set. A "~" whose user cannot be looked up stays as
// UnresolvedHome rather than becoming some other directory.
func resolveWorkingDirectory(u *Unit, homeOf func(user string) (string, error)) {
	switch u.WorkingDirectory {
	case "":
		u.WorkingDirectory = "/"
	case UnresolvedHome:
		if home, err := homeOf(u.User); err == nil && home != "" {
			u.WorkingDirectory = home
		}
	}
}

// homeOf looks up a user's home directory by name or numeric ID; an empty
// name is root, which is what a unit without User= runs as.
func homeOf(name string) (string, error) {
	var (
		acct *user.User
		err  error
	)
	switch {
	case name == "":
		acct, err = user.LookupId("0")
	case strings.Trim(name, "0123456789") == "":
		acct, err = user.LookupId(name)
	default:
		acct, err = user.Lookup(name)
	}
	if err != nil {
		return "", err
	}
	if acct.HomeDir == "" {
		return "", fmt.Errorf("user %s has no home directory", name)
	}
	return acct.HomeDir, nil
}

// searchPathFor returns the directories a unit's bare executable name is
// looked up in: its ExecSearchPath=, read over D-Bus when the unit sets
// one, because the systemctl rendering splits on spaces and a directory
// containing one would otherwise be looked for under two wrong names. The
// flattened rendering is only the fallback when the bus cannot be read.
// It is read before the first match, so a unit is never missed because
// of that split. nil means the unit sets none and systemd's default
// applies.
func searchPathFor(ctx context.Context, u *Unit, fromBus func(context.Context, string) ([]string, error)) []string {
	if len(u.ExecSearchPath) == 0 {
		return nil
	}
	if dirs, err := fromBus(ctx, u.Name); err == nil && len(dirs) > 0 {
		u.ExecSearchPath = dirs
	}
	return u.ExecSearchPath
}

// hasBareName reports whether any command names its executable without a
// slash, leaving systemd to find it on a search path.
func hasBareName(cmds []Command) bool {
	for _, c := range cmds {
		if !strings.Contains(c.Path, "/") {
			return true
		}
	}
	return false
}

// resolves reports whether a unit's ExecStart executable is the wanted
// file. A bare name (no slash) is looked up the way systemd does before it
// runs the command (find_executable_full in src/basic/path-util.c): the
// first directory in search holding a regular file of that name with an
// execute bit, with non-absolute entries skipped. Note systemd searches
// the unit's ExecSearchPath= or its own compiled-in default, never the
// unit's PATH environment. The result is then followed through symlinks
// and compared.
func resolves(path, want string, search []string) bool {
	if !strings.Contains(path, "/") {
		found := ""
		for _, dir := range search {
			if !filepath.IsAbs(dir) {
				continue
			}
			candidate := filepath.Join(dir, path)
			// systemd moves on past an entry it cannot look at, so a
			// stat error here means "not this directory", not "no unit".
			if fi, err := os.Stat(candidate); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
				found = candidate
				break
			}
			// Nothing there yet, but the binary about to be installed
			// would be: that is where systemd will find the name once it
			// exists, unless an earlier directory holds one by then. A
			// first install's unit is found this way, and build refuses
			// an output path a bare-name unit would start using.
			if r, exists, err := fspath.Resolve(candidate); err == nil && !exists && r == want {
				found = candidate
				break
			}
		}
		if found == "" {
			return false
		}
		path = found
	}
	if r, _, err := fspath.Resolve(path); err == nil {
		path = r
	}
	return path == want
}

// DefaultSearchPath asks systemd for its compiled-in executable search path,
// which is what a bare ExecStart= name is looked up in when the unit sets
// no ExecSearchPath=, and what PATH is set to for a service that sets none.
// Older systemd without that query, or no systemd-path at all, falls back
// to the compiled-in default of a split-/usr build.
func DefaultSearchPath(ctx context.Context) []string {
	return defaultSearchPath(ctx)
}

func defaultSearchPath(ctx context.Context) []string {
	out, err := exec.CommandContext(ctx, "systemd-path", "search-binaries-default").Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		return parseSearchPath(defaultSearchPathCompat)
	}
	return parseSearchPath(strings.TrimSpace(string(out)))
}

// parseSearchPath splits a colon-separated search path, dropping empty and
// non-absolute entries as systemd does.
func parseSearchPath(s string) []string {
	var dirs []string
	for _, d := range strings.Split(s, ":") {
		if filepath.IsAbs(d) {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

// execSearchPathFromBus reads the unit's ExecSearchPath property over D-Bus,
// a string array that keeps a directory name containing a space intact.
func execSearchPathFromBus(ctx context.Context, unit string) ([]string, error) {
	out, err := exec.CommandContext(ctx, "busctl", "--json=short", "get-property",
		"org.freedesktop.systemd1", unitObjectPath(unit), "org.freedesktop.systemd1.Service", "ExecSearchPath").Output()
	if err != nil {
		return nil, err
	}
	return parseBusStrings(out)
}

// parseBusStrings decodes busctl's JSON for a string-array ("as") property.
func parseBusStrings(data []byte) ([]string, error) {
	var prop struct {
		Data []string `json:"data"`
	}
	if err := json.Unmarshal(data, &prop); err != nil {
		return nil, fmt.Errorf("busctl string array: %w", err)
	}
	return prop.Data, nil
}

// execStartFromBus reads the unit's ExecStart property over D-Bus as JSON,
// which preserves argument boundaries exactly.
func execStartFromBus(ctx context.Context, unit string) ([]Command, error) {
	out, err := exec.CommandContext(ctx, "busctl", "--json=short", "get-property",
		"org.freedesktop.systemd1", unitObjectPath(unit), "org.freedesktop.systemd1.Service", "ExecStart").Output()
	if err != nil {
		return nil, err
	}
	return parseBusExecStart(out)
}

// parseBusExecStart decodes busctl's JSON for the ExecStart property, an
// array of (path, argv, ignore_errors, times..., pid, code, status) tuples.
func parseBusExecStart(data []byte) ([]Command, error) {
	var prop struct {
		Data [][]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &prop); err != nil {
		return nil, fmt.Errorf("busctl ExecStart: %w", err)
	}
	var cmds []Command
	for _, tuple := range prop.Data {
		if len(tuple) < 2 {
			return nil, errors.New("busctl ExecStart: tuple too short")
		}
		var c Command
		if err := json.Unmarshal(tuple[0], &c.Path); err != nil {
			return nil, fmt.Errorf("busctl ExecStart path: %w", err)
		}
		if err := json.Unmarshal(tuple[1], &c.Args); err != nil {
			return nil, fmt.Errorf("busctl ExecStart argv: %w", err)
		}
		cmds = append(cmds, c)
	}
	return cmds, nil
}

// unitObjectPath is systemd's D-Bus object path for a unit: the name with
// every byte outside [A-Za-z0-9] written as "_" plus two hex digits.
func unitObjectPath(unit string) string {
	var b strings.Builder
	b.WriteString("/org/freedesktop/systemd1/unit/")
	for i := 0; i < len(unit); i++ {
		ch := unit[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9':
			b.WriteByte(ch)
		default:
			fmt.Fprintf(&b, "_%02x", ch)
		}
	}
	return b.String()
}

// parseShow turns `systemctl show` output, one "Key=Value" block per unit
// separated by blank lines, into Units. A unit with several ExecStart
// commands (only Type=oneshot allows that) prints one ExecStart= line per
// command; they are all kept, newline-joined, and Args comes from the
// first. Units without an ExecStart are dropped.
func parseShow(out string) []Unit {
	var units []Unit
	for _, block := range strings.Split(out, "\n\n") {
		var u Unit
		for _, line := range strings.Split(block, "\n") {
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			switch k {
			case "Id":
				u.Name = v
			case "ExecStart":
				if u.ExecStart == "" {
					u.ExecStart = v
				} else {
					u.ExecStart += "\n" + v
				}
				for _, g := range execGroups(v) {
					u.Commands = append(u.Commands, Command{Path: execPath(g), Args: execArgs(g)})
				}
				if u.Args == nil && len(u.Commands) > 0 {
					u.Args = u.Commands[0].Args
				}
			case "ActiveState":
				u.ActiveState = v
			case "SubState":
				u.SubState = v
			case "WorkingDirectory":
				// systemd prefixes "!" when WorkingDirectory=-/path asked
				// it to ignore a missing directory, and prints "~" for the
				// unit user's home (property_get_working_directory in
				// src/core/dbus-execute.c). The "~" is kept here and
				// resolved by resolveWorkingDirectory once User is known.
				u.WorkingDirectory = strings.TrimPrefix(v, "!")
			case "User":
				u.User = v
			case "Group":
				u.Group = v
			case "SupplementaryGroups":
				u.SupplementaryGroups = strings.Fields(v)
			case "DynamicUser":
				u.DynamicUser = v == "yes"
			case "Environment":
				// Space-joined; UnitsUsing re-reads it over D-Bus for a
				// matched unit, where an assignment with a space survives.
				u.Environment = strings.Fields(v)
			case "EnvironmentFiles":
				// One line per file, "path (ignore_errors=yes|no)".
				if f, ok := parseEnvFileShow(v); ok {
					u.EnvironmentFiles = append(u.EnvironmentFiles, f)
				}
			case "ExecSearchPath":
				// Rendered space-separated; UnitsUsing re-reads it over
				// D-Bus for a matched unit, where a directory with a space
				// survives.
				u.ExecSearchPath = strings.Fields(v)
			case "MainPID":
				u.MainPID, _ = strconv.Atoi(v)
			}
		}
		if u.Name != "" && u.ExecStart != "" {
			units = append(units, u)
		}
	}
	return units
}

// execGroups splits systemd's ExecStart rendering into its "{ ... }"
// groups, one per command.
func execGroups(execStart string) []string {
	var groups []string
	rest := execStart
	for {
		_, after, ok := strings.Cut(rest, "{")
		if !ok {
			return groups
		}
		g, remainder, _ := strings.Cut(after, "}")
		if g = strings.TrimSpace(g); g != "" {
			groups = append(groups, g)
		}
		rest = remainder
	}
}

// execPaths extracts every "path=..." from systemd's ExecStart rendering,
// "{ path=/usr/bin/caddy ; argv[]=/usr/bin/caddy run ... }", one such group
// per line when a unit has several ExecStart commands.
func execPaths(execStart string) []string {
	var paths []string
	for _, g := range execGroups(execStart) {
		if p := execPath(g); p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

// execPath extracts "path=..." from one command group.
func execPath(group string) string {
	_, after, ok := strings.Cut(group, "path=")
	if !ok {
		return ""
	}
	p, _, _ := strings.Cut(after, " ;")
	return strings.TrimSpace(p)
}

// execArgs extracts the argv of one command group from systemctl show's
// rendering. systemd joins the arguments with single spaces there, so an
// argument containing a space cannot be told apart from two; UnitsUsing
// replaces these with the D-Bus array whenever busctl can be read, and this
// parse is the fallback.
func execArgs(group string) []string {
	_, after, ok := strings.Cut(group, "argv[]=")
	if !ok {
		return nil
	}
	argv, _, _ := strings.Cut(after, " ;")
	return strings.Fields(argv)
}

// Controller is the part of systemctl install needs. It is an interface so
// the restart-and-verify sequence can be tested without systemd.
type Controller interface {
	Restart(ctx context.Context, unit string) error
	Stop(ctx context.Context, unit string) error
	IsActive(ctx context.Context, unit string) (bool, error)
	MainPID(ctx context.Context, unit string) (int, error)
}

// Systemctl drives the real systemctl.
type Systemctl struct{}

// Restart runs `systemctl restart`.
func (Systemctl) Restart(ctx context.Context, unit string) error {
	out, err := exec.CommandContext(ctx, "systemctl", "restart", unit).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl restart %s: %w: %s", unit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Stop runs `systemctl stop`.
func (Systemctl) Stop(ctx context.Context, unit string) error {
	out, err := exec.CommandContext(ctx, "systemctl", "stop", unit).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl stop %s: %w: %s", unit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// IsActive runs `systemctl is-active`, which exits non-zero for any state
// other than active without that being an error for us. A unit that has
// entered the failed state is reported as an error straight away, so a
// caller waiting for it to come up does not wait out its deadline.
func (Systemctl) IsActive(ctx context.Context, unit string) (bool, error) {
	out, err := exec.CommandContext(ctx, "systemctl", "is-active", unit).Output()
	state := strings.TrimSpace(string(out))
	switch state {
	case "active":
		return true, nil
	case "failed":
		return false, fmt.Errorf("%s entered the failed state", unit)
	case "":
		return false, fmt.Errorf("systemctl is-active %s: %w", unit, err)
	}
	return false, nil
}

// MainPID returns the unit's main process ID, 0 if it has none.
func (Systemctl) MainPID(ctx context.Context, unit string) (int, error) {
	out, err := exec.CommandContext(ctx, "systemctl", "show", "-p", "MainPID", "--value", unit).Output()
	if err != nil {
		return 0, fmt.Errorf("systemctl show %s: %w", unit, err)
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}
