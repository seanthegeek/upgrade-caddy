// Package systemd discovers service units that execute a given binary and
// drives them through systemctl.
package systemd

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Unit is a systemd service that runs the binary of interest.
type Unit struct {
	Name             string   `json:"name"`
	ExecStart        string   `json:"exec_start"`     // every ExecStart= line, newline-joined
	Args             []string `json:"args,omitempty"` // argv of the first ExecStart command
	WorkingDirectory string   `json:"working_directory,omitempty"`
	MainPID          int      `json:"main_pid,omitempty"`
	ActiveState      string   `json:"active_state"`
	SubState         string   `json:"sub_state"`
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
		for _, exe := range execPaths(u.ExecStart) {
			if r, err := filepath.EvalSymlinks(exe); err == nil {
				exe = r
			}
			if exe == want {
				units = append(units, u)
				break
			}
		}
	}
	return units, nil
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
					u.Args = execArgs(v)
				} else {
					u.ExecStart += "\n" + v
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

// execPaths extracts every "path=..." from systemd's ExecStart rendering,
// "{ path=/usr/bin/caddy ; argv[]=/usr/bin/caddy run ... }", one such group
// per line when a unit has several ExecStart commands.
func execPaths(execStart string) []string {
	var paths []string
	rest := execStart
	for {
		_, after, ok := strings.Cut(rest, "path=")
		if !ok {
			return paths
		}
		p, remainder, _ := strings.Cut(after, " ;")
		if p = strings.TrimSpace(p); p != "" {
			paths = append(paths, p)
		}
		rest = remainder
	}
}

// execArgs extracts the argv of the first ExecStart command. systemd joins
// the arguments with single spaces, so an argument containing a space
// cannot be told apart from two arguments; Caddy's flags never contain one.
func execArgs(execStart string) []string {
	_, after, ok := strings.Cut(execStart, "argv[]=")
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
