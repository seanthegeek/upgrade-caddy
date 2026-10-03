package build

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caddyserver/xcaddy"
	"github.com/seanthegeek/upgrade-caddy/internal/caddybin"
	"github.com/seanthegeek/upgrade-caddy/internal/goproxy"
	"github.com/seanthegeek/upgrade-caddy/internal/systemd"
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

func TestResolveBarePathMajorChange(t *testing.T) {
	// v0 and v1 share a bare module path. "Latest" being v1 while v0 is
	// installed is a major change and needs --allow-major.
	src := installed()
	src.Plugins = []caddybin.Plugin{{ModuleID: "x", Package: "github.com/example/plugin", Version: "v0.9.0"}}
	p, err := resolve(t, src, Options{Upgrade: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	pl := plugin(p, "github.com/example/plugin")
	if pl.Version != "v0.9.0" || pl.Source != Pinned || !strings.Contains(pl.Note, "new major on the same path") {
		t.Errorf("--upgrade must not cross v0 -> v1 silently: %+v", pl)
	}
	p, err = resolve(t, src, Options{Upgrade: []string{"x"}, AllowMajor: true})
	if err != nil || plugin(p, "github.com/example/plugin").Version != "v1.3.0" {
		t.Errorf("--upgrade with --allow-major should take v1: %v %+v", err, p.Plugins)
	}
	if _, err := resolve(t, src, Options{With: []string{"github.com/example/plugin"}}); err == nil || !strings.Contains(err.Error(), "--allow-major") {
		t.Errorf("--with resolving to a new major on the same path must need --allow-major: %v", err)
	}
	if _, err := resolve(t, src, Options{With: []string{"github.com/example/plugin@v1.0.0"}}); err == nil || !strings.Contains(err.Error(), "from v0 to v1") {
		t.Errorf("explicit --with across v0 -> v1 must need --allow-major: %v", err)
	}
	if p, err := resolve(t, src, Options{With: []string{"github.com/example/plugin@v1.0.0"}, AllowMajor: true}); err != nil || plugin(p, "github.com/example/plugin").Version != "v1.0.0" {
		t.Errorf("explicit --with with --allow-major: %v", err)
	}
	// v2+incompatible on the bare path and the /v2 module path are one
	// major: moving between them is a path change, not a major change, and
	// needs no --allow-major. The installed version decides, not the path.
	src.Plugins[0].Version = "v2.0.0+incompatible"
	p, err = resolve(t, src, Options{With: []string{"github.com/example/plugin/v2@v2.0.1"}})
	if err != nil {
		t.Fatalf("same major across the path change must not need --allow-major: %v", err)
	}
	if pl := plugin(p, "github.com/example/plugin/v2"); pl == nil || pl.Version != "v2.0.1" || !strings.Contains(pl.Note, "within the same major") || len(p.Plugins) != 1 {
		t.Errorf("path move within the major: %+v", p.Plugins)
	}
	// A v1 on the bare path to /v2 is still a major change.
	src.Plugins[0].Version = "v1.3.0"
	if _, err := resolve(t, src, Options{With: []string{"github.com/example/plugin/v2@v2.0.1"}}); err == nil || !strings.Contains(err.Error(), "from major v1 to v2") {
		t.Errorf("v1 to /v2 must need --allow-major: %v", err)
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
	// Major 0 is a different major from the installed v2 as well.
	if _, err := resolve(t, installed(), Options{CaddyVersion: "v0.11.5"}); err == nil || !strings.Contains(err.Error(), "--allow-major") {
		t.Errorf("v0 must not bypass the major guard: %v", err)
	}
}

func TestResolveWithCanonicalisesVersions(t *testing.T) {
	p, err := resolve(t, installed(), Options{With: []string{"github.com/example/other@0.5.0", "github.com/example/plugin@main"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := plugin(p, "github.com/example/other").Version; got != "v0.5.0" {
		t.Errorf("a semantic version without v must reach go get as %q, got %q", "v0.5.0", got)
	}
	if got := plugin(p, "github.com/example/plugin").Version; got != "main" {
		t.Errorf("a branch name must be left alone, got %q", got)
	}
}

func TestResolveCaddyMajorFreshWording(t *testing.T) {
	_, err := resolve(t, nil, Options{CaddyVersion: "v3.0.0"})
	if err == nil || strings.Contains(err.Error(), "installed") || !strings.Contains(err.Error(), "builds by default") {
		t.Errorf("fresh build must not claim a Caddy is installed: %v", err)
	}
}

func TestResolveRefusesUnknownModules(t *testing.T) {
	src := installed()
	src.UnknownModules = []caddybin.Plugin{{ModuleID: "http.handlers.mystery"}}
	_, err := resolve(t, src, Options{})
	if err == nil || !strings.Contains(err.Error(), "http.handlers.mystery") || !strings.Contains(err.Error(), "--fresh") {
		t.Errorf("modules without package info must refuse reproduction: %v", err)
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
	// Caddy itself replaced in the installed binary must be covered too,
	// or a rebuild would quietly go back to upstream Caddy.
	forked := installed()
	forked.MainPath, forked.MainReplace = caddybin.CaddyModulePath, "/srv/caddy-fork"
	_, err = resolve(t, forked, Options{})
	if err == nil || !strings.Contains(err.Error(), "Caddy itself replaced") || !strings.Contains(err.Error(), "--replace "+caddybin.CaddyModulePath+"=") {
		t.Errorf("an installed Caddy replacement must be covered, got %v", err)
	}
	p, err = resolve(t, forked, Options{Replace: []string{caddybin.CaddyModulePath + "=/srv/caddy-fork"}})
	if err != nil || len(p.Replacements) != 1 {
		t.Errorf("covered Caddy replacement: %+v %v", p, err)
	}
	// --fresh passes no source binary at all (Run hands Resolve nil), so
	// an installed replacement cannot bind a fresh build.
	if _, err := resolve(t, nil, Options{Fresh: true}); err != nil {
		t.Errorf("--fresh ignores the installed binary, replacement included: %v", err)
	}
}

func TestVerifyReplacements(t *testing.T) {
	cf := "github.com/caddy-dns/cloudflare"
	p := &Plan{CaddyVersion: "v2.11.6", Plugins: []Plugin{{Package: cf, Version: "v0.2.4"}},
		Replacements: []xcaddy.Replace{xcaddy.NewReplace(cf, "/srv/cloudflare")}}
	replaced := &caddybin.Info{HasModuleInfo: true, MainPath: caddybin.CaddyModulePath, MainVersion: "v2.11.6",
		Plugins: []caddybin.Plugin{{Package: cf, Version: "v0.2.4", Replace: "/srv/cloudflare"}}}
	if err := p.verify(replaced); err != nil {
		t.Errorf("requested replacement in effect: %v", err)
	}
	plain := &caddybin.Info{HasModuleInfo: true, MainPath: caddybin.CaddyModulePath, MainVersion: "v2.11.6",
		Plugins: []caddybin.Plugin{{Package: cf, Version: "v0.2.4"}}}
	if err := p.verify(plain); err == nil || !strings.Contains(err.Error(), "did not take effect") {
		t.Errorf("a requested replacement missing from the output must fail: %v", err)
	}
	p.Replacements = nil
	if err := p.verify(replaced); err == nil || !strings.Contains(err.Error(), "not asked for") {
		t.Errorf("an unrequested replacement must fail: %v", err)
	}
	forkedCaddy := *plain
	forkedCaddy.MainReplace, forkedCaddy.MainReplaceVer = "github.com/fork/caddy/v2", "v2.11.6-fork"
	if err := p.verify(&forkedCaddy); err == nil || !strings.Contains(err.Error(), "not asked for") {
		t.Errorf("an unrequested Caddy replacement must fail: %v", err)
	}
	p.Replacements = []xcaddy.Replace{xcaddy.NewReplace(caddybin.CaddyModulePath, "github.com/fork/caddy/v2@v2.11.6-fork")}
	if err := p.verify(&forkedCaddy); err != nil {
		t.Errorf("requested Caddy replacement in effect: %v", err)
	}
	// A branch name is resolved by go get to a pseudo-version, so only
	// the path is compared; a different path still fails.
	p.Replacements = []xcaddy.Replace{xcaddy.NewReplace(caddybin.CaddyModulePath, "github.com/fork/caddy/v2@main")}
	forkedCaddy.MainReplaceVer = "v2.11.6-0.20260101000000-abcdefabcdef"
	if err := p.verify(&forkedCaddy); err != nil {
		t.Errorf("branch replacement should compare the path only: %v", err)
	}
	forkedCaddy.MainReplace = "github.com/other/caddy/v2"
	if err := p.verify(&forkedCaddy); err == nil {
		t.Error("a replacement by a different module must fail")
	}
	// Replacements of modules that register no Caddy module are checked
	// through the output's full replacement list: a requested one must be
	// in effect, an unrequested one is refused, and a path that is in no
	// module of the build at all did not take effect.
	dep := "github.com/example/dep"
	p.Replacements = []xcaddy.Replace{xcaddy.NewReplace(dep, "/srv/dep")}
	withDep := *plain
	withDep.Replacements = map[string]string{dep: "/srv/dep"}
	if err := p.verify(&withDep); err != nil {
		t.Errorf("replacement of a non-registering dependency in effect: %v", err)
	}
	if err := p.verify(plain); err == nil || !strings.Contains(err.Error(), "did not take effect") || !strings.Contains(err.Error(), "check the spelling") {
		t.Errorf("a replacement for a module not in the build must fail: %v", err)
	}
	p.Replacements = nil
	if err := p.verify(&withDep); err == nil || !strings.Contains(err.Error(), "not asked for") {
		t.Errorf("an unrequested replacement of a dependency must fail: %v", err)
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
	// Unless the branch resolved to another major on the same bare path,
	// which Resolve could not see: that still needs --allow-major.
	p.Plugins[0].Installed = "v0.2.1"
	if err := p.verify(good); err != nil {
		t.Errorf("branch resolved within the installed major: %v", err)
	}
	crossed := &caddybin.Info{HasModuleInfo: true, MainVersion: "v2.11.6", Plugins: []caddybin.Plugin{{Package: "github.com/caddy-dns/cloudflare", Version: "v1.0.0-0.20260101000000-abcdefabcdef"}}}
	if err := p.verify(crossed); err == nil || !strings.Contains(err.Error(), "--allow-major") {
		t.Errorf("branch resolved to another major must fail without --allow-major: %v", err)
	}
	p.AllowMajor = true
	if err := p.verify(crossed); err != nil {
		t.Errorf("--allow-major lets the branch cross: %v", err)
	}
	p.AllowMajor = false
	p.Plugins[0].Installed = "" // a fresh addition has no major to keep
	if err := p.verify(crossed); err != nil {
		t.Errorf("no installed version, nothing to compare: %v", err)
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
	if len(p.Plugins) != 1 || p.Plugins[0].ModuleID != "layer4" || len(p.Plugins[0].ModuleIDs) != 3 {
		t.Errorf("want one pinned Go module keeping every module ID, got %+v", p.Plugins)
	}
	// Any of the IDs names the module to --upgrade and --drop-replace, not
	// just the first one list-modules printed.
	p, err = resolve(t, src, Options{Upgrade: []string{"layer4.handlers.proxy"}})
	if err != nil || p.Plugins[0].Note != "already at latest" {
		t.Errorf("--upgrade by a later module ID: %v %+v", err, p.Plugins)
	}
	src.Plugins[0].Replace, src.Plugins[1].Replace, src.Plugins[2].Replace = "/srv/l4", "/srv/l4", "/srv/l4"
	p, err = resolve(t, src, Options{DropReplace: []string{"layer4.matchers.tls"}})
	if err != nil || len(p.Replacements) != 0 || !strings.Contains(p.Plugins[0].Note, "dropped") {
		t.Errorf("--drop-replace by a later module ID: %v %+v", err, p.Plugins)
	}
	var text strings.Builder
	p.WriteText(&text)
	if !strings.Contains(text.String(), "layer4, layer4.handlers.proxy, layer4.matchers.tls") {
		t.Errorf("the plan should list every module ID:\n%s", text.String())
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

func TestUnplannedAndTransitiveLockfile(t *testing.T) {
	p := &Plan{CaddyVersion: "v2.11.6", Plugins: []Plugin{{Package: "github.com/example/plugin", Version: "v1.3.0", Source: Added}}}
	built := &caddybin.Info{HasModuleInfo: true, MainPath: caddybin.CaddyModulePath, MainVersion: "v2.11.6", Plugins: []caddybin.Plugin{
		{ModuleID: "a", Package: "github.com/example/plugin", Version: "v1.3.0"},
		{ModuleID: "b", Package: "github.com/example/dep", Version: "v0.4.0"}, // registered by a dependency of the plugin
		{ModuleID: "c", Package: "github.com/example/dep", Version: "v0.4.0"},
	}}
	if err := p.verify(built); err != nil {
		t.Fatalf("a transitive plugin must not fail verification: %v", err)
	}
	if got := p.Unplanned(built); len(got) != 1 || got[0] != "github.com/example/dep@v0.4.0" {
		t.Errorf("unplanned: %v", got)
	}
	f, err := os.CreateTemp(t.TempDir(), "lock-*.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeLockfile(f, p, built); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(f.Name())
	var lf Lockfile
	if err := json.Unmarshal(data, &lf); err != nil {
		t.Fatal(err)
	}
	if len(lf.Plugins) != 3 || lf.Plugins[0].Source != Added || lf.Plugins[1].Source != Transitive {
		t.Errorf("lockfile sources: %+v", lf.Plugins)
	}
}

func TestLockfileDescribes(t *testing.T) {
	built := &caddybin.Info{HasModuleInfo: true, MainPath: caddybin.CaddyModulePath, MainVersion: "v2.11.6", MainSum: "h1:caddy", Plugins: []caddybin.Plugin{
		{ModuleID: "a", Package: "github.com/example/plugin", Version: "v1.3.0", Sum: "h1:p"},
	}}
	good := &Lockfile{Schema: 1, Caddy: LockModule{Package: caddybin.CaddyModulePath, Version: "v2.11.6", Sum: "h1:caddy"},
		Plugins: []LockModule{{ModuleID: "a", Package: "github.com/example/plugin", Version: "v1.3.0", Sum: "h1:p"}}}
	if err := good.Describes(built); err != nil {
		t.Errorf("matching lockfile: %v", err)
	}
	stale := *good
	stale.Caddy.Version = "v2.11.4"
	if err := stale.Describes(built); err == nil {
		t.Error("stale Caddy version must be rejected")
	}
	wrongSum := *good
	wrongSum.Plugins = []LockModule{{Package: "github.com/example/plugin", Version: "v1.3.0", Sum: "h1:other"}}
	if err := wrongSum.Describes(built); err == nil {
		t.Error("plugin checksum mismatch must be rejected")
	}
	// The Caddy checksum must match exactly: a lockfile with the checksum
	// deleted, or one that carries a checksum the binary does not, was not
	// written for this binary.
	noSum := *good
	noSum.Caddy.Sum = ""
	if err := noSum.Describes(built); err == nil {
		t.Error("a deleted Caddy checksum must be rejected")
	}
	unsummed := *built
	unsummed.MainSum = ""
	if err := good.Describes(&unsummed); err == nil {
		t.Error("a checksum the binary does not carry must be rejected")
	}
	// A plugin replaced by a local directory has no checksum, so the
	// directory is its only identity: a lockfile naming another one is
	// for another build.
	localBuilt := *built
	localBuilt.Plugins = []caddybin.Plugin{{ModuleID: "a", Package: "github.com/example/plugin", Version: "v1.3.0", Replace: "/srv/plugin-a"}}
	localLock := *good
	localLock.Plugins = []LockModule{{ModuleID: "a", Package: "github.com/example/plugin", Version: "v1.3.0", ReplacedBy: "/srv/plugin-a"}}
	if err := localLock.Describes(&localBuilt); err != nil {
		t.Errorf("matching local replacement: %v", err)
	}
	localLock.Plugins[0].ReplacedBy = "/srv/plugin-b"
	if err := localLock.Describes(&localBuilt); err == nil {
		t.Error("a different local replacement must be rejected")
	}
	// The same for Caddy itself, which --replace can target.
	replacedCaddy := *built
	replacedCaddy.MainSum, replacedCaddy.MainReplace = "", "/srv/caddy"
	if err := good.Describes(&replacedCaddy); err == nil {
		t.Error("a replaced Caddy must not match a lockfile that records no replacement")
	}
	caddyLock := *good
	caddyLock.Caddy = LockModule{Package: caddybin.CaddyModulePath, Version: "v2.11.6", ReplacedBy: "/srv/caddy"}
	if err := caddyLock.Describes(&replacedCaddy); err != nil {
		t.Errorf("matching Caddy replacement: %v", err)
	}
	// A Go module that registers two Caddy modules appears twice in the
	// binary and must be listed twice, under its module IDs.
	twice := *built
	twice.Plugins = []caddybin.Plugin{
		{ModuleID: "a", Package: "github.com/example/plugin", Version: "v1.3.0", Sum: "h1:p"},
		{ModuleID: "b", Package: "github.com/example/plugin", Version: "v1.3.0", Sum: "h1:p"},
	}
	if err := good.Describes(&twice); err == nil {
		t.Error("one listed registration for two in the binary must be rejected")
	}
	wrongID := *good
	wrongID.Plugins = []LockModule{{ModuleID: "z", Package: "github.com/example/plugin", Version: "v1.3.0", Sum: "h1:p"}}
	if err := wrongID.Describes(built); err == nil {
		t.Error("a different module ID must be rejected")
	}
	missing := *good
	missing.Plugins = nil
	if err := missing.Describes(built); err == nil {
		t.Error("a plugin the lockfile does not list must be rejected")
	}
	empty := &Lockfile{Schema: 1}
	if err := empty.Describes(built); err == nil {
		t.Error("an empty lockfile must be rejected")
	}
	// Round trip through disk.
	dir := t.TempDir()
	f, _ := os.CreateTemp(dir, "lock-*.json")
	p := &Plan{Plugins: []Plugin{{Package: "github.com/example/plugin", Version: "v1.3.0", Source: Added}}}
	if err := writeLockfile(f, p, built); err != nil {
		t.Fatal(err)
	}
	lf, err := ReadLockfile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if err := lf.Describes(built); err != nil {
		t.Errorf("a lockfile build wrote must describe its binary: %v", err)
	}
	// And with replacements on both Caddy and a plugin, including a
	// module registering twice.
	replaced := twice
	replaced.MainReplace, replaced.MainReplaceVer = "github.com/fork/caddy/v2", "v2.11.6-fork"
	replaced.Plugins[1].Replace = "/srv/plugin"
	f2, _ := os.CreateTemp(dir, "lock-*.json")
	if err := writeLockfile(f2, p, &replaced); err != nil {
		t.Fatal(err)
	}
	lf2, err := ReadLockfile(f2.Name())
	if err != nil {
		t.Fatal(err)
	}
	if lf2.Caddy.ReplacedBy != "github.com/fork/caddy/v2@v2.11.6-fork" {
		t.Errorf("Caddy replacement must be recorded: %+v", lf2.Caddy)
	}
	if err := lf2.Describes(&replaced); err != nil {
		t.Errorf("a lockfile with replacements must describe its binary: %v", err)
	}
	os.WriteFile(f.Name(), []byte("{}"), 0o644)
	if _, err := ReadLockfile(f.Name()); err == nil {
		t.Error("{} has no schema and must be rejected")
	}
}

func TestRefuseLive(t *testing.T) {
	ctx := context.Background()
	none := func(context.Context, string) ([]systemd.Unit, error) { return nil, nil }
	if err := refuseLive(ctx, "/opt/caddy", none); err != nil {
		t.Errorf("no unit, no refusal: %v", err)
	}
	// Not being able to ask is a refusal, never "no unit".
	failing := func(context.Context, string) ([]systemd.Unit, error) { return nil, errors.New("boom") }
	if err := refuseLive(ctx, "/opt/caddy", failing); err == nil || !strings.Contains(err.Error(), "cannot confirm") {
		t.Errorf("systemd failure must refuse: %v", err)
	}
	live := func(context.Context, string) ([]systemd.Unit, error) {
		return []systemd.Unit{{Name: "caddy.service"}}, nil
	}
	err := refuseLive(ctx, "/opt/caddy", live)
	if err == nil || !strings.Contains(err.Error(), "caddy.service") || !strings.Contains(err.Error(), "'install'") {
		t.Errorf("a live binary must refuse and point at install: %v", err)
	}
}

func TestReplacementSurvivesUpgradeAndWith(t *testing.T) {
	cf := "github.com/caddy-dns/cloudflare"
	replaced := func() *caddybin.Info {
		src := installed()
		src.Plugins[0].Replace = "../cloudflare"
		return src
	}
	// Choosing a version is not choosing a source: none of these may drop
	// the installed replacement on its own.
	for name, opts := range map[string]Options{
		"--upgrade":     {Upgrade: []string{cf}},
		"--upgrade-all": {UpgradeAll: true},
		"--with":        {With: []string{cf + "@v0.2.4"}},
	} {
		_, err := resolve(t, replaced(), opts)
		if err == nil || !strings.Contains(err.Error(), "--drop-replace "+cf) || !strings.Contains(err.Error(), "--replace "+cf+"=") {
			t.Errorf("%s must not drop the replacement silently, and the error must offer both ways out: %v", name, err)
		}
	}
	// Covered, the upgrade goes ahead with the replacement kept.
	p, err := resolve(t, replaced(), Options{Upgrade: []string{cf}, Replace: []string{cf + "=/srv/cloudflare"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Replacements) != 1 || plugin(p, cf).Source != Upgraded || plugin(p, cf).Version != "v0.2.4" {
		t.Errorf("upgrade with the replacement kept: %+v %+v", p.Replacements, plugin(p, cf))
	}
	// Dropped explicitly, by Caddy module ID: no replacement, and the plan
	// says so.
	p, err = resolve(t, replaced(), Options{DropReplace: []string{"dns.providers.cloudflare"}, UpgradeAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Replacements) != 0 || !strings.Contains(plugin(p, cf).Note, "replacement by ../cloudflare dropped") {
		t.Errorf("dropped replacement: %+v %+v", p.Replacements, plugin(p, cf))
	}
	// Dropping what is not replaced, dropping and keeping at once, and
	// dropping with nothing installed are all mistakes to report.
	if _, err := resolve(t, installed(), Options{DropReplace: []string{cf}}); err == nil || !strings.Contains(err.Error(), "was not built with a replacement") {
		t.Errorf("drop of an unreplaced module: %v", err)
	}
	if _, err := resolve(t, replaced(), Options{DropReplace: []string{cf}, Replace: []string{cf + "=/srv/cloudflare"}}); err == nil || !strings.Contains(err.Error(), "contradicts") {
		t.Errorf("drop and keep together: %v", err)
	}
	if _, err := resolve(t, nil, Options{Fresh: true, DropReplace: []string{cf}}); err == nil || !strings.Contains(err.Error(), "no installed binary") {
		t.Errorf("drop with --fresh: %v", err)
	}
	// Caddy itself can be dropped back to upstream the same way.
	forked := installed()
	forked.MainPath, forked.MainReplace, forked.MainReplaceVer = caddybin.CaddyModulePath, "github.com/fork/caddy/v2", "v2.11.6-fork"
	p, err = resolve(t, forked, Options{DropReplace: []string{caddybin.CaddyModulePath}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Replacements) != 0 || !strings.Contains(p.CaddyNote, "github.com/fork/caddy/v2@v2.11.6-fork dropped") {
		t.Errorf("dropped Caddy replacement: %+v %q", p.Replacements, p.CaddyNote)
	}
	var text strings.Builder
	p.WriteText(&text)
	if !strings.Contains(text.String(), p.CaddyNote) {
		t.Errorf("the plan text must show the Caddy note:\n%s", text.String())
	}
	// A --with that moves a replaced plugin to another major's module path
	// names a new source outright; the old path's replacement cannot apply
	// to it, and the plan says so instead of demanding --drop-replace.
	moved := installed()
	moved.Plugins[0] = caddybin.Plugin{ModuleID: "x", Package: "github.com/example/plugin", Version: "v1.3.0", Replace: "/srv/plugin"}
	p, err = resolve(t, moved, Options{With: []string{"github.com/example/plugin/v2@v2.0.1"}, AllowMajor: true})
	if err != nil {
		t.Fatal(err)
	}
	if pl := plugin(p, "github.com/example/plugin/v2"); pl == nil || !strings.Contains(pl.Note, "does not carry over") || len(p.Replacements) != 0 {
		t.Errorf("major move of a replaced plugin: %+v %+v", pl, p.Replacements)
	}
}
