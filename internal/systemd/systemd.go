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
	for _, block := range strings.Split(string(out), "\n\n") {
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
		if u.Name == "" || u.ExecStart == "" {
			continue
		}
		if exe := execPath(u.ExecStart); exe != "" {
			if r, err := filepath.EvalSymlinks(exe); err == nil {
				exe = r
			}
			if exe == want {
				units = append(units, u)
			}
		}
	}
	return units, nil
}

// execPath extracts "path=..." from systemd's ExecStart rendering:
// "{ path=/usr/bin/caddy ; argv[]=/usr/bin/caddy run ... }".
func execPath(execStart string) string {
	_, rest, ok := strings.Cut(execStart, "path=")
	if !ok {
		return ""
	}
	p, _, _ := strings.Cut(rest, " ;")
	return strings.TrimSpace(p)
}
