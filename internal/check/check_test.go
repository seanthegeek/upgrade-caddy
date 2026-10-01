package check

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/seanthegeek/upgrade-caddy/internal/goproxy"
)

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

func fakeProxy(t *testing.T, versions map[string]string, broken ...string) *goproxy.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, b := range broken {
			if r.URL.Path == b {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
		}
		if v, ok := versions[r.URL.Path]; ok {
			w.Write([]byte(`{"Version":"` + v + `","Time":"2026-01-01T00:00:00Z"}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return &goproxy.Client{Sources: goproxy.ParseGOPROXY(srv.URL), HTTP: srv.Client()}
}

func TestResolveStatus(t *testing.T) {
	ctx := context.Background()

	// Newer within the major, but the major probe fails: still outdated,
	// and the probe failure is a hard error so check exits 1.
	p := fakeProxy(t, map[string]string{"/example.com/m/@latest": "v1.2.0"}, "/example.com/m/v2/@latest")
	s := Status{Name: "m", Package: "example.com/m", Installed: "v1.0.0"}
	resolveStatus(ctx, p, &s)
	if !s.NewerInMajor || !s.Outdated || s.Latest != "v1.2.0" {
		t.Errorf("failed major probe must not hide a within-major update: %+v", s)
	}
	if !strings.Contains(s.Error, "could not probe") {
		t.Errorf("failed major probe must be an error, not a warning: %+v", s)
	}
	// Current within the major, probe fails: not outdated, but an error.
	s = Status{Name: "m", Package: "example.com/m", Installed: "v1.2.0"}
	resolveStatus(ctx, p, &s)
	if s.Outdated || s.Error == "" {
		t.Errorf("current but unprobeable must be an error: %+v", s)
	}
	// A hard failure on the lookup itself is an error, not "current".
	s = Status{Name: "m", Package: "example.com/m", Installed: "v1.0.0"}
	resolveStatus(ctx, fakeProxy(t, nil, "/example.com/m/@latest"), &s)
	if s.Error == "" || s.Outdated {
		t.Errorf("hard lookup failure: %+v", s)
	}
	// Cannot look up: a note, not an error.
	s = Status{Name: "m", Package: "example.com/m", Installed: "v1.0.0"}
	resolveStatus(ctx, &goproxy.Client{Sources: goproxy.ParseGOPROXY("off")}, &s)
	if s.Error != "" || s.Note == "" || s.Outdated {
		t.Errorf("GOPROXY=off: %+v", s)
	}
	// Current within the major, newer major available: outdated.
	p = fakeProxy(t, map[string]string{"/example.com/m/@latest": "v1.0.0", "/example.com/m/v2/@latest": "v2.0.0"})
	s = Status{Name: "m", Package: "example.com/m", Installed: "v1.0.0"}
	resolveStatus(ctx, p, &s)
	if s.NewerInMajor || !s.Outdated || s.MajorAvailable == nil || s.MajorAvailable.Behind != 1 {
		t.Errorf("newer major: %+v", s)
	}
}

func TestAnyErrors(t *testing.T) {
	r := &Report{Caddy: Status{Name: "caddy"}, Plugins: []Status{{Name: "p", Error: "boom"}}}
	if !r.anyErrors() {
		t.Error("a component error must be reported")
	}
	if (&Report{Caddy: Status{Name: "caddy", Note: "GOPROXY=off"}}).anyErrors() {
		t.Error("a not-checked note is not an error")
	}
}
