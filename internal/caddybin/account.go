package caddybin

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
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
	ids, err := u.GroupIds()
	if err != nil {
		return nil, fmt.Errorf("listing groups of user %q: %w", name, err)
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

// Apply makes cmd run as the account. When the account is the current user
// nothing changes. Otherwise the process must be root, and the child then
// gets the account's user, group and supplementary groups, with HOME, USER
// and LOGNAME naming the account the way systemd sets them for a User=
// service; the rest of the environment is inherited. A nil account means
// the current user.
func (a *Account) Apply(cmd *exec.Cmd) error {
	// Compared as uint32: an ID never exceeds 32 bits, while int is 32 bits
	// on some targets, so widening the process's IDs is the safe direction.
	if a == nil || (a.UID == uint32(os.Geteuid()) && a.GID == uint32(os.Getegid())) {
		return nil
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("running %s as user %s requires root", cmd.Path, a.Name)
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: a.UID, Gid: a.GID, Groups: a.Groups}
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	cmd.Env = a.environ(env)
	return nil
}

// environ rewrites an environment for the account: HOME, USER and LOGNAME
// name it (HOME only when the account has a home directory) and everything
// else is kept.
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

// String names the account for a plan: "caddy (uid 999)".
func (a *Account) String() string {
	if a == nil {
		return ""
	}
	return fmt.Sprintf("%s (uid %d)", a.Name, a.UID)
}

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
