package install

import (
	"strings"
	"testing"

	"github.com/seanthegeek/upgrade-caddy/internal/pkgmgr"
)

func TestRemoveCommand(t *testing.T) {
	cases := map[string]string{"dpkg": "sudo apt remove caddy", "rpm": "sudo dnf remove caddy", "pacman": "sudo pacman -R caddy", "apk": "sudo apk del caddy"}
	for mgr, want := range cases {
		if got := removeCommand(&pkgmgr.Owner{Manager: mgr, Package: "caddy"}); got != want {
			t.Errorf("%s: %q", mgr, got)
		}
	}
	err := refuseSystemPackage("/usr/bin/caddy", &pkgmgr.Owner{Manager: "dpkg", Package: "caddy"})
	if !strings.Contains(err.Error(), "sudo apt remove caddy") || !strings.Contains(err.Error(), "manual-installation") {
		t.Errorf("dpkg message: %v", err)
	}
}
