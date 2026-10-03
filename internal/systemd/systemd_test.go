package systemd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Captured from `systemctl show -p Id -p ExecStart -p ActiveState -p SubState
// -p WorkingDirectory -p MainPID` on Ubuntu 24.04 (systemd 255): Caddy's
// packaged unit, a unit with no ExecStart, and a Type=oneshot unit with two
// ExecStart= commands, which systemd prints as two ExecStart= lines.
// sample is `systemctl show` output for three units. The caddy.service
// block, User= to SupplementaryGroups= included, was captured from Ubuntu's
// caddy package under systemd 255; custom.service's DynamicUser=yes and
// SupplementaryGroups= lines use the same boolean and list rendering as
// the captured DynamicUser=no and ExecSearchPath= lines.
const sample = `ExecStart={ path=/usr/bin/caddy ; argv[]=/usr/bin/caddy run --environ --config /etc/caddy/Caddyfile ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }
Id=caddy.service
ActiveState=active
SubState=running
WorkingDirectory=
User=caddy
Group=caddy
DynamicUser=no
SupplementaryGroups=
Environment=
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
DynamicUser=yes
SupplementaryGroups=adm www-data
Environment=FOO=bar BAZ=qux
EnvironmentFiles=/etc/caddy-ci/env (ignore_errors=no)
EnvironmentFiles=/etc/caddy-ci/env.d/*.conf (ignore_errors=yes)
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
	if c.User != "caddy" || c.Group != "caddy" || c.DynamicUser || len(c.SupplementaryGroups) != 0 {
		t.Errorf("caddy unit account: %+v", c)
	}
	if u := units[1]; !u.DynamicUser || !reflect.DeepEqual(u.SupplementaryGroups, []string{"adm", "www-data"}) || u.User != "" {
		t.Errorf("custom unit account: %+v", u)
	}
	// Environment= and EnvironmentFiles= (rendered "path (ignore_errors=..)"
	// per line, systemctl-show.c) are kept; the first unit sets neither.
	if u := units[1]; !reflect.DeepEqual(u.Environment, []string{"FOO=bar", "BAZ=qux"}) ||
		!reflect.DeepEqual(u.EnvironmentFiles, []EnvFile{{Path: "/etc/caddy-ci/env"}, {Path: "/etc/caddy-ci/env.d/*.conf", Optional: true}}) {
		t.Errorf("custom unit environment: %+v %+v", u.Environment, u.EnvironmentFiles)
	}
	if len(c.Environment) != 0 || len(c.EnvironmentFiles) != 0 {
		t.Errorf("caddy unit sets no environment: %+v %+v", c.Environment, c.EnvironmentFiles)
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

// Captured from `systemctl show -p Id -p ExecStart -p ActiveState -p SubState
// -p WorkingDirectory -p ExecSearchPath -p MainPID` on Ubuntu 24.04 (systemd
// 255) for a throwaway user-scope unit with `ExecStart=true run --config
// /etc/caddy/Caddyfile` and two ExecSearchPath= lines: a bare executable
// name is rendered as-is, the search path space-separated, and the user
// manager's default working directory as "!" (missing-ok) plus the home.
const bareSample = `MainPID=0
ExecStart={ path=true ; argv[]=true run --config /etc/caddy/Caddyfile ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }
WorkingDirectory=!/home/sean
ExecSearchPath=/opt/probe /usr/bin
Id=uc-bare-probe.service
ActiveState=inactive
SubState=dead
`

// Captured with `busctl --user --json=short get-property
// org.freedesktop.systemd1 <unit> org.freedesktop.systemd1.Service
// ExecSearchPath` for the same unit.
const busSearchPath = `{"type":"as","data":["/opt/probe","/usr/bin"]}`

func TestParseShowBareNameAndSearchPath(t *testing.T) {
	units := parseShow(bareSample)
	if len(units) != 1 {
		t.Fatalf("got %d units: %+v", len(units), units)
	}
	u := units[0]
	if len(u.Commands) != 1 || u.Commands[0].Path != "true" {
		t.Errorf("a bare executable name must be kept as given: %+v", u.Commands)
	}
	if want := []string{"/opt/probe", "/usr/bin"}; !reflect.DeepEqual(u.ExecSearchPath, want) {
		t.Errorf("ExecSearchPath: %v", u.ExecSearchPath)
	}
	if u.WorkingDirectory != "/home/sean" {
		t.Errorf("the missing-ok marker must be stripped from WorkingDirectory: %q", u.WorkingDirectory)
	}
	if got := parseShow("Id=x.service\nExecStart={ path=/x ; argv[]=/x }\nWorkingDirectory=~\n")[0].WorkingDirectory; got != UnresolvedHome {
		t.Errorf("the home marker must be kept for resolution: %q", got)
	}
	dirs, err := parseBusStrings([]byte(busSearchPath))
	if err != nil || !reflect.DeepEqual(dirs, []string{"/opt/probe", "/usr/bin"}) {
		t.Errorf("bus search path: %v %v", dirs, err)
	}
	if _, err := parseBusStrings([]byte("junk")); err == nil {
		t.Error("junk must be an error")
	}
}

func TestPreciseEnvironmentFailsClosed(t *testing.T) {
	boom := errors.New("busctl: no such property")
	envOK := func() ([]string, error) { return []string{"FOO=a b"}, nil }
	envBad := func() ([]string, error) { return nil, boom }
	filesOK := func() ([]EnvFile, error) { return []EnvFile{{Path: "/etc/my env", Optional: true}}, nil }
	filesBad := func() ([]EnvFile, error) { return nil, boom }
	// The rendering split "FOO=a b" in two; the bus value replaces it.
	u := Unit{Name: "x.service", Environment: []string{"FOO=a", "b"}, EnvironmentFiles: []EnvFile{{Path: "/etc/my"}}}
	if err := preciseEnvironment(&u, envOK, filesOK); err != nil || !reflect.DeepEqual(u.Environment, []string{"FOO=a b"}) || u.EnvironmentFiles[0].Path != "/etc/my env" {
		t.Errorf("bus values must replace the rendering: %v %+v", err, u)
	}
	// With settings rendered and the bus unreadable, refuse rather than
	// validate with a guessed environment.
	u = Unit{Name: "x.service", Environment: []string{"FOO=a", "b"}}
	if err := preciseEnvironment(&u, envBad, filesOK); err == nil || !strings.Contains(err.Error(), "x.service") || !strings.Contains(err.Error(), "Environment=") {
		t.Errorf("unreadable Environment= with settings rendered must fail: %v", err)
	}
	u = Unit{Name: "x.service", EnvironmentFiles: []EnvFile{{Path: "/etc/my"}}}
	if err := preciseEnvironment(&u, envOK, filesBad); err == nil || !strings.Contains(err.Error(), "EnvironmentFiles=") {
		t.Errorf("unreadable EnvironmentFiles= with settings rendered must fail: %v", err)
	}
	// Nothing rendered: nothing to get wrong, and the bus is not consulted.
	u = Unit{Name: "x.service"}
	if err := preciseEnvironment(&u, envBad, filesBad); err != nil || len(u.Environment) != 0 || len(u.EnvironmentFiles) != 0 {
		t.Errorf("no settings must need no bus: %v %+v", err, u)
	}
}

func TestResolvesBareName(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	sbin := filepath.Join(dir, "sbin")
	os.Mkdir(bin, 0o755)
	os.Mkdir(sbin, 0o755)
	caddy := filepath.Join(bin, "caddy")
	os.WriteFile(caddy, []byte("x"), 0o755)
	want, _ := filepath.EvalSymlinks(caddy)
	linkToCaddy := filepath.Join(bin, "link")
	os.Symlink("caddy", linkToCaddy)
	dangling := filepath.Join(bin, "dangling")
	os.Symlink("caddy-next", dangling)
	// A non-executable file earlier in the path is skipped, as systemd
	// skips it; a relative entry is ignored; an absent name never matches.
	os.WriteFile(filepath.Join(sbin, "caddy"), []byte("x"), 0o644)
	search := []string{"relative/dir", filepath.Join(dir, "missing"), sbin, bin}
	cases := []struct {
		name   string
		path   string
		search []string
		want   bool
	}{
		{"bare name found", "caddy", search, true},
		{"bare name absent", "nope", search, false},
		{"bare name, no search path", "caddy", nil, false},
		{"bare name, only the non-executable copy", "caddy", []string{sbin}, false},
		{"absolute path ignores the search path", caddy, nil, true},
		{"link to the binary", linkToCaddy, nil, true},
		{"dangling link to the future binary, other target", dangling, nil, false},
		{"relative path with a slash is not searched", "bin/caddy", search, false},
	}
	// A unit whose ExecStart is a link to a binary that is about to be
	// installed for the first time is found when the install target
	// resolves to the same future file.
	if !resolves(dangling, filepath.Join(bin, "caddy-next"), nil) {
		t.Error("a dangling ExecStart link must match the future binary it points to")
	}
	// So is a bare name whose future file is the install target: systemd
	// will find it in that directory once it exists. An executable of that
	// name earlier in the search path still wins, and a future file in
	// another directory than the target's is not it.
	future := filepath.Join(bin, "caddy-new")
	if !resolves("caddy-new", future, []string{sbin, bin}) {
		t.Error("a bare name must match the target about to be installed in its search path")
	}
	os.WriteFile(filepath.Join(sbin, "caddy-new"), []byte("x"), 0o755)
	if resolves("caddy-new", future, []string{sbin, bin}) {
		t.Error("an existing executable earlier in the search path wins over the future target")
	}
	if resolves("caddy-new", filepath.Join(dir, "elsewhere", "caddy-new"), []string{bin}) {
		t.Error("a future file outside the search path is not a match")
	}
	for _, c := range cases {
		if got := resolves(c.path, want, c.search); got != c.want {
			t.Errorf("%s: resolves(%q)=%v want %v", c.name, c.path, got, c.want)
		}
	}
	// A symlinked search directory still resolves to the real file.
	link := filepath.Join(dir, "linkbin")
	os.Symlink(bin, link)
	if !resolves("caddy", want, []string{link}) {
		t.Error("a symlinked search directory must resolve to the real file")
	}
}

// Captured from `systemctl show -p Id -p User -p WorkingDirectory
// caddy.service` on Ubuntu 24.04 (systemd 255): the packaged unit sets
// User= and no WorkingDirectory=, which systemd renders empty.
const userSample = `WorkingDirectory=
User=caddy
Id=caddy.service
`

func TestResolveWorkingDirectory(t *testing.T) {
	homes := func(name string) (string, error) {
		if name == "caddy" {
			return "/var/lib/caddy", nil
		}
		if name == "" {
			return "/root", nil
		}
		return "", errors.New("no such user")
	}
	u := parseShow(userSample + "ExecStart={ path=/usr/bin/caddy ; argv[]=/usr/bin/caddy run }\n")[0]
	if u.User != "caddy" {
		t.Fatalf("User= must be parsed: %+v", u)
	}
	cases := []struct {
		name, wd, user, want string
	}{
		{"unset means the root directory, as systemd's empty_to_root", "", "caddy", "/"},
		{"home of the unit user", "~", "caddy", "/var/lib/caddy"},
		{"home of root when User= is unset", "~", "", "/root"},
		{"unknown user stays unresolved", "~", "nobody-here", UnresolvedHome},
		{"explicit directory kept", "/srv", "caddy", "/srv"},
	}
	for _, c := range cases {
		u := Unit{WorkingDirectory: c.wd, User: c.user}
		resolveWorkingDirectory(&u, homes)
		if u.WorkingDirectory != c.want {
			t.Errorf("%s: got %q want %q", c.name, u.WorkingDirectory, c.want)
		}
	}
	// The real lookup: root by empty name and by ID agree, and a user that
	// does not exist is an error, not a directory.
	byEmpty, err := homeOf("")
	if err != nil {
		t.Skipf("root lookup unavailable here: %v", err)
	}
	if byID, err := homeOf("0"); err != nil || byID != byEmpty || byEmpty == "" {
		t.Errorf("root by empty name %q and by id %q should agree: %v", byEmpty, byID, err)
	}
	if _, err := homeOf("no-such-user-upgrade-caddy"); err == nil {
		t.Error("an unknown user must be an error")
	}
}

func TestSearchPathForReadsBusBeforeMatching(t *testing.T) {
	// A search directory with a space: the flattened rendering splits
	// it, the bus keeps it. The bus value must be used before matching.
	u := Unit{Name: "x.service", ExecSearchPath: []string{"/opt/my", "caddy", "/usr/bin"}}
	bus := func(_ context.Context, name string) ([]string, error) {
		if name != "x.service" {
			t.Errorf("asked for %q", name)
		}
		return []string{"/opt/my caddy", "/usr/bin"}, nil
	}
	if got := searchPathFor(context.Background(), &u, bus); !reflect.DeepEqual(got, []string{"/opt/my caddy", "/usr/bin"}) {
		t.Errorf("bus search path must win: %v", got)
	}
	if !reflect.DeepEqual(u.ExecSearchPath, []string{"/opt/my caddy", "/usr/bin"}) {
		t.Errorf("the unit must carry the exact value: %v", u.ExecSearchPath)
	}
	// Bus unavailable: the flattened value is the fallback.
	u = Unit{Name: "y.service", ExecSearchPath: []string{"/opt/probe", "/usr/bin"}}
	broken := func(context.Context, string) ([]string, error) { return nil, errors.New("no bus") }
	if got := searchPathFor(context.Background(), &u, broken); !reflect.DeepEqual(got, []string{"/opt/probe", "/usr/bin"}) {
		t.Errorf("fallback: %v", got)
	}
	// No ExecSearchPath= at all: nil, and the bus is not asked.
	u = Unit{Name: "z.service"}
	asked := false
	spy := func(context.Context, string) ([]string, error) { asked = true; return nil, nil }
	if got := searchPathFor(context.Background(), &u, spy); got != nil || asked {
		t.Errorf("no search path set: got %v asked=%v", got, asked)
	}
}

func TestParseSearchPath(t *testing.T) {
	got := parseSearchPath("/usr/local/sbin:/usr/local/bin::relative:/usr/bin")
	if want := []string{"/usr/local/sbin", "/usr/local/bin", "/usr/bin"}; !reflect.DeepEqual(got, want) {
		t.Errorf("parseSearchPath: %v", got)
	}
	if got := parseSearchPath(defaultSearchPathCompat); len(got) != 6 || got[0] != "/usr/local/sbin" || got[5] != "/bin" {
		t.Errorf("compiled-in fallback: %v", got)
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
