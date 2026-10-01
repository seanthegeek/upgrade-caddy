// Package systemd discovers service units that execute a given binary.
package systemd

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
)

// Unit is a systemd service that runs the binary of interest.
type Unit struct {
	Name        string `json:"name"`
	ExecStart   string `json:"exec_start"`
	ActiveState string `json:"active_state"`
	SubState    string `json:"sub_state"`
}

// UnitsUsing lists loaded service units whose ExecStart executable resolves
// to binary. It returns nil, nil when systemctl is not available.
func UnitsUsing(ctx context.Context, binary string) ([]Unit, error) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return nil, nil
	}
	want, err := filepath.EvalSymlinks(binary)
	if err != nil {
		want = binary
	}
	out, err := exec.CommandContext(ctx, "systemctl", "show", "--no-pager",
		"-p", "Id", "-p", "ExecStart", "-p", "ActiveState", "-p", "SubState", "*.service").Output()
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
// separated by blank lines, into Units. Units without an ExecStart are
// dropped.
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
				u.ExecStart = v
			case "ActiveState":
				u.ActiveState = v
			case "SubState":
				u.SubState = v
			}
		}
		if u.Name != "" && u.ExecStart != "" {
			units = append(units, u)
		}
	}
	return units
}

// execPaths extracts every "path=..." from systemd's ExecStart rendering:
// "{ path=/usr/bin/caddy ; argv[]=/usr/bin/caddy run ... }". A unit with
// several ExecStart lines renders them as consecutive "{ ... }" groups.
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
