package caddybin

import (
	"os"
	"os/exec"
	"os/user"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestLookupAccount(t *testing.T) {
	// root exists everywhere, by name and by number.
	for _, name := range []string{"root", "0"} {
		a, err := LookupAccount(name, "", nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if a.UID != 0 || a.Name != "root" || a.Home == "" {
			t.Errorf("root by %q: %+v", name, a)
		}
	}
	// Group= replaces the primary group; a group given twice is listed once.
	a, err := LookupAccount("root", "0", []string{"0"})
	if err != nil {
		t.Fatal(err)
	}
	if a.GID != 0 || len(a.Groups) == 0 {
		t.Errorf("Group= and SupplementaryGroups=: %+v", a)
	}
	seen := map[uint32]int{}
	for _, g := range a.Groups {
		seen[g]++
	}
	if seen[0] != 1 {
		t.Errorf("group 0 should be listed exactly once: %v", a.Groups)
	}
	// Unknown names are errors, never "some other account".
	if _, err := LookupAccount("no-such-user-upgrade-caddy", "", nil); err == nil || !strings.Contains(err.Error(), "looking up user") {
		t.Errorf("unknown user: %v", err)
	}
	if _, err := LookupAccount("root", "no-such-group-upgrade-caddy", nil); err == nil || !strings.Contains(err.Error(), "looking up group") {
		t.Errorf("unknown group: %v", err)
	}
	if _, err := LookupAccount("root", "", []string{"no-such-group-upgrade-caddy"}); err == nil {
		t.Error("unknown supplementary group must be an error")
	}
	// Group= replaces the primary group in the supplementary list too, as
	// systemd seeds getgrouplist with the overridden group: root's own
	// group 0 is gone unless the group file lists root as a member, which
	// Debian-family systems do not.
	if _, err := user.LookupGroupId("65534"); err == nil {
		a, err := LookupAccount("root", "65534", nil)
		if err != nil {
			t.Fatal(err)
		}
		if a.GID != 65534 || !slices.Contains(a.Groups, uint32(65534)) || slices.Contains(a.Groups, 0) {
			t.Errorf("Group= override must replace the primary group in the list: %+v", a)
		}
	}
	if gid, err := LookupGroup("0"); err != nil || gid != 0 {
		t.Errorf("LookupGroup by ID: %d %v", gid, err)
	}
	if _, err := LookupGroup("no-such-group-upgrade-caddy"); err == nil {
		t.Error("LookupGroup of an unknown group must be an error")
	}
}

func TestApply(t *testing.T) {
	var none *Account
	cmd := exec.Command("true")
	if err := none.Apply(cmd); err != nil || cmd.SysProcAttr != nil || cmd.Env != nil {
		t.Errorf("nil account must leave the command alone: %v %+v", err, cmd)
	}
	self, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	if err := self.Apply(cmd); err != nil || cmd.SysProcAttr != nil {
		t.Errorf("the current account must leave the command alone: %v %+v", err, cmd)
	}
	// A different group list is a different identity, and so is the same
	// user and group with no supplementary groups when the process has
	// some: an empty list now means none, not "inherit".
	withGroup := *self
	withGroup.Groups = append([]uint32{1 << 20}, withGroup.Groups...)
	if err := withGroup.Apply(cmd); os.Geteuid() != 0 && (err == nil || !strings.Contains(err.Error(), "requires root")) {
		t.Errorf("a different group list is a different identity: %v", err)
	}
	bare := &Account{Name: "bare", UID: self.UID, GID: self.GID}
	if len(self.Groups) > 1 || (len(self.Groups) == 1 && self.Groups[0] != self.GID) {
		if err := bare.Apply(cmd); os.Geteuid() != 0 && (err == nil || !strings.Contains(err.Error(), "requires root")) {
			t.Errorf("dropping the supplementary groups is a switch: %v", err)
		}
	}
	other := &Account{Name: "other", UID: self.UID + 1, GID: self.GID, Home: "/srv/other"}
	err = other.Apply(cmd)
	if os.Geteuid() != 0 {
		if err == nil || !strings.Contains(err.Error(), "requires root") {
			t.Errorf("switching accounts without root must be refused, got %v", err)
		}
		return
	}
	if err != nil || cmd.SysProcAttr == nil || cmd.SysProcAttr.Credential.Uid != other.UID {
		t.Errorf("as root the child gets the account's credentials: %v %+v", err, cmd.SysProcAttr)
	}
	if !slices.Contains(cmd.Env, "HOME=/srv/other") {
		t.Errorf("an inherited environment gets the account's HOME: %v", cmd.Env)
	}
	// An environment the caller composed is left alone.
	composed := exec.Command("true")
	composed.Env = []string{"HOME=/composed", "X=1"}
	if err := other.Apply(composed); err != nil || !reflect.DeepEqual(composed.Env, []string{"HOME=/composed", "X=1"}) {
		t.Errorf("a composed environment must not be rewritten: %v %v", err, composed.Env)
	}
}

func TestSameIdentity(t *testing.T) {
	a := &Account{UID: 999, GID: 999, Groups: []uint32{999, 33}}
	for name, b := range map[string]*Account{
		"same":                       {UID: 999, GID: 999, Groups: []uint32{999, 33}},
		"primary not repeated":       {UID: 999, GID: 999, Groups: []uint32{33}},
		"different order":            {UID: 999, GID: 999, Groups: []uint32{33, 999}},
		"primary duplicated in list": {UID: 999, GID: 999, Groups: []uint32{33, 999, 999}},
	} {
		if !a.SameIdentity(b) {
			t.Errorf("%s: want same identity, %+v vs %+v", name, a, b)
		}
	}
	for name, b := range map[string]*Account{
		"other user":    {UID: 1000, GID: 999, Groups: []uint32{999, 33}},
		"other group":   {UID: 999, GID: 33, Groups: []uint32{999, 33}},
		"missing group": {UID: 999, GID: 999, Groups: []uint32{999}},
		"extra group":   {UID: 999, GID: 999, Groups: []uint32{999, 33, 4}},
		"nil":           nil,
	} {
		if a.SameIdentity(b) {
			t.Errorf("%s: want different identity, %+v vs %+v", name, a, b)
		}
	}
	if (*Account)(nil).SameIdentity(a) {
		t.Error("nil is nobody")
	}
}

func TestAccountEnviron(t *testing.T) {
	a := &Account{Name: "caddy", Home: "/var/lib/caddy"}
	got := a.environ([]string{"HOME=/root", "USER=root", "PATH=/usr/bin", "LOGNAME=root", "GOPROXY=direct"})
	want := []string{"PATH=/usr/bin", "GOPROXY=direct", "USER=caddy", "LOGNAME=caddy", "HOME=/var/lib/caddy"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("environ: got %v, want %v", got, want)
	}
	// No home directory known: HOME is dropped rather than left pointing at
	// the privileged user's.
	noHome := &Account{Name: "nobody"}
	for _, kv := range noHome.environ([]string{"HOME=/root"}) {
		if strings.HasPrefix(kv, "HOME=") {
			t.Errorf("HOME must not survive for an account without one: %v", kv)
		}
	}
	if (&Account{Name: "caddy", UID: 999}).String() != "caddy (uid 999)" || (*Account)(nil).String() != "" {
		t.Error("String")
	}
}
