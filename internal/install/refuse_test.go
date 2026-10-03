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
	// Facts verified against Ubuntu's caddy maintainer scripts: the unit is
	// masked, the user is kept, and only purge deletes /etc/caddy.
	for _, want := range []string{"caddy.service masked", "keeps the caddy user", "purge would delete /etc/caddy", "systemctl unmask"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("dpkg message should say %q: %v", want, err)
		}
	}
	generic := refuseSystemPackage("/usr/local/bin/caddy", &pkgmgr.Owner{Manager: "rpm", Package: "caddy"})
	if strings.Contains(generic.Error(), "on Debian and Ubuntu") || !strings.Contains(generic.Error(), "left behind") {
		t.Errorf("non-dpkg message must not state Debian specifics: %v", generic)
	}
}
