package caddybin

import (
	"context"
	"strings"
	"testing"
)

const sample = `admin.api.load
admin.api.metrics

  Standard modules: 2

dns.providers.cloudflare v0.0.0-20240814120000-0123456789ab github.com/caddy-dns/cloudflare
http.handlers.replace_response v0.0.0-20240101000000-abcdefabcdef github.com/caddyserver/replace-response => ../replace-response
http.handlers.broken v1.2.3 github.com/example/broken [some error text]

  Non-standard modules: 3

mystery.module

  Unknown modules: 1
`

func TestParseListModules(t *testing.T) {
	std, nonstd, unknown := ParseListModules(sample)
	if std != 2 {
		t.Errorf("standard=%d want 2", std)
	}
	if len(nonstd) != 3 {
		t.Fatalf("nonstandard=%d want 3: %+v", len(nonstd), nonstd)
	}
	cf := nonstd[0]
	if cf.ModuleID != "dns.providers.cloudflare" || cf.Package != "github.com/caddy-dns/cloudflare" || cf.Version != "v0.0.0-20240814120000-0123456789ab" {
		t.Errorf("bad cloudflare entry: %+v", cf)
	}
	if nonstd[1].Replace != "../replace-response" {
		t.Errorf("replace not parsed: %+v", nonstd[1])
	}
	if nonstd[2].Err != "some error text" || nonstd[2].Version != "v1.2.3" {
		t.Errorf("error line not parsed: %+v", nonstd[2])
	}
	if len(unknown) != 1 || unknown[0].ModuleID != "mystery.module" {
		t.Errorf("unknown not parsed: %+v", unknown)
	}
}

func TestParseListModulesDistro(t *testing.T) {
	out := "tls.stek.standard\n\n  Standard modules: 98\n\n  Non-standard modules: 0\n\n  Unknown modules: 0\n"
	std, nonstd, unknown := ParseListModules(out)
	if std != 98 || len(nonstd) != 0 || len(unknown) != 0 {
		t.Errorf("got std=%d nonstd=%v unknown=%v", std, nonstd, unknown)
	}
}

func TestParseListModulesBareIDs(t *testing.T) {
	_, _, unknown := ParseListModules("a.b\nc.d\n")
	if len(unknown) != 2 {
		t.Errorf("bare IDs should become unknown: %+v", unknown)
	}
}

// TestInspectHostCaddy runs against whatever caddy is on PATH. It is skipped
// under -short and when no caddy is installed, so the hermetic tests above
// stay the ones CI relies on.
func TestInspectHostCaddy(t *testing.T) {
	if testing.Short() {
		t.Skip("-short")
	}
	path, err := Find("")
	if err != nil {
		t.Skip("no caddy on PATH")
	}
	info, err := Inspect(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Version == "" || info.GoVersion == "" || info.StandardCount == 0 {
		t.Errorf("incomplete info: %+v", info)
	}
	if info.HasModuleInfo && !strings.HasPrefix(info.MainVersion, "v") {
		t.Errorf("module info present but main version is %q", info.MainVersion)
	}
	if !info.HasModuleInfo && len(info.Plugins) != 0 {
		t.Errorf("no module info but plugins listed: %+v", info.Plugins)
	}
}

func TestIsCaddyModule(t *testing.T) {
	cases := map[string]bool{
		"github.com/caddyserver/caddy/v2":   true,
		"github.com/caddyserver/caddy/v3":   true,
		"github.com/caddyserver/caddy":      false, // v1, unsupported
		"github.com/caddyserver/xcaddy":     false,
		"github.com/caddy-dns/cloudflare":   false,
		"github.com/caddyserver/caddy/v2/x": false,
	}
	for in, want := range cases {
		if got := IsCaddyModule(in); got != want {
			t.Errorf("IsCaddyModule(%q)=%v want %v", in, got, want)
		}
	}
}
