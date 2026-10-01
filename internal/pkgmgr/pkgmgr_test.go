package pkgmgr

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

func TestParseDpkgSearch(t *testing.T) {
	cases := []struct{ in, want string }{
		{"caddy: /usr/bin/caddy", "caddy"},
		{"caddy:amd64: /usr/bin/caddy", "caddy"},
		{"diversion by caddy from: /usr/bin/caddy\ndiversion by caddy to: /usr/bin/caddy.default\ncaddy: /usr/bin/caddy", "caddy"},
		{"local diversion from: /usr/bin/caddy\nlocal diversion to: /usr/bin/caddy.real\ncaddy: /usr/bin/caddy", "caddy"},
		{"caddy, caddy-custom: /usr/bin/caddy", "caddy"}, // several owners
		{"caddy:amd64, other:amd64: /usr/bin/caddy", "caddy"},
		{"dpkg-query: no path found matching pattern /opt/caddy", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := parseDpkgSearch(c.in); got != c.want {
			t.Errorf("parseDpkgSearch(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestParseRpmQuery(t *testing.T) {
	o := parseRpmQuery("caddy 2.8.4-1.fc40\n")
	if o == nil || o.Manager != "rpm" || o.Package != "caddy" || o.Version != "2.8.4-1.fc40" {
		t.Errorf("got %+v", o)
	}
	// Two owning packages print two lines; the first wins.
	o = parseRpmQuery("caddy 2.8.4-1.fc40\nother 1.0-1.fc40\n")
	if o == nil || o.Package != "caddy" || o.Version != "2.8.4-1.fc40" {
		t.Errorf("multi-owner: got %+v", o)
	}
	if parseRpmQuery("") != nil {
		t.Error("empty output should give nil")
	}
}

func TestParsePacmanOwner(t *testing.T) {
	o := parsePacmanOwner("/usr/bin/caddy is owned by caddy 2.8.4-1\n")
	if o == nil || o.Manager != "pacman" || o.Package != "caddy" || o.Version != "2.8.4-1" {
		t.Errorf("got %+v", o)
	}
	if parsePacmanOwner("error: No package owns /opt/caddy") != nil {
		t.Error("unowned file should give nil")
	}
}

func TestParseApkOwner(t *testing.T) {
	o := parseApkOwner("/usr/bin/caddy is owned by caddy-2.8.4-r0\n")
	if o == nil || o.Manager != "apk" || o.Package != "caddy" || o.Version != "2.8.4-r0" {
		t.Errorf("got %+v", o)
	}
	// A package name containing a dash before the version.
	o = parseApkOwner("/usr/bin/x is owned by caddy-dns-1.0.0-r1")
	if o == nil || o.Package != "caddy-dns" || o.Version != "1.0.0-r1" {
		t.Errorf("dashed name: got %+v", o)
	}
	if parseApkOwner("ERROR: /opt/caddy: Could not find owner package") != nil {
		t.Error("unowned file should give nil")
	}
}

func TestParseOwnerEdgeCases(t *testing.T) {
	if parsePacmanOwner("/usr/bin/caddy is owned by ") != nil {
		t.Error("pacman with no package name should give nil")
	}
	if parseApkOwner("/usr/bin/caddy is owned by ") != nil {
		t.Error("apk with no package name should give nil")
	}
	// No "-digit" boundary: the whole string is the package, no version.
	if o := parseApkOwner("/usr/bin/caddy is owned by caddy"); o == nil || o.Package != "caddy" || o.Version != "" {
		t.Errorf("apk without version: %+v", o)
	}
}

// Exit codes and messages captured from each manager when asked about a
// file no package owns, and a few failure shapes.
func TestClassifyFailsClosed(t *testing.T) {
	// dpkg
	if pkg, err := classifyDpkg(result{code: 1, stderr: "dpkg-query: no path found matching pattern /opt/caddy"}); err != nil || pkg != "" {
		t.Errorf("dpkg not owned: %q %v", pkg, err)
	}
	if pkg, err := classifyDpkg(result{code: 0, stdout: "caddy: /usr/bin/caddy"}); err != nil || pkg != "caddy" {
		t.Errorf("dpkg owned: %q %v", pkg, err)
	}
	if _, err := classifyDpkg(result{code: 0, stdout: "diversion by x from: /usr/bin/caddy"}); err == nil {
		t.Error("dpkg exit 0 with no owner line is unknown, not 'not owned'")
	}
	if _, err := classifyDpkg(result{code: 2, stderr: "dpkg-query: error: database locked"}); err == nil {
		t.Error("dpkg other failure must be an error")
	}
	if _, err := classifyDpkg(result{code: 1, stderr: "Kein Pfad gefunden"}); err == nil {
		t.Error("dpkg exit 1 without the known message must be an error")
	}
	// rpm
	if o, err := classifyRpm(result{code: 1, stdout: "file /opt/caddy is not owned by any package"}); err != nil || o != nil {
		t.Errorf("rpm not owned: %+v %v", o, err)
	}
	if o, err := classifyRpm(result{code: 0, stdout: "caddy 2.8.4-1.fc40\n"}); err != nil || o == nil || o.Package != "caddy" {
		t.Errorf("rpm owned: %+v %v", o, err)
	}
	if _, err := classifyRpm(result{code: 1, stderr: "error: rpmdb open failed"}); err == nil {
		t.Error("rpm other failure must be an error")
	}
	// pacman
	if o, err := classifyPacman(result{code: 1, stderr: "error: No package owns /opt/caddy"}); err != nil || o != nil {
		t.Errorf("pacman not owned: %+v %v", o, err)
	}
	if _, err := classifyPacman(result{code: 1, stderr: "error: could not open file"}); err == nil {
		t.Error("pacman other failure must be an error")
	}
	// apk
	if o, err := classifyApk(result{code: 1, stderr: "ERROR: /opt/caddy: Could not find owner package"}); err != nil || o != nil {
		t.Errorf("apk not owned: %+v %v", o, err)
	}
	if _, err := classifyApk(result{code: -1}); err == nil {
		t.Error("apk command failure must be an error")
	}
}

func TestFindCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Find(ctx, "/usr/bin/true")
	if _, hasMgr := anyManager(); hasMgr && !errors.Is(err, ErrUnknown) {
		t.Errorf("a cancelled context must not read as 'not owned': %v", err)
	}
}

func anyManager() (string, bool) {
	for _, b := range []string{"dpkg", "rpm", "pacman", "apk"} {
		if _, err := exec.LookPath(b); err == nil {
			return b, true
		}
	}
	return "", false
}
