package systemd

import (
	"reflect"
	"strings"
	"testing"
)

// Captured from `systemctl show -p Id -p ExecStart -p ActiveState -p SubState
// -p WorkingDirectory -p MainPID` on Ubuntu 24.04 (systemd 255): Caddy's
// packaged unit, a unit with no ExecStart, and a Type=oneshot unit with two
// ExecStart= commands, which systemd prints as two ExecStart= lines.
const sample = `ExecStart={ path=/usr/bin/caddy ; argv[]=/usr/bin/caddy run --environ --config /etc/caddy/Caddyfile ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }
Id=caddy.service
ActiveState=active
SubState=running
WorkingDirectory=
MainPID=315

Id=systemd-tmpfiles-clean.service
ActiveState=inactive
SubState=dead

ExecStart={ path=/usr/bin/true ; argv[]=/usr/bin/true ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }
ExecStart={ path=/opt/caddy/caddy ; argv[]=/opt/caddy/caddy run --config /srv/site.json --adapter json --envfile /etc/caddy/a.env,/etc/caddy/b.env --envfile=/etc/caddy/c.env ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }
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
	if lines := strings.Count(units[1].ExecStart, "\n"); lines != 1 {
		t.Errorf("both ExecStart lines should be kept, got %d newlines in %q", lines, units[1].ExecStart)
	}
	if got := units[1].Args; len(got) == 0 || got[0] != "/usr/bin/true" {
		t.Errorf("before matching, args default to the first ExecStart, got %v", got)
	}
	if cmds := units[1].Commands; len(cmds) != 2 || cmds[1].Path != "/opt/caddy/caddy" || cmds[1].Args[2] != "--config" {
		t.Errorf("every ExecStart command should be kept with its own argv: %+v", cmds)
	}
}

func TestArgsComeFromTheMatchingCommand(t *testing.T) {
	// A Type=oneshot unit where Caddy is the second command: the config
	// flags must come from that command, not from /usr/bin/true.
	units := parseShow(sample)
	u := units[1]
	var matched *Unit
	for _, c := range u.Commands {
		if c.Path == "/opt/caddy/caddy" {
			u.Args = c.Args
			matched = &u
		}
	}
	if matched == nil {
		t.Fatal("fixture should contain the caddy command")
	}
	config, adapter, env := matched.ConfigArgs()
	if config != "/srv/site.json" || adapter != "json" || len(env) != 3 {
		t.Errorf("config flags from the matched command: %q %q %v", config, adapter, env)
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
	// Before UnitsUsing matches a command, Args is the first command's
	// argv; here that is /usr/bin/true, which has no --config.
	config, _, _ = units[1].ConfigArgs()
	if config != "" {
		t.Errorf("unmatched multi-ExecStart unit should expose the first argv, got %q", config)
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

func TestExecArgsNoArgv(t *testing.T) {
	if got := execArgs("garbage"); got != nil {
		t.Errorf("no argv: %v", got)
	}
}

// Captured with `busctl --json=short get-property org.freedesktop.systemd1
// <unit> org.freedesktop.systemd1.Service ExecStart` on Ubuntu 24.04: Caddy's
// packaged unit, and a probe unit whose --config value contains a space.
const busCaddy = `{"type":"a(sasbttttuii)","data":[["/usr/bin/caddy",["/usr/bin/caddy","run","--environ","--config","/etc/caddy/Caddyfile"],false,0,0,0,0,0,0,0]]}`
const busSpace = `{"type":"a(sasbttttuii)","data":[["/usr/bin/true",["/usr/bin/true","--config","/srv/my site/Caddyfile"],false,0,0,0,0,0,0,0]]}`

func TestParseBusExecStart(t *testing.T) {
	cmds, err := parseBusExecStart([]byte(busCaddy))
	if err != nil || len(cmds) != 1 || cmds[0].Path != "/usr/bin/caddy" || len(cmds[0].Args) != 5 {
		t.Fatalf("caddy: %+v %v", cmds, err)
	}
	cmds, err = parseBusExecStart([]byte(busSpace))
	if err != nil || len(cmds) != 1 {
		t.Fatalf("space: %+v %v", cmds, err)
	}
	u := Unit{Args: cmds[0].Args}
	if config, _, _ := u.ConfigArgs(); config != "/srv/my site/Caddyfile" {
		t.Errorf("an argument with a space must survive: %q", config)
	}
	// The flattened systemctl rendering of the same unit loses it.
	flat := Unit{Args: execArgs("path=/usr/bin/true ; argv[]=/usr/bin/true --config /srv/my site/Caddyfile ; ignore_errors=no")}
	if config, _, _ := flat.ConfigArgs(); config == "/srv/my site/Caddyfile" {
		t.Error("the flattened form was not expected to preserve the space; the D-Bus path exists for that")
	}
	if _, err := parseBusExecStart([]byte(`{"data":[["/x"]]}`)); err == nil {
		t.Error("a short tuple must be an error")
	}
	if _, err := parseBusExecStart([]byte(`junk`)); err == nil {
		t.Error("junk must be an error")
	}
}

func TestUnitObjectPath(t *testing.T) {
	cases := map[string]string{
		"caddy.service":          "/org/freedesktop/systemd1/unit/caddy_2eservice",
		"uc-space-probe.service": "/org/freedesktop/systemd1/unit/uc_2dspace_2dprobe_2eservice",
		"a@b.service":            "/org/freedesktop/systemd1/unit/a_40b_2eservice",
	}
	for in, want := range cases {
		if got := unitObjectPath(in); got != want {
			t.Errorf("unitObjectPath(%q)=%q want %q", in, got, want)
		}
	}
}
