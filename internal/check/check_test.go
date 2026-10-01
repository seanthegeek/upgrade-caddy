package check

import "testing"

func TestShort(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "-"},
		{"v2.11.6", "v2.11.6"},
		{"v0.0.0-20240814120000-0123456789ab", "v0.0.0-20240814-0123456789ab"},
		{"v1.2.3-0.20240814120000-0123456789ab", "v1.2.3-0.20240814-0123456789ab"},
		{"v2.10.0-0.20250101120000-abcdefabcdef+dirty", "v2.10.0-0.20250101-abcdefabcdef+dirty"},
	}
	for _, c := range cases {
		if got := short(c.in); got != c.want {
			t.Errorf("short(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestAnyOutdated(t *testing.T) {
	current := Status{Name: "caddy", Installed: "v2.11.6", Latest: "v2.11.6"}
	behind := Status{Name: "p", Installed: "v1.0.0", Latest: "v1.1.0", NewerInMajor: true, Outdated: true}
	major := Status{Name: "caddy", Installed: "v2.11.6", Latest: "v2.11.6", Outdated: true, MajorAvailable: &Major{Package: "x/v3", Version: "v3.0.0"}}

	if (&Report{Caddy: current}).anyOutdated() {
		t.Error("all current should be false")
	}
	if !(&Report{Caddy: current, Plugins: []Status{behind}}).anyOutdated() {
		t.Error("outdated plugin should be true")
	}
	if !(&Report{Caddy: major}).anyOutdated() {
		t.Error("newer major should always count as an available update")
	}
}
