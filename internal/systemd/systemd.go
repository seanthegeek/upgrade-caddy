// Package systemd discovers service units that execute a given binary and
// drives them through systemctl.
package systemd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Command is one ExecStart= command of a unit.
type Command struct {
	Path string   `json:"path"`
	Args []string `json:"args"`
}

// Unit is a systemd service that runs the binary of interest.
type Unit struct {
	Name             string    `json:"name"`
	ExecStart        string    `json:"exec_start"`         // every ExecStart= line, newline-joined
	Commands         []Command `json:"commands,omitempty"` // every ExecStart= command, in order
	Args             []string  `json:"args,omitempty"`     // argv of the command that runs the binary of interest (the first, until matched)
	WorkingDirectory string    `json:"working_directory,omitempty"`
	MainPID          int       `json:"main_pid,omitempty"`
	ActiveState      string    `json:"active_state"`
	SubState         string    `json:"sub_state"`
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

const showProps = "-p Id -p ExecStart -p ActiveState -p SubState -p WorkingDirectory -p MainPID"

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
	want, err := filepath.EvalSymlinks(binary)
	if err != nil {
		want = binary
	}
	args := append([]string{"show", "--no-pager"}, strings.Fields(showProps)...)
	out, err := exec.CommandContext(ctx, "systemctl", append(args, "*.service")...).Output()
	if err != nil {
		return nil, err
	}
	var units []Unit
	for _, u := range parseShow(string(out)) {
		matched := false
		for _, c := range u.Commands {
			if resolves(c.Path, want) {
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
		for _, c := range u.Commands {
			if resolves(c.Path, want) {
				u.Args = c.Args // the command that runs this binary, not necessarily the first
				break
			}
		}
		units = append(units, u)
	}
	return units, nil
}

func resolves(path, want string) bool {
	if r, err := filepath.EvalSymlinks(path); err == nil {
		path = r
	}
	return path == want
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
				u.WorkingDirectory = v
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
