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

func TestNewerMajor(t *testing.T) {
	c := newServer(t, map[string]string{
		"/example.com/m/@latest":    "v1.9.0",
		"/example.com/m/v2/@latest": "v2.1.0",
		"/example.com/m/v3/@latest": "v3.0.1",
		"/example.com/n/v2/@latest": "v2.0.0",
	})
	ctx := context.Background()

	path, info, ok, err := c.NewerMajor(ctx, "example.com/m")
	if err != nil || !ok || path != "example.com/m/v3" || info.Version != "v3.0.1" {
		t.Errorf("from v1: ok=%v path=%q ver=%q err=%v", ok, path, info.Version, err)
	}
	path, info, ok, err = c.NewerMajor(ctx, "example.com/m/v2")
	if err != nil || !ok || path != "example.com/m/v3" || info.Version != "v3.0.1" {
		t.Errorf("from v2: ok=%v path=%q ver=%q err=%v", ok, path, info.Version, err)
	}
	_, _, ok, err = c.NewerMajor(ctx, "example.com/m/v3")
	if err != nil || ok {
		t.Errorf("from v3: expected none, ok=%v err=%v", ok, err)
	}
	_, _, ok, err = c.NewerMajor(ctx, "example.com/n/v2")
	if err != nil || ok {
		t.Errorf("n/v2: expected none, ok=%v err=%v", ok, err)
	}
	_, _, ok, err = c.NewerMajor(ctx, "gopkg.in/yaml.v2")
	if err != nil || ok {
		t.Errorf("gopkg.in: expected skip, ok=%v err=%v", ok, err)
	}
}
