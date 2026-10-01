package build

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seanthegeek/upgrade-caddy/internal/caddybin"
	"github.com/seanthegeek/upgrade-caddy/internal/goproxy"
)

// fakeProxy serves @latest for the module paths in versions and 404 for
// everything else, which is how proxy.golang.org answers for a module path
// that does not exist.
func fakeProxy(t *testing.T, versions map[string]string) *goproxy.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mod := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), "/@latest")
		if v, ok := versions[mod]; ok {
			w.Write([]byte(`{"Version":"` + v + `","Time":"2026-01-01T00:00:00Z"}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return &goproxy.Client{Sources: goproxy.ParseGOPROXY(srv.URL), HTTP: srv.Client()}
}

// installed mimics an xcaddy-built Caddy v2.10.2 with two plugins.
func installed() *caddybin.Info {
	return &caddybin.Info{
		Path: "/opt/caddy/caddy", ResolvedPath: "/opt/caddy/caddy",
		Version: "v2.10.2", MainVersion: "v2.10.2", HasModuleInfo: true,
		Plugins: []caddybin.Plugin{
			{ModuleID: "dns.providers.cloudflare", Package: "github.com/caddy-dns/cloudflare", Version: "v0.2.1"},
			{ModuleID: "http.handlers.rate_limit", Package: "github.com/mholt/caddy-ratelimit", Version: "v0.1.0"},
		},
	}
}

var versions = map[string]string{
	"github.com/caddyserver/caddy/v2":  "v2.11.6",
	"github.com/caddy-dns/cloudflare":  "v0.2.4",
	"github.com/mholt/caddy-ratelimit": "v0.1.0",
	"github.com/example/plugin":        "v1.3.0",
	"github.com/example/plugin/v2":     "v2.0.1",
	"github.com/example/other":         "v0.5.0",
}

func resolve(t *testing.T, src *caddybin.Info, opts Options) (*Plan, error) {
	t.Helper()
	opts.Proxy = fakeProxy(t, versions)
	if opts.Output == "" {
		opts.Output = filepath.Join(t.TempDir(), "caddy")
	}
	return Resolve(context.Background(), src, opts)
}

func plugin(p *Plan, pkg string) *Plugin {
	for i := range p.Plugins {
		if p.Plugins[i].Package == pkg {
			return &p.Plugins[i]
		}
	}
	return nil
}

func TestResolveDefaultsPinPluginsAndUpgradesCaddy(t *testing.T) {
	p, err := resolve(t, installed(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if p.CaddyInstalled != "v2.10.2" || p.CaddyVersion != "v2.11.6" {
		t.Errorf("caddy: %s -> %s", p.CaddyInstalled, p.CaddyVersion)
	}
	if len(p.Plugins) != 2 {
		t.Fatalf("plugins: %+v", p.Plugins)
	}
	for _, pl := range p.Plugins {
		if pl.Source != Pinned || pl.Version != pl.Installed {
			t.Errorf("plugin should be pinned: %+v", pl)
		}
	}
}

func TestResolveUpgradeByModuleIDAndPath(t *testing.T) {
	p, err := resolve(t, installed(), Options{Upgrade: []string{"dns.providers.cloudflare", "github.com/mholt/caddy-ratelimit"}})
	if err != nil {
		t.Fatal(err)
	}
	cf := plugin(p, "github.com/caddy-dns/cloudflare")
	if cf.Version != "v0.2.4" || cf.Source != Upgraded || cf.Installed != "v0.2.1" {
		t.Errorf("cloudflare: %+v", cf)
	}
	rl := plugin(p, "github.com/mholt/caddy-ratelimit")
	if rl.Version != "v0.1.0" || rl.Source != Pinned || !strings.Contains(rl.Note, "already at latest") {
		t.Errorf("ratelimit: %+v", rl)
	}
	if _, err := resolve(t, installed(), Options{Upgrade: []string{"github.com/not/installed"}}); err == nil || !strings.Contains(err.Error(), "not in the installed plugin set") {
		t.Errorf("unknown --upgrade should fail, got %v", err)
	}
}

func TestResolveUpgradeAll(t *testing.T) {
	p, err := resolve(t, installed(), Options{UpgradeAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if plugin(p, "github.com/caddy-dns/cloudflare").Source != Upgraded {
		t.Error("cloudflare should be upgraded")
	}
}

func TestResolveWith(t *testing.T) {
	p, err := resolve(t, installed(), Options{With: []string{"github.com/example/other", "github.com/caddy-dns/cloudflare@v0.2.3"}})
	if err != nil {
		t.Fatal(err)
	}
	if o := plugin(p, "github.com/example/other"); o == nil || o.Version != "v0.5.0" || o.Source != Added {
		t.Errorf("added plugin: %+v", o)
	}
	if cf := plugin(p, "github.com/caddy-dns/cloudflare"); cf.Version != "v0.2.3" || cf.Source != Overridden {
		t.Errorf("version override: %+v", cf)
	}
	// --with at the installed version changes nothing and stays pinned.
	same, err := resolve(t, installed(), Options{With: []string{"github.com/caddy-dns/cloudflare@v0.2.1"}})
	if err != nil {
		t.Fatal(err)
	}
	if cf := plugin(same, "github.com/caddy-dns/cloudflare"); cf.Source != Pinned || cf.Version != "v0.2.1" {
		t.Errorf("--with at the installed version: %+v", cf)
	}
	if len(p.Plugins) != 3 {
		t.Errorf("want 3 plugins, got %d", len(p.Plugins))
	}
}

func TestResolveWithNewMajorNeedsAllowMajor(t *testing.T) {
	src := installed()
	src.Plugins = append(src.Plugins, caddybin.Plugin{ModuleID: "x", Package: "github.com/example/plugin", Version: "v1.2.0"})

	_, err := resolve(t, src, Options{With: []string{"github.com/example/plugin/v2@v2.0.1"}})
	if err == nil || !strings.Contains(err.Error(), "--allow-major") {
		t.Fatalf("crossing a plugin major without --allow-major should fail, got %v", err)
	}
	p, err := resolve(t, src, Options{With: []string{"github.com/example/plugin/v2"}, AllowMajor: true})
	if err != nil {
		t.Fatal(err)
	}
	if plugin(p, "github.com/example/plugin") != nil {
		t.Error("old major should be replaced, not kept alongside (duplicate module registration)")
	}
	if np := plugin(p, "github.com/example/plugin/v2"); np == nil || np.Version != "v2.0.1" || np.Source != Overridden || !strings.Contains(np.Note, "major version change") {
		t.Errorf("new major: %+v", np)
	}
	if len(p.Plugins) != 3 {
		t.Errorf("want 3 plugins, got %d", len(p.Plugins))
	}
}

func TestResolveUpgradeNotesNewerMajor(t *testing.T) {
	src := installed()
	src.Plugins = []caddybin.Plugin{{ModuleID: "x", Package: "github.com/example/plugin", Version: "v1.2.0"}}
	p, err := resolve(t, src, Options{Upgrade: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	pl := plugin(p, "github.com/example/plugin")
	if pl.Version != "v1.3.0" || !strings.Contains(pl.Note, "github.com/example/plugin/v2") {
		t.Errorf("should upgrade within major and note v2: %+v", pl)
	}
}

func TestResolveCaddyMajor(t *testing.T) {
	if _, err := resolve(t, installed(), Options{CaddyVersion: "v3.0.0"}); err == nil || !strings.Contains(err.Error(), "--allow-major") {
		t.Errorf("caddy major change without --allow-major should fail, got %v", err)
	}
	p, err := resolve(t, installed(), Options{CaddyVersion: "3.0.0", AllowMajor: true})
	if err != nil || p.CaddyVersion != "v3.0.0" {
		t.Errorf("with --allow-major: %v %+v", err, p)
	}
	p, err = resolve(t, installed(), Options{CaddyVersion: "v2.11.1"})
	if err != nil || p.CaddyVersion != "v2.11.1" {
		t.Errorf("explicit same-major version: %v %+v", err, p)
	}
}

func TestResolveCaddyMajorFreshWording(t *testing.T) {
	_, err := resolve(t, nil, Options{CaddyVersion: "v3.0.0"})
	if err == nil || strings.Contains(err.Error(), "installed") || !strings.Contains(err.Error(), "builds by default") {
		t.Errorf("fresh build must not claim a Caddy is installed: %v", err)
	}
}

func TestResolveRefusesDistroBuild(t *testing.T) {
	src := &caddybin.Info{Path: "/usr/bin/caddy", ResolvedPath: "/usr/bin/caddy", Version: "2.6.2"}
	_, err := resolve(t, src, Options{})
	if err == nil || !strings.Contains(err.Error(), "--fresh") {
		t.Errorf("distro build should be refused with a pointer to --fresh, got %v", err)
	}
}

func TestResolveFresh(t *testing.T) {
	p, err := resolve(t, nil, Options{With: []string{"github.com/example/other@v0.4.0"}})
	if err != nil {
		t.Fatal(err)
	}
	if p.SourcePath != "" || p.CaddyVersion != "v2.11.6" || len(p.Plugins) != 1 || p.Plugins[0].Version != "v0.4.0" {
		t.Errorf("fresh plan: %+v", p)
	}
}

func TestResolveReplacements(t *testing.T) {
	src := installed()
	src.Plugins[0].Replace = "../cloudflare"
	_, err := resolve(t, src, Options{})
	if err == nil || !strings.Contains(err.Error(), "--replace") {
		t.Errorf("installed replacement must be covered, got %v", err)
	}
	p, err := resolve(t, src, Options{Replace: []string{"github.com/caddy-dns/cloudflare=/srv/cloudflare"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Replacements) != 1 || string(p.Replacements[0].New) != "/srv/cloudflare" {
		t.Errorf("replacements: %+v", p.Replacements)
	}
	if _, err := resolve(t, installed(), Options{Replace: []string{"garbage"}}); err == nil {
		t.Error("malformed --replace should fail")
	}
}

func TestResolveRefusesOutputOverInstalled(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "caddy")
	if err := writeFile(bin); err != nil {
		t.Fatal(err)
	}
	src := installed()
	src.Path, src.ResolvedPath = bin, bin
	_, err := resolve(t, src, Options{Output: bin})
	if err == nil || !strings.Contains(err.Error(), "use 'install'") {
		t.Errorf("output over the installed binary should be refused, got %v", err)
	}
}

func TestVerify(t *testing.T) {
	p := &Plan{CaddyVersion: "v2.11.6", Plugins: []Plugin{{Package: "github.com/caddy-dns/cloudflare", Version: "v0.2.4"}}}
	good := &caddybin.Info{HasModuleInfo: true, MainVersion: "v2.11.6", Plugins: []caddybin.Plugin{{Package: "github.com/caddy-dns/cloudflare", Version: "v0.2.4"}}}
	if err := p.verify(good); err != nil {
		t.Errorf("good build: %v", err)
	}
	bad := &caddybin.Info{HasModuleInfo: true, MainVersion: "v2.11.6"}
	if err := p.verify(bad); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("missing plugin: %v", err)
	}
	wrong := &caddybin.Info{HasModuleInfo: true, MainVersion: "v2.11.5", Plugins: good.Plugins}
	if err := p.verify(wrong); err == nil {
		t.Error("wrong caddy version should fail")
	}
	// A branch name is resolved by go get, so only presence is checked.
	p.Plugins[0].Version = "main"
	if err := p.verify(good); err != nil {
		t.Errorf("non-semver version should only check presence: %v", err)
	}
}

func TestResolveErrorBranches(t *testing.T) {
	// An empty --with path.
	if _, err := resolve(t, installed(), Options{With: []string{"@v1.0.0"}}); err == nil || !strings.Contains(err.Error(), "empty module path") {
		t.Errorf("empty --with: %v", err)
	}
	// A plugin Caddy reported an error for cannot be pinned.
	src := installed()
	src.Plugins[0].Error = "module registered twice"
	if _, err := resolve(t, src, Options{}); err == nil || !strings.Contains(err.Error(), "reported an error") {
		t.Errorf("plugin error: %v", err)
	}
	// A plugin without module info cannot be pinned.
	src = installed()
	src.Plugins[0].Version = ""
	if _, err := resolve(t, src, Options{}); err == nil || !strings.Contains(err.Error(), "cannot pin") {
		t.Errorf("plugin without version: %v", err)
	}
	// Latest Caddy unknown to the proxy.
	opts := Options{Proxy: fakeProxy(t, map[string]string{}), Output: filepath.Join(t.TempDir(), "caddy")}
	if _, err := Resolve(context.Background(), installed(), opts); err == nil || !strings.Contains(err.Error(), "looking up latest Caddy") {
		t.Errorf("caddy lookup failure: %v", err)
	}
	// --upgrade of a plugin the proxy does not know.
	src = installed()
	src.Plugins = append(src.Plugins, caddybin.Plugin{ModuleID: "x", Package: "github.com/example/unknown", Version: "v1.0.0"})
	if _, err := resolve(t, src, Options{Upgrade: []string{"x"}}); err == nil || !strings.Contains(err.Error(), "--upgrade github.com/example/unknown") {
		t.Errorf("upgrade lookup failure: %v", err)
	}
}

func TestResolveOneGoModuleSeveralCaddyModules(t *testing.T) {
	// A plugin like caddy-l4 registers many Caddy module IDs from one Go
	// module; list-modules prints one line each, and the build must pin the
	// Go module once.
	src := installed()
	src.Plugins = []caddybin.Plugin{
		{ModuleID: "layer4", Package: "github.com/example/other", Version: "v0.5.0"},
		{ModuleID: "layer4.handlers.proxy", Package: "github.com/example/other", Version: "v0.5.0"},
		{ModuleID: "layer4.matchers.tls", Package: "github.com/example/other", Version: "v0.5.0"},
	}
	p, err := resolve(t, src, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Plugins) != 1 || p.Plugins[0].ModuleID != "layer4" {
		t.Errorf("want one pinned Go module keeping the first module ID, got %+v", p.Plugins)
	}
}

func TestResolveDefaultOutputAndNotes(t *testing.T) {
	p, err := Resolve(context.Background(), nil, Options{Proxy: fakeProxy(t, versions)})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(p.Output) != "caddy" {
		t.Errorf("default output should be ./caddy, got %s", p.Output)
	}
	// Already at latest within its major, with a newer major: both notes.
	src := installed()
	src.Plugins = []caddybin.Plugin{{ModuleID: "x", Package: "github.com/example/plugin", Version: "v1.3.0"}}
	p, err = resolve(t, src, Options{Upgrade: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	pl := plugin(p, "github.com/example/plugin")
	if pl.Source != Pinned || !strings.Contains(pl.Note, "already at latest; newer major v2.0.1") {
		t.Errorf("both notes expected: %+v", pl)
	}
}

func TestVerifyMoreBranches(t *testing.T) {
	p := &Plan{CaddyVersion: "v2.11.6", Plugins: []Plugin{{Package: "github.com/caddy-dns/cloudflare", Version: "v0.2.4"}}}
	if err := p.verify(&caddybin.Info{}); err == nil || !strings.Contains(err.Error(), "no Go module information") {
		t.Errorf("no module info: %v", err)
	}
	wrongPlugin := &caddybin.Info{HasModuleInfo: true, MainVersion: "v2.11.6", Plugins: []caddybin.Plugin{{Package: "github.com/caddy-dns/cloudflare", Version: "v0.2.3"}}}
	if err := p.verify(wrongPlugin); err == nil || !strings.Contains(err.Error(), "wanted v0.2.4") {
		t.Errorf("plugin version mismatch: %v", err)
	}
}
