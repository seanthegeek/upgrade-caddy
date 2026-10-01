package systemd

import "testing"

// Captured from `systemctl show -p Id -p ExecStart -p ActiveState -p SubState`
// on Ubuntu 24.04 with Caddy's packaged unit, plus a unit with no ExecStart.
const sample = `ExecStart={ path=/usr/bin/caddy ; argv[]=/usr/bin/caddy run --environ --config /etc/caddy/Caddyfile ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }
Id=caddy.service
ActiveState=active
SubState=running

Id=systemd-tmpfiles-clean.service
ActiveState=inactive
SubState=dead

ExecStart={ path=/usr/bin/true ; argv[]=/usr/bin/true ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 } ; { path=/opt/caddy/caddy ; argv[]=/opt/caddy/caddy run ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }
Id=custom.service
ActiveState=failed
SubState=failed
`

func TestParseShow(t *testing.T) {
	units := parseShow(sample)
	if len(units) != 2 {
		t.Fatalf("got %d units, want 2 (unit without ExecStart must be dropped): %+v", len(units), units)
	}
	if units[0].Name != "caddy.service" || units[0].ActiveState != "active" || units[0].SubState != "running" {
		t.Errorf("caddy unit: %+v", units[0])
	}
	if units[1].Name != "custom.service" || units[1].ActiveState != "failed" {
		t.Errorf("custom unit: %+v", units[1])
	}
}

func TestExecPaths(t *testing.T) {
	units := parseShow(sample)
	if got := execPaths(units[0].ExecStart); len(got) != 1 || got[0] != "/usr/bin/caddy" {
		t.Errorf("single ExecStart: %v", got)
	}
	if got := execPaths(units[1].ExecStart); len(got) != 2 || got[0] != "/usr/bin/true" || got[1] != "/opt/caddy/caddy" {
		t.Errorf("multiple ExecStart: %v", got)
	}
	if got := execPaths("garbage"); got != nil {
		t.Errorf("no path: %v", got)
	}
}
