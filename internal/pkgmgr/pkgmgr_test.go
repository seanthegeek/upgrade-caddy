package pkgmgr

import "testing"

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
