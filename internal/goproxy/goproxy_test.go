package goproxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSplitMajor(t *testing.T) {
	cases := []struct {
		in    string
		base  string
		major int
	}{
		{"github.com/caddyserver/caddy/v2", "github.com/caddyserver/caddy", 2},
		{"github.com/caddy-dns/cloudflare", "github.com/caddy-dns/cloudflare", 1},
		{"example.com/v1", "example.com/v1", 1}, // v1 is never a suffix
		{"example.com/vendor", "example.com/vendor", 1},
		{"example.com/m/v10", "example.com/m", 10},
	}
	for _, c := range cases {
		b, m := SplitMajor(c.in)
		if b != c.base || m != c.major {
			t.Errorf("SplitMajor(%q)=(%q,%d) want (%q,%d)", c.in, b, m, c.base, c.major)
		}
		if got := MajorPath(b, m); got != c.in {
			t.Errorf("MajorPath(%q,%d)=%q want %q", b, m, got, c.in)
		}
	}
}

func TestEscapePath(t *testing.T) {
	if got := EscapePath("github.com/Azure/go-SDK"); got != "github.com/!azure/go-!s!d!k" {
		t.Errorf("got %q", got)
	}
}

func newServer(t *testing.T, versions map[string]string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v, ok := versions[r.URL.Path]; ok {
			w.Write([]byte(`{"Version":"` + v + `","Time":"2026-01-01T00:00:00Z"}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, HTTP: srv.Client()}
}

func TestLatestAndNotFound(t *testing.T) {
	c := newServer(t, map[string]string{"/example.com/m/@latest": "v1.2.3"})
	ctx := context.Background()
	info, err := c.Latest(ctx, "example.com/m")
	if err != nil || info.Version != "v1.2.3" {
		t.Fatalf("Latest: %v %+v", err, info)
	}
	_, err = c.Latest(ctx, "example.com/missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestNewerMajors(t *testing.T) {
	c := newServer(t, map[string]string{
		"/example.com/m/@latest":       "v1.9.0",
		"/example.com/m/v2/@latest":    "v2.1.0",
		"/example.com/m/v3/@latest":    "v3.0.1",
		"/example.com/n/v2/@latest":    "v2.0.0",
		"/example.com/skip/@latest":    "v1.0.0",
		"/example.com/skip/v4/@latest": "v4.2.0", // v2 and v3 were never published
	})
	ctx := context.Background()

	got, err := c.NewerMajors(ctx, "example.com/m")
	if err != nil || len(got) != 2 || got[0].Path != "example.com/m/v2" || got[1].Version != "v3.0.1" || got[1].Major != 3 {
		t.Errorf("from v1: %+v err=%v", got, err)
	}
	got, err = c.NewerMajors(ctx, "example.com/m/v2")
	if err != nil || len(got) != 1 || got[0].Path != "example.com/m/v3" {
		t.Errorf("from v2: %+v err=%v", got, err)
	}
	if got, err = c.NewerMajors(ctx, "example.com/m/v3"); err != nil || len(got) != 0 {
		t.Errorf("from v3: expected none, got %+v err=%v", got, err)
	}
	if got, err = c.NewerMajors(ctx, "example.com/n/v2"); err != nil || len(got) != 0 {
		t.Errorf("n/v2: expected none, got %+v err=%v", got, err)
	}
	if got, err = c.NewerMajors(ctx, "gopkg.in/yaml.v2"); err != nil || len(got) != 0 {
		t.Errorf("gopkg.in: expected skip, got %+v err=%v", got, err)
	}
	// One skipped major number is tolerated; two consecutive misses end the walk.
	got, err = c.NewerMajors(ctx, "example.com/skip")
	if err != nil || len(got) != 0 {
		t.Errorf("skip: v2 and v3 both missing should end the walk before v4, got %+v err=%v", got, err)
	}
	c2 := newServer(t, map[string]string{
		"/example.com/skip/@latest":    "v1.0.0",
		"/example.com/skip/v3/@latest": "v3.1.0", // only v2 skipped
	})
	got, err = c2.NewerMajors(ctx, "example.com/skip")
	if err != nil || len(got) != 1 || got[0].Version != "v3.1.0" {
		t.Errorf("one skipped major should be found, got %+v err=%v", got, err)
	}
}
