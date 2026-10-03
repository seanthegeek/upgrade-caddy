package caddybin

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// Account is the user a Caddy binary is executed as when it is run for
// `version`, `list-modules` or `validate`. install uses it so that a binary
// it has not installed yet never runs as root: a plugin's initialisation
// code runs the moment the binary starts, before the service's own user and
// sandbox would apply, and root is never needed to print a version or read
// a config. Only root can switch accounts, so Apply refuses to pretend
// otherwise.
type Account struct {
	Name   string   `json:"name"`
	UID    uint32   `json:"uid"`
	GID    uint32   `json:"gid"`
	Groups []uint32 `json:"groups,omitempty"` // supplementary groups
	Home   string   `json:"home,omitempty"`
}

// LookupAccount resolves a user, given by name or numeric ID, through
// os/user: its primary group, its supplementary groups and its home
// directory. group, when not empty, is a group name or numeric ID that
// takes the place of the primary group, as a unit's Group= does, and extra
// are added to the supplementary groups, as SupplementaryGroups= does.
func LookupAccount(name, group string, extra []string) (*Account, error) {
	var (
		u   *user.User
		err error
	)
	if isNumeric(name) {
		u, err = user.LookupId(name)
	} else {
		u, err = user.Lookup(name)
	}
	if err != nil {
		return nil, fmt.Errorf("looking up user %q: %w", name, err)
	}
	a := &Account{Name: u.Username, Home: u.HomeDir}
	if a.UID, err = parseID(u.Uid); err != nil {
		return nil, fmt.Errorf("user %q: %w", name, err)
	}
	if a.GID, err = parseID(u.Gid); err != nil {
		return nil, fmt.Errorf("user %q: %w", name, err)
	}
	if group != "" {
		if a.GID, err = lookupGroup(group); err != nil {
			return nil, err
		}
	}
	// os/user's pure-Go fallback (CGO_ENABLED=0 release builds) reads
	// /etc/group directly, which is where a system user's groups are.
	// Either way the list starts with the user's primary group from the
	// user database; systemd instead seeds getgrouplist with the group the
	// service actually gets (initgroups(user, gid) in
	// src/core/exec-invoke.c), so when Group= overrides it the original
	// primary group is dropped and the override takes its place.
	ids, err := u.GroupIds()
	if err != nil {
		return nil, fmt.Errorf("listing groups of user %q: %w", name, err)
	}
	if group != "" {
		ids = slices.DeleteFunc(ids, func(id string) bool { return id == u.Gid })
		ids = append(ids, strconv.FormatUint(uint64(a.GID), 10))
	}
	for _, g := range extra {
		gid, err := lookupGroup(g)
		if err != nil {
			return nil, err
		}
		ids = append(ids, strconv.FormatUint(uint64(gid), 10))
	}
	seen := map[uint32]bool{}
	for _, id := range ids {
		gid, err := parseID(id)
		if err != nil {
			return nil, fmt.Errorf("user %q: %w", name, err)
		}
		if !seen[gid] {
			seen[gid] = true
			a.Groups = append(a.Groups, gid)
		}
	}
	return a, nil
}

// Apply makes cmd run as the account. When the account is the current
// identity, supplementary groups included, nothing changes. Otherwise the
// process must be root, and the child then gets exactly the account's user,
// group and supplementary groups: an empty list means none, never the
// parent's, so a binary run as the sudo invoker or as a root service with
// Group= alone does not keep root's extra groups. When cmd.Env is unset the
// inherited environment is used with HOME, USER and LOGNAME naming the
// account, the way systemd sets them for a User= service; an environment
// the caller composed is left as it is. A nil account means the current
// user.
func (a *Account) Apply(cmd *exec.Cmd) error {
	if a == nil {
		return nil
	}
	cur, err := Current()
	if err != nil {
		return err
	}
	if a.SameIdentity(cur) {
		return nil
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("running %s as user %s requires root", cmd.Path, a.Name)
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: a.UID, Gid: a.GID, Groups: a.Groups}
	// A caller that composed cmd.Env already decided what the account's
	// variables are (install builds a unit's environment the way systemd
	// does, where Environment= may override them); only an inherited
	// environment is rewritten here.
	if cmd.Env == nil {
		cmd.Env = a.environ(os.Environ())
	}
	return nil
}

// environ rewrites an inherited environment for the account: HOME, USER and
// LOGNAME name it (HOME only when the account has a home directory) and
// everything else is kept.
func (a *Account) environ(env []string) []string {
	kept := make([]string, 0, len(env)+3)
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if k != "HOME" && k != "USER" && k != "LOGNAME" {
			kept = append(kept, kv)
		}
	}
	kept = append(kept, "USER="+a.Name, "LOGNAME="+a.Name)
	if a.Home != "" {
		kept = append(kept, "HOME="+a.Home)
	}
	return kept
}

// Current is the identity of this process: effective user and group and
// the supplementary groups. IDs are converted as uint32, since an ID never
// exceeds 32 bits while int is 32 bits on some targets.
func Current() (*Account, error) {
	groups, err := os.Getgroups()
	if err != nil {
		return nil, fmt.Errorf("reading the process's groups: %w", err)
	}
	a := &Account{UID: uint32(os.Geteuid()), GID: uint32(os.Getegid())}
	for _, g := range groups {
		a.Groups = append(a.Groups, uint32(g))
	}
	return a, nil
}

// SameIdentity reports whether two accounts would give a process the same
// permissions: the same user, the same group and the same set of groups
// overall. The primary group is counted among the groups on both sides,
// because whether it also appears in the supplementary list varies between
// how a login was set up and how a service is started, without changing
// what the process may read.
func (a *Account) SameIdentity(b *Account) bool {
	if a == nil || b == nil || a.UID != b.UID || a.GID != b.GID {
		return false
	}
	return groupSet(a) == groupSet(b)
}

// groupSet renders an account's groups, primary included, as a canonical
// string for comparison.
func groupSet(a *Account) string {
	set := map[uint32]bool{a.GID: true}
	for _, g := range a.Groups {
		set[g] = true
	}
	ids := make([]string, 0, len(set))
	for g := range set {
		ids = append(ids, strconv.FormatUint(uint64(g), 10))
	}
	slices.Sort(ids)
	return strings.Join(ids, ",")
}

// String names the account for a plan: "caddy (uid 999)".
func (a *Account) String() string {
	if a == nil {
		return ""
	}
	return fmt.Sprintf("%s (uid %d)", a.Name, a.UID)
}

// LookupGroup resolves a group name or numeric ID to its ID.
func LookupGroup(name string) (uint32, error) { return lookupGroup(name) }

func lookupGroup(name string) (uint32, error) {
	var (
		g   *user.Group
		err error
	)
	if isNumeric(name) {
		g, err = user.LookupGroupId(name)
	} else {
		g, err = user.LookupGroup(name)
	}
	if err != nil {
		return 0, fmt.Errorf("looking up group %q: %w", name, err)
	}
	gid, err := parseID(g.Gid)
	if err != nil {
		return 0, fmt.Errorf("group %q: %w", name, err)
	}
	return gid, nil
}

func isNumeric(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

func parseID(s string) (uint32, error) {
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, errors.New("ID " + strconv.Quote(s) + " is not a number")
	}
	return uint32(n), nil
}
