package systemd

import (
	"reflect"
	"testing"
)

// Captured from `systemctl show -p Id -p ExecStart -p ActiveState -p SubState
// -p WorkingDirectory -p MainPID` on Ubuntu 24.04 with Caddy's packaged
// unit, plus a unit with no ExecStart and one with several ExecStart lines.
const sample = `ExecStart={ path=/usr/bin/caddy ; argv[]=/usr/bin/caddy run --environ --config /etc/caddy/Caddyfile ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }
Id=caddy.service
ActiveState=active
SubState=running
WorkingDirectory=
MainPID=315

Id=systemd-tmpfiles-clean.service
ActiveState=inactive
SubState=dead

ExecStart={ path=/usr/bin/true ; argv[]=/usr/bin/true ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 } ; { path=/opt/caddy/caddy ; argv[]=/opt/caddy/caddy run --config /srv/site.json --adapter json --envfile /etc/caddy/a.env,/etc/caddy/b.env --envfile=/etc/caddy/c.env ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }
Id=custom.service
ActiveState=failed
SubState=failed
WorkingDirectory=/srv
MainPID=0
`

func TestParseShow(t *testing.T) {
	units := parseShow(sample)
	if len(units) != 2 {
		t.Fatalf("got %d units, want 2 (unit without ExecStart must be dropped): %+v", len(units), units)
	}
	c := units[0]
	if c.Name != "caddy.service" || c.ActiveState != "active" || c.SubState != "running" || c.MainPID != 315 || c.WorkingDirectory != "" {
		t.Errorf("caddy unit: %+v", c)
	}
	wantArgs := []string{"/usr/bin/caddy", "run", "--environ", "--config", "/etc/caddy/Caddyfile"}
	if !reflect.DeepEqual(c.Args, wantArgs) {
		t.Errorf("caddy args: %v", c.Args)
	}
	if units[1].Name != "custom.service" || units[1].ActiveState != "failed" || units[1].WorkingDirectory != "/srv" {
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

func TestConfigArgs(t *testing.T) {
	units := parseShow(sample)
	config, adapter, env := units[0].ConfigArgs()
	if config != "/etc/caddy/Caddyfile" || adapter != "" || env != nil {
		t.Errorf("packaged unit: %q %q %v", config, adapter, env)
	}
	// Only the first ExecStart's argv is parsed; here that is /usr/bin/true.
	config, _, _ = units[1].ConfigArgs()
	if config != "" {
		t.Errorf("multi ExecStart should use the first argv only, got %q", config)
	}
	u := Unit{Args: []string{"/opt/caddy/caddy", "run", "--config", "/srv/site.json", "--adapter", "json",
		"--envfile", "/etc/caddy/a.env,/etc/caddy/b.env", "--envfile=/etc/caddy/c.env"}}
	config, adapter, env = u.ConfigArgs()
	if config != "/srv/site.json" || adapter != "json" {
		t.Errorf("explicit: %q %q", config, adapter)
	}
	if want := []string{"/etc/caddy/a.env", "/etc/caddy/b.env", "/etc/caddy/c.env"}; !reflect.DeepEqual(env, want) {
		t.Errorf("envfiles: %v", env)
	}
	u = Unit{Args: []string{"caddy", "run", "-c", "/x/Caddyfile", "-a=caddyfile"}}
	config, adapter, _ = u.ConfigArgs()
	if config != "/x/Caddyfile" || adapter != "caddyfile" {
		t.Errorf("short flags: %q %q", config, adapter)
	}
}
