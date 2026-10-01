package goproxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
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
		{"gopkg.in/yaml.v3", "gopkg.in/yaml", 3},
		{"gopkg.in/yaml.v1", "gopkg.in/yaml", 1},
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
	// A leading zero is not a valid suffix, so it is not a major version.
	if b, m := SplitMajor("example.com/m/v02"); m != 1 || b != "example.com/m/v02" {
		t.Errorf("leading zero: (%q,%d)", b, m)
	}
}

func TestEscapePath(t *testing.T) {
	if got := EscapePath("github.com/Azure/go-SDK"); got != "github.com/!azure/go-!s!d!k" {
		t.Errorf("got %q", got)
	}
}

func TestParseGOPROXY(t *testing.T) {
	cases := map[string][]Source{
		"":                                {{URL: "https://proxy.golang.org"}, {URL: "direct"}},
		"https://proxy.golang.org,direct": {{URL: "https://proxy.golang.org"}, {URL: "direct"}},
		"https://a.example/|https://b.example/,direct": {{URL: "https://a.example", FallbackOnAnyError: true}, {URL: "https://b.example"}, {URL: "direct"}},
		"off":                       {{URL: "off"}},
		"direct":                    {{URL: "direct"}},
		" https://a.example , off ": {{URL: "https://a.example"}, {URL: "off"}},
		// A host without a scheme gets https://, as the go command does;
		// absolute paths and full URLs are left alone.
		"proxy.example.com,direct": {{URL: "https://proxy.example.com"}, {URL: "direct"}},
		"localhost:3000":           {{URL: "https://localhost:3000"}},
		"file:///srv/proxy/":       {{URL: "file:///srv/proxy"}},
		"/srv/proxy":               {{URL: "/srv/proxy"}},
	}
	for in, want := range cases {
		if got := ParseGOPROXY(in); !reflect.DeepEqual(got, want) {
			t.Errorf("ParseGOPROXY(%q)=%+v want %+v", in, got, want)
		}
	}
}

// newServer serves @latest for the given module paths and 404 for the rest;
// paths listed in broken answer 500.
func newServer(t *testing.T, versions map[string]string, broken ...string) *httptest.Server {
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
	return srv
}

func client(goproxy, noProxy string) *Client {
	return &Client{Sources: ParseGOPROXY(goproxy), NoProxy: noProxy}
}

func TestLatestAndNotFound(t *testing.T) {
	srv := newServer(t, map[string]string{"/example.com/m/@latest": "v1.2.3"})
	ctx := context.Background()
	c := client(srv.URL+",direct", "")
	info, err := c.Latest(ctx, "example.com/m")
	if err != nil || info.Version != "v1.2.3" {
		t.Fatalf("Latest: %v %+v", err, info)
	}
	// Not on the proxy, and direct is not consulted: still ErrNotFound.
	_, err = c.Latest(ctx, "example.com/missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	_, err = c.Latest(ctx, "example.com/Bad Path")
	if err == nil {
		t.Error("invalid module path should fail before any request")
	}
}

func TestLatestKeywordsAndPrivate(t *testing.T) {
	ctx := context.Background()
	if _, err := client("off", "").Latest(ctx, "example.com/m"); !errors.Is(err, ErrOff) {
		t.Errorf("GOPROXY=off: %v", err)
	}
	if _, err := client("direct", "").Latest(ctx, "example.com/m"); !errors.Is(err, ErrDirect) {
		t.Errorf("GOPROXY=direct: %v", err)
	}
	srv := newServer(t, map[string]string{"/corp.example/m/@latest": "v9.9.9"})
	_, err := client(srv.URL, "corp.example/*").Latest(ctx, "corp.example/m")
	if !errors.Is(err, ErrPrivate) {
		t.Errorf("GONOPROXY match should not be looked up: %v", err)
	}
	if reason, ok := NotChecked(err); !ok || reason == "" {
		t.Errorf("NotChecked: %q %v", reason, ok)
	}
	if _, ok := NotChecked(errors.New("other")); ok {
		t.Error("unrelated error is not a not-checked reason")
	}
}

func TestLatestFallback(t *testing.T) {
	ctx := context.Background()
	primary := newServer(t, map[string]string{"/a.example/m/@latest": "v1.0.0"}, "/b.example/m/@latest")
	secondary := newServer(t, map[string]string{"/b.example/m/@latest": "v1.2.0", "/c.example/m/@latest": "v1.3.0"})

	// Comma: fall back on 404 only. c is missing on primary (404) -> secondary.
	c := client(primary.URL+","+secondary.URL, "")
	if info, err := c.Latest(ctx, "c.example/m"); err != nil || info.Version != "v1.3.0" {
		t.Errorf("comma fallback on 404: %v %+v", err, info)
	}
	// Comma: a 500 from primary is terminal.
	if _, err := c.Latest(ctx, "b.example/m"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("comma must not fall back on a 500: %v", err)
	}
	// Pipe: a 500 from primary falls through to secondary.
	p := client(primary.URL+"|"+secondary.URL, "")
	if info, err := p.Latest(ctx, "b.example/m"); err != nil || info.Version != "v1.2.0" {
		t.Errorf("pipe fallback on 500: %v %+v", err, info)
	}
	// An invalid version from the primary is an error like any other: a
	// pipe falls through to the healthy secondary, a comma does not.
	bad := newServer(t, map[string]string{"/d.example/m/@latest": "garbage"})
	good := newServer(t, map[string]string{"/d.example/m/@latest": "v1.0.0"})
	if info, err := client(bad.URL+"|"+good.URL, "").Latest(ctx, "d.example/m"); err != nil || info.Version != "v1.0.0" {
		t.Errorf("pipe must fall through an invalid-version answer: %v %+v", err, info)
	}
	if _, err := client(bad.URL+","+good.URL, "").Latest(ctx, "d.example/m"); err == nil || !strings.Contains(err.Error(), "invalid version") {
		t.Errorf("comma must treat an invalid-version answer as terminal: %v", err)
	}
	// A failure that "|" lets through, ending in direct: the go command
	// would fetch from version control, which this tool cannot, so it is
	// "not checked" rather than the proxy's error. A "," keeps the error.
	if _, err := client(primary.URL+"|direct", "").Latest(ctx, "b.example/m"); !errors.Is(err, ErrDirect) {
		t.Errorf("500 then |direct should be ErrDirect: %v", err)
	}
	if _, err := client(primary.URL+",direct", "").Latest(ctx, "b.example/m"); err == nil || errors.Is(err, ErrDirect) || errors.Is(err, ErrNotFound) {
		t.Errorf("500 then ,direct should keep the proxy error: %v", err)
	}
	// Missing everywhere, ending in direct: ErrNotFound.
	if _, err := client(primary.URL+","+secondary.URL+",direct", "").Latest(ctx, "nowhere.example/m"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing everywhere: %v", err)
	}
}

func TestLatestBadResponses(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bad.example/json/@latest":
			w.Write([]byte("not json"))
		case "/bad.example/empty/@latest":
			w.Write([]byte(`{"Version":""}`))
		case "/bad.example/garbage/@latest":
			w.Write([]byte(`{"Version":"garbage"}`))
		case "/bad.example/noncanonical/@latest":
			w.Write([]byte(`{"Version":"1.2.3"}`)) // proxies must return the v-prefixed canonical form
		case "/bad.example/wrongmajor/@latest":
			w.Write([]byte(`{"Version":"v3.0.0"}`)) // a v3 version cannot live on a bare path
		case "/bad.example/wrongmajor/v2/@latest":
			w.Write([]byte(`{"Version":"v1.0.0"}`)) // nor v1 on a /v2 path
		case "/good.example/incompatible/@latest":
			w.Write([]byte(`{"Version":"v2.0.0+incompatible"}`)) // this one is legal on a bare path
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	c := client(srv.URL, "")
	for _, m := range []string{"bad.example/json", "bad.example/empty", "bad.example/500", "bad.example/garbage", "bad.example/noncanonical", "bad.example/wrongmajor", "bad.example/wrongmajor/v2"} {
		if _, err := c.Latest(ctx, m); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("%s: want a hard error, got %v", m, err)
		}
	}
	if info, err := c.Latest(ctx, "good.example/incompatible"); err != nil || info.Version != "v2.0.0+incompatible" {
		t.Errorf("+incompatible on a bare path is valid: %v %+v", err, info)
	}
}

func TestFileProxyURLParsing(t *testing.T) {
	// Percent-escapes in the path are decoded, as url.Parse does.
	dir := filepath.Join(t.TempDir(), "proxy cache")
	mod := filepath.Join(dir, "example.com", "m")
	os.MkdirAll(mod, 0o755)
	os.WriteFile(filepath.Join(mod, "@latest"), []byte(`{"Version":"v1.0.0"}`), 0o644)
	entry := "file://" + strings.ReplaceAll(dir, " ", "%20")
	if info, err := client(entry, "").Latest(context.Background(), "example.com/m"); err != nil || info.Version != "v1.0.0" {
		t.Errorf("escaped file URL: %v %+v", err, info)
	}
	for _, bad := range []string{"file://host/srv/proxy", "file:///srv/proxy?x=1", "file:///srv/proxy#f"} {
		if _, _, err := fileProxyDir(bad); err == nil {
			t.Errorf("%q: a file URL with more than a path must be rejected", bad)
		}
	}
	if _, isFile, err := fileProxyDir("https://proxy.golang.org"); err != nil || isFile {
		t.Errorf("https entry: isFile=%v err=%v", isFile, err)
	}
}

func TestGoEnvFile(t *testing.T) {
	dir := t.TempDir()
	envfile := filepath.Join(dir, "env")
	os.WriteFile(envfile, []byte("# written by go env -w\nGOPRIVATE=corp.example/*\nGOPROXY=https://proxy.corp.example\n"), 0o644)
	reset := func() { envFileOnce = sync.Once{}; envFile = nil }
	t.Cleanup(reset)

	// Values persisted with `go env -w` are seen when the process env is unset.
	t.Setenv("GOENV", envfile)
	t.Setenv("GOPRIVATE", "")
	t.Setenv("GOPROXY", "")
	os.Unsetenv("GOPRIVATE")
	os.Unsetenv("GOPROXY")
	reset()
	if got := GoEnv("GOPRIVATE"); got != "corp.example/*" {
		t.Errorf("GOPRIVATE from GOENV file: %q", got)
	}
	c := New()
	if c.NoProxy != "corp.example/*" || c.Sources[0].URL != "https://proxy.corp.example" {
		t.Errorf("New should use the GOENV file: %+v", c)
	}
	// The process environment wins.
	t.Setenv("GOPRIVATE", "other.example/*")
	reset()
	if got := GoEnv("GOPRIVATE"); got != "other.example/*" {
		t.Errorf("process env should win: %q", got)
	}
	// GOENV=off disables the file.
	os.Unsetenv("GOPRIVATE")
	t.Setenv("GOENV", "off")
	reset()
	if got := GoEnv("GOPRIVATE"); got != "" {
		t.Errorf("GOENV=off should ignore the file: %q", got)
	}
}

func TestFileProxy(t *testing.T) {
	dir := t.TempDir()
	mod := filepath.Join(dir, "example.com", "!upper")
	os.MkdirAll(mod, 0o755)
	os.WriteFile(filepath.Join(mod, "@latest"), []byte(`{"Version":"v1.5.0","Time":"2026-01-01T00:00:00Z"}`), 0o644)
	ctx := context.Background()
	c := client("file://"+dir+",direct", "")
	info, err := c.Latest(ctx, "example.com/Upper")
	if err != nil || info.Version != "v1.5.0" {
		t.Errorf("file proxy: %v %+v", err, info)
	}
	if _, err := c.Latest(ctx, "example.com/missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing file should be not found: %v", err)
	}
	os.WriteFile(filepath.Join(mod, "@latest"), []byte("junk"), 0o644)
	if _, err := c.Latest(ctx, "example.com/Upper"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("bad JSON in a file proxy is a hard error: %v", err)
	}
}

func TestErrorsRedactProxyCredentials(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/example.com/bad/@latest" {
			w.Write([]byte(`{"Version":"garbage"}`))
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	withCreds := strings.Replace(srv.URL, "http://", "http://user:hunter2@", 1)
	c := client(withCreds, "")
	for _, m := range []string{"example.com/bad", "example.com/500"} {
		_, err := c.Latest(ctx, m)
		if err == nil {
			t.Fatalf("%s: expected an error", m)
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: error leaks the proxy password: %v", m, err)
		}
		if !strings.Contains(err.Error(), "user:xxxxx@") {
			t.Errorf("%s: error should show the redacted form: %v", m, err)
		}
	}
}

func TestNewerMajors(t *testing.T) {
	srv := newServer(t, map[string]string{
		"/example.com/m/@latest":       "v1.9.0",
		"/example.com/m/v2/@latest":    "v2.1.0",
		"/example.com/m/v3/@latest":    "v3.0.1",
		"/example.com/n/v2/@latest":    "v2.0.0",
		"/example.com/skip/@latest":    "v1.0.0",
		"/example.com/skip/v4/@latest": "v4.2.0", // v2 and v3 were never published
		"/gopkg.in/yaml.v2/@latest":    "v2.4.0",
		"/gopkg.in/yaml.v3/@latest":    "v3.0.1",
	})
	ctx := context.Background()
	c := client(srv.URL+",direct", "")

	got, err := c.NewerMajors(ctx, "example.com/m", "v1.9.0")
	if err != nil || len(got) != 2 || got[0].Path != "example.com/m/v2" || got[1].Version != "v3.0.1" || got[1].Major != 3 {
		t.Errorf("from v1: %+v err=%v", got, err)
	}
	got, err = c.NewerMajors(ctx, "example.com/m/v2", "v2.1.0")
	if err != nil || len(got) != 1 || got[0].Path != "example.com/m/v3" {
		t.Errorf("from v2: %+v err=%v", got, err)
	}
	if got, err = c.NewerMajors(ctx, "example.com/m/v3", ""); err != nil || len(got) != 0 {
		t.Errorf("from v3: expected none, got %+v err=%v", got, err)
	}
	if got, err = c.NewerMajors(ctx, "example.com/n/v2", "v2.0.0"); err != nil || len(got) != 0 {
		t.Errorf("n/v2: expected none, got %+v err=%v", got, err)
	}
	// A bare path can hold v2+incompatible: then /v2 is the same major and
	// the probe starts at /v3.
	got, err = c.NewerMajors(ctx, "example.com/m", "v2.0.0+incompatible")
	if err != nil || len(got) != 1 || got[0].Path != "example.com/m/v3" {
		t.Errorf("installed v2+incompatible on a bare path must not report /v2 as newer: %+v err=%v", got, err)
	}
	// gopkg.in uses a dot suffix and is probed like any other module.
	got, err = c.NewerMajors(ctx, "gopkg.in/yaml.v2", "v2.4.0")
	if err != nil || len(got) != 1 || got[0].Path != "gopkg.in/yaml.v3" {
		t.Errorf("gopkg.in: %+v err=%v", got, err)
	}
	// One skipped major number is tolerated; two consecutive misses end the walk.
	got, err = c.NewerMajors(ctx, "example.com/skip", "v1.0.0")
	if err != nil || len(got) != 0 {
		t.Errorf("skip: v2 and v3 both missing should end the walk before v4, got %+v err=%v", got, err)
	}
	srv2 := newServer(t, map[string]string{
		"/example.com/skip/@latest":    "v1.0.0",
		"/example.com/skip/v3/@latest": "v3.1.0", // only v2 skipped
	})
	got, err = client(srv2.URL, "").NewerMajors(ctx, "example.com/skip", "v1.0.0")
	if err != nil || len(got) != 1 || got[0].Version != "v3.1.0" {
		t.Errorf("one skipped major should be found, got %+v err=%v", got, err)
	}
	// A proxy failure other than not-found stops the probe with an error.
	srv3 := newServer(t, map[string]string{}, "/example.com/m/v2/@latest")
	if _, err := client(srv3.URL, "").NewerMajors(ctx, "example.com/m", "v1.0.0"); err == nil {
		t.Error("a 500 while probing should be reported")
	}
	if _, err := client("off", "").NewerMajors(ctx, "example.com/m", "v1.0.0"); !errors.Is(err, ErrOff) {
		t.Errorf("GOPROXY=off while probing: %v", err)
	}
}
