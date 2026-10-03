package systemd

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The inputs and expected results are systemd's own, from
// src/test/test-env-file.c (v255), so the parser agrees with systemd on
// what an EnvironmentFile= means.
func TestParseEnvFileMatchesSystemd(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"env_file_1", "a=a\na=b\na=b\na=a\nb=b\\\nc\nd= d\\\ne  \\\nf  \ng=g\\ \nh= ąęół\\ śćńźżμ \ni=i\\",
			[]string{"a=a", "b=bc", "d=de  f", "g=g ", "h=ąęół śćńźżμ", "i=i"}},
		{"env_file_2", "a=a\\\n", []string{"a=a"}},
		{"env_file_3", "#SPAMD_ARGS=\"-d --socketpath=/var/lib/bulwark/spamd \\\n#--nouser-config                                     \\\nnormal1=line\\\n111\n;normal=ignored                                      \\\nnormal2=line222\nnormal ignored                                       \\\n",
			[]string{"normal1=line111", "normal2=line222"}},
		{"env_file_4", "# Generated\n\nHWMON_MODULES=\"coretemp f71882fg\"\n\n# For compatibility reasons\n\nMODULE_0=coretemp\nMODULE_1=f71882fg",
			[]string{"HWMON_MODULES=coretemp f71882fg", "MODULE_0=coretemp", "MODULE_1=f71882fg"}},
		{"env_file_5", "a=\nb=", []string{"a=", "b="}},
		{"env_file_6", "a=\\ \\n \\t \\x \\y \\' \nb= \\$'                  \nc= ' \\n\\t\\$\\`\\\\\n'   \nd= \" \\n\\t\\$\\`\\\\\n\"   \n",
			[]string{"a= n t x y '", "b=$'", "c= \\n\\t\\$\\`\\\\\n", "d= \\n\\t$`\\\n"}},
	}
	for _, c := range cases {
		got, err := ParseEnvFile([]byte(c.in))
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %q %v, want %q", c.name, got, err, c.want)
		}
	}
	// Invalid UTF-8 and unicode noncharacters are refused, as in
	// load_env_file_invalid_utf8.
	for _, bad := range []string{"fo￾o=bar", "foo=b￿ar", "baz=hello world￾", "x=\xff"} {
		if _, err := ParseEnvFile([]byte(bad)); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func TestLoadEnvironmentMergeOrder(t *testing.T) {
	dir := t.TempDir()
	one := filepath.Join(dir, "one.env")
	two := filepath.Join(dir, "two.env")
	os.WriteFile(one, []byte("A=file1\nB=file1\n"), 0o644)
	os.WriteFile(two, []byte("B=file2\nC=file2\n"), 0o644)
	u := Unit{
		Environment:      []string{"A=unit", "D=unit"},
		EnvironmentFiles: []EnvFile{{Path: one}, {Path: filepath.Join(dir, "*.env")}, {Path: filepath.Join(dir, "missing"), Optional: true}},
	}
	got, err := LoadEnvironment(u)
	if err != nil {
		t.Fatal(err)
	}
	// Environment= first, then files in order, later assignments winning;
	// the glob matched both files, two.env last. The optional missing file
	// is skipped.
	want := []string{"A=file1", "D=unit", "B=file2", "C=file2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("merge order: got %q, want %q", got, want)
	}
	// A required file that is missing is an error, as it fails the unit.
	u.EnvironmentFiles = []EnvFile{{Path: filepath.Join(dir, "missing")}}
	if _, err := LoadEnvironment(u); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("a missing required file must be an error: %v", err)
	}
	// A file that exists but is unreadable is an error even when optional,
	// as systemd skips only the lookup of a "-" file, not a failed read.
	bad := filepath.Join(dir, "bad.env")
	os.WriteFile(bad, []byte("x=\xff"), 0o644)
	u.EnvironmentFiles = []EnvFile{{Path: bad, Optional: true}}
	if _, err := LoadEnvironment(u); err == nil {
		t.Error("an unparsable optional file must still be an error")
	}
	if got, err := LoadEnvironment(Unit{}); err != nil || len(got) != 0 {
		t.Errorf("no settings, no variables: %q %v", got, err)
	}
}

func TestParseBusEnvFiles(t *testing.T) {
	got, err := parseBusEnvFiles([]byte(`{"type":"a(sb)","data":[["/etc/caddy/env",false],["/etc/caddy/env with space",true]]}`))
	want := []EnvFile{{Path: "/etc/caddy/env"}, {Path: "/etc/caddy/env with space", Optional: true}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("bus EnvironmentFiles: %+v %v", got, err)
	}
	if _, err := parseBusEnvFiles([]byte("junk")); err == nil {
		t.Error("junk must be an error")
	}
	if f, ok := parseEnvFileShow("/etc/x (ignore_errors=yes)"); !ok || f.Path != "/etc/x" || !f.Optional {
		t.Errorf("show rendering: %+v %v", f, ok)
	}
	if _, ok := parseEnvFileShow(""); ok {
		t.Error("an empty EnvironmentFiles= line is no file")
	}
}
