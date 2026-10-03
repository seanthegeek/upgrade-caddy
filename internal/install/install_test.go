package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/seanthegeek/upgrade-caddy/internal/build"
	"github.com/seanthegeek/upgrade-caddy/internal/caddybin"
	"github.com/seanthegeek/upgrade-caddy/internal/systemd"
)

func TestNeedsRoot(t *testing.T) {
	cases := []struct {
		name     string
		euid     int
		writable bool
		restart  int
		caps     bool
		want     bool
		reasons  int
	}{
		{"user, writable, no service", 1000, true, 0, false, false, 0},
		{"user, not writable", 1000, false, 0, false, true, 1},
		{"user, restart needed", 1000, true, 1, false, true, 1},
		{"user, caps", 1000, true, 0, true, true, 1},
		{"user, everything", 1000, false, 2, true, true, 3},
		{"root, everything", 0, false, 2, true, false, 3},
	}
	for _, c := range cases {
		got, reasons := needsRoot(c.euid, c.writable, "/usr/bin", c.restart, "caddy.service", c.caps, nil)
		if got != c.want || len(reasons) != c.reasons {
			t.Errorf("%s: got %v %v", c.name, got, reasons)
		}
	}
	// Having to validate as another account is a reason of its own.
	if got, reasons := needsRoot(1000, true, "/usr/bin", 0, "", false, []string{"validate /etc/caddy/Caddyfile as caddy, the account caddy.service runs as"}); !got || len(reasons) != 1 {
		t.Errorf("switching accounts for validation needs root: %v %v", got, reasons)
	}
}

func TestParseCaps(t *testing.T) {
	cases := map[string]string{
		"/usr/bin/caddy cap_net_bind_service=ep\n":   "cap_net_bind_service=ep",
		"/usr/bin/caddy = cap_net_bind_service+ep\n": "cap_net_bind_service+ep",
		"":                 "",
		"/usr/bin/caddy\n": "",
	}
	for in, want := range cases {
		if got := parseCaps(in); got != want {
			t.Errorf("parseCaps(%q)=%q want %q", in, got, want)
		}
	}
}

func TestReadCapsDistinguishesNoneFromUnknown(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain")
	os.WriteFile(plain, []byte("x"), 0o755)
	caps, err := readCaps(plain)
	if err != nil || caps != nil {
		t.Errorf("a plain file has no capabilities: %v %v", caps, err)
	}
	if _, err := readCaps(filepath.Join(dir, "missing")); err == nil {
		t.Error("an unreadable target is an error, not 'no capabilities'")
	}
	// Writing capabilities needs CAP_SETFCAP; unprivileged, swap must
	// report the failure and roll back rather than drop them silently.
	if os.Geteuid() != 0 {
		target := filepath.Join(dir, "caddy")
		os.WriteFile(target, []byte("old"), 0o755)
		staged := filepath.Join(dir, "staged")
		os.WriteFile(staged, []byte("new"), 0o755)
		_, err := swap(target, staged, []byte{1, 0, 0, 2, 0, 4, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
		if err == nil || !strings.Contains(err.Error(), "file capabilities") {
			t.Errorf("unprivileged capability write must fail loudly: %v", err)
		}
		if got, _ := os.ReadFile(target); string(got) != "old" {
			t.Errorf("target must be rolled back after a capability failure, got %q", got)
		}
	}
}

func TestRestartUnitsFirstInstallCleansUp(t *testing.T) {
	settleDelay = 10 * time.Millisecond
	t.Cleanup(func() { settleDelay = time.Second })
	dir := t.TempDir()
	target := filepath.Join(dir, "caddy")
	os.WriteFile(target, []byte("new"), 0o755)
	os.WriteFile(target+".lock.json", []byte("newlock"), 0o644)
	var log strings.Builder
	ctl := &fakeCtl{}
	_, err := restartUnits(context.Background(), ctl, []string{"a.service"}, target, "", 50*time.Millisecond, &log)
	if err == nil || !strings.Contains(err.Error(), "removed from") {
		t.Errorf("first install restart failure: %v", err)
	}
	for _, f := range []string{target, target + ".lock.json"} {
		if _, statErr := os.Stat(f); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("%s must be removed again after a failed first-install restart", f)
		}
	}
	// The unit that failed verification was restarted too and may still
	// be running (a wrong main process, say), so it is stopped as well
	// before the binary is removed from under it.
	if len(ctl.stops) != 1 || ctl.stops[0] != "a.service" {
		t.Errorf("the failing unit must be stopped before the binary is removed: %v", ctl.stops)
	}
}

func TestRestartUnitsFirstInstallStopsAlreadyRestartedUnits(t *testing.T) {
	settleDelay = 10 * time.Millisecond
	t.Cleanup(func() { settleDelay = time.Second })
	dir := t.TempDir()
	target := filepath.Join(dir, "caddy")
	os.WriteFile(target, []byte("new"), 0o755)
	// a.service comes up (active, still active after settle); b.service
	// never does. Nothing was installed before, so a.service must be
	// stopped again before the binary is removed from under it.
	ctl := &fakeCtl{active: []bool{true, true}}
	var log strings.Builder
	restarted, err := restartUnits(context.Background(), ctl, []string{"a.service", "b.service"}, target, "", 50*time.Millisecond, &log)
	if err == nil || len(restarted) != 1 || restarted[0] != "a.service" {
		t.Fatalf("expected a.service restarted then failure on b.service: restarted=%v err=%v", restarted, err)
	}
	if want := []string{"a.service", "b.service"}; !reflect.DeepEqual(ctl.stops, want) {
		t.Errorf("the already-restarted unit and the failing one must both be stopped again: %v", ctl.stops)
	}
	if !strings.Contains(err.Error(), "the 2 restarted unit(s) stopped") {
		t.Errorf("the error should say what was undone: %v", err)
	}
	if _, statErr := os.Stat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("the new binary must be removed again")
	}
}

func TestRestartUnitsFirstInstallKeepsFilesWhenStopFails(t *testing.T) {
	settleDelay = 10 * time.Millisecond
	t.Cleanup(func() { settleDelay = time.Second })
	dir := t.TempDir()
	target := filepath.Join(dir, "caddy")
	os.WriteFile(target, []byte("new"), 0o755)
	os.WriteFile(target+".lock.json", []byte("newlock"), 0o644)
	// The unit never comes up and cannot be stopped either: it may still
	// be running, so the binary must not be removed from under it.
	ctl := &fakeCtl{stopErr: errors.New("stop refused")}
	var log strings.Builder
	_, err := restartUnits(context.Background(), ctl, []string{"a.service"}, target, "", 50*time.Millisecond, &log)
	if err == nil || !strings.Contains(err.Error(), "left at") || !strings.Contains(err.Error(), "stop refused") {
		t.Errorf("a failed stop must be reported and the files kept: %v", err)
	}
	for _, f := range []string{target, target + ".lock.json"} {
		if _, statErr := os.Stat(f); statErr != nil {
			t.Errorf("%s must be left in place when a unit could not be stopped", f)
		}
	}
}

func TestIdentifyNoticesReplacement(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "caddy")
	os.WriteFile(target, []byte("installed"), 0o755)
	before, err := identify(target)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := identify(target)
	if again != before {
		t.Error("an untouched file must keep its identity")
	}
	// A package upgrade or a rebuild puts a new inode at the path.
	other := filepath.Join(dir, "other")
	os.WriteFile(other, []byte("installed"), 0o755)
	os.Rename(other, target)
	if after, _ := identify(target); after == before {
		t.Error("a replaced file must get a new identity")
	}
	// An in-place rewrite changes size (or mtime).
	before, _ = identify(target)
	os.WriteFile(target, []byte("rewritten in place"), 0o755)
	if after, _ := identify(target); after == before {
		t.Error("a rewritten file must get a new identity")
	}
	if _, err := identify(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing file is an error")
	}
	if !sameUnits([]systemd.Unit{{Name: "a"}, {Name: "b"}}, []systemd.Unit{{Name: "a"}, {Name: "b"}}) ||
		sameUnits([]systemd.Unit{{Name: "a"}}, []systemd.Unit{{Name: "a"}, {Name: "b"}}) ||
		sameUnits([]systemd.Unit{{Name: "a"}}, []systemd.Unit{{Name: "c"}}) {
		t.Error("sameUnits must compare the unit names in order")
	}
	// The same unit with a different config, adapter, envfile or working
	// directory is a changed unit: validation would otherwise be stale.
	base := systemd.Unit{Name: "a", WorkingDirectory: "/srv", Args: []string{"caddy", "run", "--config", "/etc/a", "--adapter", "caddyfile", "--envfile", "/e1"}}
	same := base
	same.Args = []string{"caddy", "run", "--environ", "--config", "/etc/a", "--adapter", "caddyfile", "--envfile", "/e1"} // an unrelated flag
	if !sameUnits([]systemd.Unit{base}, []systemd.Unit{same}) {
		t.Error("a flag that does not affect validation must not count as a change")
	}
	for name, args := range map[string][]string{
		"config":  {"caddy", "run", "--config", "/etc/b"},
		"adapter": {"caddy", "run", "--config", "/etc/a", "--adapter", "json", "--envfile", "/e1"},
		"envfile": {"caddy", "run", "--config", "/etc/a", "--adapter", "caddyfile", "--envfile", "/e2"},
	} {
		changed := base
		changed.Args = args
		if sameUnits([]systemd.Unit{base}, []systemd.Unit{changed}) {
			t.Errorf("a changed %s must count as a changed unit", name)
		}
	}
	moved := base
	moved.WorkingDirectory = "/srv2"
	if sameUnits([]systemd.Unit{base}, []systemd.Unit{moved}) {
		t.Error("a changed working directory must count as a changed unit")
	}
	// The account the unit runs as decides who validate runs as.
	for name, change := range map[string]func(*systemd.Unit){
		"user":    func(u *systemd.Unit) { u.User = "caddy" },
		"group":   func(u *systemd.Unit) { u.Group = "www-data" },
		"groups":  func(u *systemd.Unit) { u.SupplementaryGroups = []string{"adm"} },
		"dynamic": func(u *systemd.Unit) { u.DynamicUser = true },
	} {
		changed := base
		change(&changed)
		if sameUnits([]systemd.Unit{base}, []systemd.Unit{changed}) {
			t.Errorf("a changed %s must count as a changed unit", name)
		}
	}
}

func TestCheckWorkDirsRefusesUnresolvedHome(t *testing.T) {
	units := []systemd.Unit{
		{Name: "ok.service", Args: []string{"caddy", "run", "--config", "/etc/a"}, WorkingDirectory: "/"},
		{Name: "home.service", Args: []string{"caddy", "run", "--config", "Caddyfile"}, WorkingDirectory: systemd.UnresolvedHome},
	}
	vals := validationsFromUnits(units)
	err := checkWorkDirs(vals)
	if err == nil || !strings.Contains(err.Error(), "home.service") || !strings.Contains(err.Error(), "--config") {
		t.Errorf("an unresolved home directory must refuse validation and name the unit: %v", err)
	}
	if err := checkWorkDirs(vals[:1]); err != nil {
		t.Errorf("a resolved working directory is fine: %v", err)
	}
}

func TestLockfileSwapRestoresOnFailedInstall(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "caddy")
	lock := target + ".lock.json"
	os.WriteFile(lock, []byte("old"), 0o644)
	// The staged lockfile is missing, so the final rename fails after the
	// old one was moved aside: it must come back, and the error must not
	// claim it is stranded.
	err := swapLockfile(target, filepath.Join(dir, "missing.lock.json"))
	if err == nil || strings.Contains(err.Error(), "could not be put back") {
		t.Errorf("a successful restore must not be reported as stranded: %v", err)
	}
	if got, _ := os.ReadFile(lock); string(got) != "old" {
		t.Errorf("the previous lockfile must be back in place, got %q", got)
	}
	if _, err := os.Stat(lock + ".previous"); !errors.Is(err, os.ErrNotExist) {
		t.Error("nothing should be left at .previous after the restore")
	}
}

func inode(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Ino
}

func TestSwapAndRollback(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "caddy")
	newPath := filepath.Join(dir, ".caddy-install-1")
	if err := os.WriteFile(target, []byte("old"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldIno := inode(t, target)

	previous, err := swap(target, newPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if previous != target+".previous" {
		t.Errorf("previous path: %s", previous)
	}
	if got, _ := os.ReadFile(target); string(got) != "new" {
		t.Errorf("target content: %q", got)
	}
	if inode(t, previous) != oldIno {
		t.Error(".previous must be a hard link to the old binary's inode")
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o750 {
		t.Errorf("mode not preserved: %v", fi.Mode().Perm())
	}
	if _, err := os.Stat(newPath); !errors.Is(err, os.ErrNotExist) {
		t.Error("staged file should be gone after rename")
	}

	if restoreErr, keepErr := rollback(target, previous); restoreErr != nil || keepErr != nil {
		t.Fatal(restoreErr, keepErr)
	}
	if got, _ := os.ReadFile(target); string(got) != "old" {
		t.Errorf("after rollback target content: %q", got)
	}
	if inode(t, target) != oldIno {
		t.Error("rollback should restore the original inode")
	}
	if got, _ := os.ReadFile(target + ".failed"); string(got) != "new" {
		t.Error("failed binary should be kept as .failed")
	}
	if _, err := os.Stat(previous); !errors.Is(err, os.ErrNotExist) {
		t.Error(".previous should be consumed by rollback")
	}
}

func TestRollbackRestoresEvenWhenFailedCopyCannotBeKept(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "caddy")
	os.WriteFile(target, []byte("bad"), 0o755)
	os.WriteFile(target+".previous", []byte("good"), 0o755)
	// A non-empty directory at the .failed path makes the link fail.
	os.MkdirAll(filepath.Join(target+".failed", "x"), 0o755)
	restoreErr, keepErr := rollback(target, target+".previous")
	if got, _ := os.ReadFile(target); string(got) != "good" {
		t.Fatalf("target must be restored first, got %q", got)
	}
	if restoreErr != nil {
		t.Errorf("the restore succeeded and must not be reported as failed: %v", restoreErr)
	}
	if keepErr == nil || !strings.Contains(keepErr.Error(), "could not keep the failed binary") {
		t.Errorf("the diagnostic failure must still be reported: %v", keepErr)
	}
	// And the operator-facing summary says the rollback succeeded, without
	// claiming the failed binary was kept when it was not.
	if msg := rolledBack(restoreErr, keepErr); !strings.Contains(msg, "rolled back to the previous binary") || !strings.Contains(msg, "could not be kept") || strings.Contains(msg, "is kept") {
		t.Errorf("summary for a successful restore with a failed diagnostic copy: %q", msg)
	}
	if msg := rolledBack(nil, nil); !strings.Contains(msg, "is kept as") {
		t.Errorf("summary for a clean rollback: %q", msg)
	}
	if msg := rolledBack(errors.New("x"), nil); !strings.Contains(msg, "ROLLBACK FAILED") {
		t.Errorf("summary for a failed restore: %q", msg)
	}
}

func TestLockfileSwapAndRollback(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "caddy")
	lock := target + ".lock.json"
	os.WriteFile(lock, []byte("old"), 0o644)
	staged := filepath.Join(dir, "staged.lock.json")
	os.WriteFile(staged, []byte("new"), 0o644)
	if err := swapLockfile(target, staged); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(lock); string(got) != "new" {
		t.Errorf("lockfile after swap: %q", got)
	}
	if got, _ := os.ReadFile(lock + ".previous"); string(got) != "old" {
		t.Errorf("previous lockfile should be kept: %q", got)
	}
	if err := rollbackLockfile(target); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(lock); string(got) != "old" {
		t.Errorf("lockfile after rollback: %q", got)
	}
	// First install: no previous lockfile, rollback removes the new one.
	os.Remove(lock)
	os.WriteFile(staged, []byte("new"), 0o644)
	if err := swapLockfile(target, staged); err != nil {
		t.Fatal(err)
	}
	if err := rollbackLockfile(target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Error("rollback with no previous lockfile should remove the new one")
	}
}

func TestRestartUnitsRollsBackUnderCancelledContext(t *testing.T) {
	settleDelay = 10 * time.Millisecond
	t.Cleanup(func() { settleDelay = time.Second })
	dir := t.TempDir()
	target := filepath.Join(dir, "caddy")
	os.WriteFile(target, []byte("new"), 0o755)
	os.WriteFile(target+".previous", []byte("old"), 0o755)
	os.WriteFile(target+".lock.json", []byte("newlock"), 0o644)
	os.WriteFile(target+".lock.json.previous", []byte("oldlock"), 0o644)

	// The caller's context is already cancelled, as after Ctrl-C or the
	// overall deadline: the restart fails, but the rollback restarts must
	// still run under their own deadline.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// The forward restart never becomes active; the rollback restarts do.
	ctl := &fakeCtl{active: []bool{false}, defaultActive: true}
	var log strings.Builder
	restarted, err := restartUnits(ctx, ctl, []string{"a.service", "b.service"}, target, target+".previous", time.Second, &log)
	if err == nil || len(restarted) != 0 {
		t.Fatalf("expected failure, got restarted=%v err=%v", restarted, err)
	}
	if got, _ := os.ReadFile(target); string(got) != "old" {
		t.Errorf("binary should be rolled back, got %q", got)
	}
	if got, _ := os.ReadFile(target + ".lock.json"); string(got) != "oldlock" {
		t.Errorf("lockfile should be rolled back, got %q", got)
	}
	if len(ctl.restarts) != 3 { // the failed one, then both again for the rollback
		t.Errorf("rollback restarts must run despite the cancelled context: %v", ctl.restarts)
	}
	if !strings.Contains(err.Error(), "rolled back to the previous binary") || strings.Contains(err.Error(), "did not come back up") {
		t.Errorf("a successful, verified rollback should be reported as such: %v", err)
	}
}

func TestRestartUnitsReportsServiceDownAfterRollback(t *testing.T) {
	settleDelay = 10 * time.Millisecond
	t.Cleanup(func() { settleDelay = time.Second })
	dir := t.TempDir()
	target := filepath.Join(dir, "caddy")
	os.WriteFile(target, []byte("new"), 0o755)
	os.WriteFile(target+".previous", []byte("old"), 0o755)
	// Nothing ever becomes active, not even on the restored binary.
	ctl := &fakeCtl{}
	var log strings.Builder
	_, err := restartUnits(context.Background(), ctl, []string{"a.service"}, target, target+".previous", 50*time.Millisecond, &log)
	if err == nil || !strings.Contains(err.Error(), "did not come back up") || !strings.Contains(err.Error(), "after rollback") {
		t.Errorf("a rollback whose restart fails must say so: %v", err)
	}
	if len(ctl.restarts) != 2 {
		t.Errorf("forward restart plus one verified rollback restart expected: %v", ctl.restarts)
	}
	if got, _ := os.ReadFile(target); string(got) != "old" {
		t.Errorf("binary should still be rolled back on disk: %q", got)
	}
}

func TestRestartUnitsReportsFailedRollbackHonestly(t *testing.T) {
	settleDelay = 10 * time.Millisecond
	t.Cleanup(func() { settleDelay = time.Second })
	dir := t.TempDir()
	target := filepath.Join(dir, "caddy")
	os.WriteFile(target, []byte("new"), 0o755)
	os.WriteFile(target+".lock.json", []byte("newlock"), 0o644)
	os.WriteFile(target+".lock.json.previous", []byte("oldlock"), 0o644)
	// previous points at a file that does not exist, so the restore fails.
	ctl := &fakeCtl{}
	var log strings.Builder
	_, err := restartUnits(context.Background(), ctl, []string{"a.service", "b.service"}, target, filepath.Join(dir, "gone"), 50*time.Millisecond, &log)
	if err == nil || !strings.Contains(err.Error(), "ROLLBACK FAILED") || strings.Contains(err.Error(), "rolled back to") {
		t.Errorf("a failed restore must not be reported as rolled back: %v", err)
	}
	// The target still holds the failed binary, so nothing is restarted on
	// it again and its lockfile stays with it.
	if len(ctl.restarts) != 1 {
		t.Errorf("no recovery restarts after a failed restore, got %v", ctl.restarts)
	}
	if got, _ := os.ReadFile(target + ".lock.json"); string(got) != "newlock" || !strings.Contains(err.Error(), "lockfile was left beside it") {
		t.Errorf("the lockfile must follow the binary, got %q: %v", got, err)
	}
}

func TestLockfileSwapRefusesUnremovableStaleCopy(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "caddy")
	lock := target + ".lock.json"
	os.WriteFile(lock, []byte("old"), 0o644)
	staged := filepath.Join(dir, "staged.lock.json")
	os.WriteFile(staged, []byte("new"), 0o644)
	// A stale .previous that is a non-empty directory cannot be removed;
	// a later rollback would take it for the previous lockfile, so the
	// swap must stop before the live lockfile moves.
	os.MkdirAll(lock+".previous/x", 0o755)
	err := swapLockfile(target, staged)
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Errorf("an unremovable stale .previous must stop the swap: %v", err)
	}
	if got, _ := os.ReadFile(lock); string(got) != "old" {
		t.Errorf("the live lockfile must be untouched, got %q", got)
	}
	if got, _ := os.ReadFile(staged); string(got) != "new" {
		t.Errorf("the staged lockfile must be untouched, got %q", got)
	}
}

func TestValidationsFromUnits(t *testing.T) {
	units := []systemd.Unit{
		{Name: "a.service", Args: []string{"caddy", "run", "--config", "/etc/a/Caddyfile"}, WorkingDirectory: "/srv/a"},
		{Name: "b.service", Args: []string{"caddy", "run", "--config", "/etc/b.json", "--adapter", "json"}},
		{Name: "c.service", Args: []string{"caddy", "run", "--config", "/etc/a/Caddyfile"}, WorkingDirectory: "/srv/a"}, // same as a
		{Name: "d.service", Args: []string{"caddy", "run"}},                                                             // no --config
	}
	got := validationsFromUnits(units)
	if len(got) != 2 || got[0].From != "a.service" || got[1].Config != "/etc/b.json" || got[1].Adapter != "json" {
		t.Errorf("want two distinct validations, got %+v", got)
	}
	// The same config read by a different account is a different
	// validation: what the file looks like depends on who opens it.
	units[2].User = "caddy"
	if got := validationsFromUnits(units); len(got) != 3 || got[2].From != "c.service" {
		t.Errorf("one config under two users must be validated twice, got %+v", got)
	}
}

func TestSwapPreservesSetIDAndStickyBits(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "caddy")
	newPath := filepath.Join(dir, "staged")
	os.WriteFile(target, []byte("old"), 0o755)
	// setuid, setgid and sticky on a file the test owns.
	want := os.FileMode(0o755) | os.ModeSetuid | os.ModeSetgid | os.ModeSticky
	if err := os.Chmod(target, want); err != nil {
		t.Skipf("cannot set set-ID bits here: %v", err)
	}
	if fi, _ := os.Stat(target); fi.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0 {
		t.Skip("filesystem does not keep set-ID bits")
	}
	os.WriteFile(newPath, []byte("new"), 0o600)
	if _, err := swap(target, newPath, nil); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(target)
	if got := fi.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky); got != want {
		t.Errorf("mode after swap %v, want %v", got, want)
	}
}

func TestSwapFailsClosedOnStatError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can stat anything; no way to make the check fail")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	os.Mkdir(locked, 0o755)
	target := filepath.Join(locked, "caddy")
	os.WriteFile(target, []byte("old"), 0o755)
	os.WriteFile(target+".lock.json", []byte("oldlock"), 0o644)
	staged := filepath.Join(dir, "staged")
	os.WriteFile(staged, []byte("new"), 0o755)
	stagedLock := filepath.Join(dir, "staged.lock.json")
	os.WriteFile(stagedLock, []byte("newlock"), 0o644)
	// A directory that cannot be searched makes every stat inside it fail
	// with "permission denied", which is not "does not exist".
	if err := os.Chmod(locked, 0); err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	if _, err := swap(target, staged, nil); err == nil || !strings.Contains(err.Error(), "checking") {
		t.Errorf("a stat failure must stop the swap, not be read as a first install: %v", err)
	}
	if _, err := os.Stat(staged); err != nil {
		t.Error("the staged binary must be left where it was")
	}
	// In an unsearchable directory the stale-copy check fails first, with
	// the same outcome: the swap stops before anything moves.
	if err := swapLockfile(target, stagedLock); err == nil || !(strings.Contains(err.Error(), "checking") || strings.Contains(err.Error(), "removing stale")) {
		t.Errorf("a stat failure must stop the lockfile swap: %v", err)
	}
	if _, err := os.Stat(stagedLock); err != nil {
		t.Error("the staged lockfile must be left where it was")
	}
	if err := rollbackLockfile(target); err == nil || !strings.Contains(err.Error(), "checking") {
		t.Errorf("not knowing whether a previous lockfile exists must not remove the live one: %v", err)
	}
	os.Chmod(locked, 0o755)
	if got, _ := os.ReadFile(target); string(got) != "old" {
		t.Errorf("target must be untouched, got %q", got)
	}
	if got, _ := os.ReadFile(target + ".lock.json"); string(got) != "oldlock" {
		t.Errorf("lockfile must be untouched, got %q", got)
	}
}

func TestAttestStagedPair(t *testing.T) {
	dir := t.TempDir()
	info := &caddybin.Info{HasModuleInfo: true, MainPath: caddybin.CaddyModulePath, MainVersion: "v2.11.6", MainSum: "h1:caddy",
		Plugins: []caddybin.Plugin{{Package: "github.com/example/plugin", Version: "v1.3.0", Sum: "h1:p"}}}
	good := filepath.Join(dir, "good.lock.json")
	os.WriteFile(good, []byte(`{"schema":1,"caddy":{"package":"`+caddybin.CaddyModulePath+`","version":"v2.11.6","sum":"h1:caddy"},"plugins":[{"package":"github.com/example/plugin","version":"v1.3.0","sum":"h1:p"}]}`), 0o644)
	if err := attest(good, info); err != nil {
		t.Errorf("a lockfile that describes the binary: %v", err)
	}
	// The same lockfile for a different binary (a plugin swapped out)
	// is refused, and so is a lockfile that cannot be read at all.
	other := *info
	other.Plugins = []caddybin.Plugin{{Package: "github.com/example/evil", Version: "v1.0.0", Sum: "h1:e"}}
	if err := attest(good, &other); err == nil {
		t.Error("a lockfile for another plugin set must be refused")
	}
	if err := attest(filepath.Join(dir, "missing.lock.json"), info); err == nil {
		t.Error("a missing staged lockfile must be refused")
	}
}

func TestSwapFirstInstall(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "caddy")
	newPath := filepath.Join(dir, "staged")
	os.WriteFile(newPath, []byte("new"), 0o755)
	previous, err := swap(target, newPath, nil)
	if err != nil || previous != "" {
		t.Fatalf("first install: previous=%q err=%v", previous, err)
	}
	if restoreErr, _ := rollback(target, previous); restoreErr == nil {
		t.Error("rollback with no previous must fail")
	}
}

// fakeCtl records calls and answers IsActive from a script.
type fakeCtl struct {
	restarts      []string
	stops         []string
	active        []bool // scripted answers; once exhausted, defaultActive
	defaultActive bool
	pid           int
	restartErr    error
	stopErr       error
	activeErr     error
	pidErr        error
}

func (f *fakeCtl) Restart(_ context.Context, unit string) error {
	f.restarts = append(f.restarts, unit)
	return f.restartErr
}

func (f *fakeCtl) Stop(_ context.Context, unit string) error {
	f.stops = append(f.stops, unit)
	return f.stopErr
}

func (f *fakeCtl) IsActive(_ context.Context, _ string) (bool, error) {
	if f.activeErr != nil {
		return false, f.activeErr
	}
	if len(f.active) == 0 {
		return f.defaultActive, nil
	}
	a := f.active[0]
	f.active = f.active[1:]
	return a, nil
}

func (f *fakeCtl) MainPID(_ context.Context, _ string) (int, error) { return f.pid, f.pidErr }

func TestRestartAndVerify(t *testing.T) {
	settleDelay = 10 * time.Millisecond
	t.Cleanup(func() { settleDelay = time.Second })
	ctx := context.Background()
	ok := &fakeCtl{active: []bool{false, true, true}} // activating, active, still active
	if err := restartAndVerify(ctx, ok, "caddy.service", "/nonexistent/caddy", 5*time.Second); err != nil {
		t.Errorf("should succeed once active: %v", err)
	}
	if len(ok.restarts) != 1 {
		t.Errorf("restarts: %v", ok.restarts)
	}
	dead := &fakeCtl{}
	err := restartAndVerify(ctx, dead, "caddy.service", "/nonexistent/caddy", 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "not active") {
		t.Errorf("should time out when never active: %v", err)
	}
	// A Type=simple unit whose process dies right after forking: active once,
	// then gone before the settle check.
	flaky := &fakeCtl{active: []bool{true, false}}
	err = restartAndVerify(ctx, flaky, "caddy.service", "/nonexistent/caddy", time.Second)
	if err == nil || !strings.Contains(err.Error(), "did not stay active") {
		t.Errorf("should fail when the unit does not stay active: %v", err)
	}
	// With a readable /proc entry, the running executable is compared.
	self := &fakeCtl{active: []bool{true, true}, pid: os.Getpid()}
	exe, _ := os.Executable()
	if err := restartAndVerify(ctx, self, "x.service", exe, time.Second); err != nil {
		t.Errorf("own executable should match: %v", err)
	}
	if err := restartAndVerify(ctx, &fakeCtl{active: []bool{true, true}, pid: os.Getpid()}, "x.service", "/bin/sh", time.Second); err == nil || !strings.Contains(err.Error(), "runs") {
		t.Errorf("mismatched executable should fail: %v", err)
	}
}

func TestValidateArgs(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "caddy")
	argsFile := filepath.Join(dir, "args")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" > "+argsFile+"\npwd >> "+argsFile+"\nexit ${FAKE_EXIT:-0}\n"), 0o755)
	p := Validation{Config: "/etc/caddy/Caddyfile", Adapter: "caddyfile", EnvFiles: []string{"/a.env", "/b.env"}, WorkDir: dir}
	var log strings.Builder
	if err := validate(context.Background(), script, p, &log, false); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(argsFile)
	want := "validate --config /etc/caddy/Caddyfile --adapter caddyfile --envfile /a.env --envfile /b.env\n"
	if !strings.HasPrefix(string(got), want) {
		t.Errorf("args: %q", got)
	}
	if real, _ := filepath.EvalSymlinks(dir); !strings.Contains(string(got), real) {
		t.Errorf("should run in the unit's working directory: %q", got)
	}
	t.Setenv("FAKE_EXIT", "1")
	if err := validate(context.Background(), script, p, &log, false); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Errorf("non-zero exit should fail: %v", err)
	}
	// Asked to run as another account without being root, validate refuses
	// rather than running as whoever it is.
	if os.Geteuid() != 0 {
		p.account = &caddybin.Account{Name: "caddy", UID: uint32(os.Geteuid()) + 1}
		if err := validate(context.Background(), script, p, &log, false); err == nil || !strings.Contains(err.Error(), "requires root") {
			t.Errorf("switching accounts without root must fail: %v", err)
		}
	}
}

func TestStageAndCopy(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	from := filepath.Join(src, "built")
	os.WriteFile(from, []byte("bin"), 0o755)
	os.WriteFile(from+".lock.json", []byte("{}"), 0o644)
	bin, lock, err := stage(from, filepath.Join(dst, "caddy"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(bin) != dst || filepath.Dir(lock) != dst {
		t.Errorf("staged outside target dir: %s %s", bin, lock)
	}
	if !strings.HasPrefix(filepath.Base(bin), ".caddy-install-") || !strings.HasSuffix(lock, ".lock.json") {
		t.Errorf("staged names: %s %s", bin, lock)
	}
	// A path planted beforehand is never written through: the staged
	// names are unpredictable and created exclusively.
	planted := filepath.Join(dst, ".caddy-install-"+strings.TrimPrefix(filepath.Base(bin), ".caddy-install-"))
	if planted != bin {
		t.Fatal("test assumption broken")
	}
	bin2, _, err := stage(from, filepath.Join(dst, "caddy"))
	if err != nil || bin2 == bin {
		t.Errorf("a second staging must get a fresh exclusive file: %s %v", bin2, err)
	}
	if got, _ := os.ReadFile(bin); string(got) != "bin" {
		t.Errorf("staged content: %q", got)
	}
	if fi, _ := os.Stat(bin); fi.Mode().Perm() != 0o755 {
		t.Errorf("staged mode: %v", fi.Mode().Perm())
	}
	os.Remove(from + ".lock.json")
	if _, _, err := stage(from, filepath.Join(dst, "caddy2")); err == nil {
		t.Error("missing lockfile should fail staging")
	}
}

func TestFromRejectsBuildSelection(t *testing.T) {
	cases := map[string]Options{
		"upgrade-all":   {From: "/x/caddy", Build: build.Options{UpgradeAll: true}},
		"with":          {From: "/x/caddy", Build: build.Options{With: []string{"example.com/p"}}},
		"caddy-version": {From: "/x/caddy", Build: build.Options{CaddyVersion: "v2.11.0"}},
		"allow-major":   {From: "/x/caddy", Build: build.Options{AllowMajor: true}},
	}
	for name, opts := range cases {
		_, err := Resolve(context.Background(), opts)
		if err == nil || !strings.Contains(err.Error(), "--from installs an existing build") {
			t.Errorf("%s: want the --from rejection, got %v", name, err)
		}
	}
	if got := buildSelectionFlags(build.Options{Upgrade: []string{"x"}, Replace: []string{"a=b"}}); got != "--upgrade, --replace" {
		t.Errorf("buildSelectionFlags: %q", got)
	}
}

func TestRefuseSystemPackageMessage(t *testing.T) {
	err := refuseSystemPackage("/usr/bin/caddy", nil)
	if !strings.Contains(err.Error(), "--fresh --target /usr/bin/caddy") {
		t.Errorf("distro message: %v", err)
	}
}

func TestRestartAndVerifyErrors(t *testing.T) {
	settleDelay = 10 * time.Millisecond
	t.Cleanup(func() { settleDelay = time.Second })
	ctx := context.Background()
	boom := errors.New("boom")
	if err := restartAndVerify(ctx, &fakeCtl{restartErr: boom}, "x.service", "/x", time.Second); !errors.Is(err, boom) {
		t.Errorf("restart error should propagate: %v", err)
	}
	if err := restartAndVerify(ctx, &fakeCtl{activeErr: boom}, "x.service", "/x", time.Second); !errors.Is(err, boom) {
		t.Errorf("is-active error should propagate: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := restartAndVerify(cancelled, &fakeCtl{active: []bool{false}}, "x.service", "/x", time.Second); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled context while waiting: %v", err)
	}
	// A PID with no /proc entry means the process vanished after it was
	// seen active: that is a failed restart, not a successful one.
	if err := restartAndVerify(ctx, &fakeCtl{active: []bool{true, true}, pid: 2147483000}, "x.service", "/x", time.Second); err == nil || !strings.Contains(err.Error(), "could not be inspected") {
		t.Errorf("a vanished process must fail verification: %v", err)
	}
	// A /proc entry that exists but cannot be read (another user's
	// process, unprivileged): being active is the best we can confirm.
	if os.Geteuid() != 0 {
		if err := restartAndVerify(ctx, &fakeCtl{active: []bool{true, true}, pid: 1}, "x.service", "/x", time.Second); err != nil {
			t.Errorf("permission denied on /proc/1/exe should fall back to active: %v", err)
		}
	}
	// A failure to query MainPID is not "no main process": verification
	// could not be done, so the restart check fails.
	if err := restartAndVerify(ctx, &fakeCtl{active: []bool{true, true}, pidErr: boom}, "x.service", "/x", time.Second); !errors.Is(err, boom) {
		t.Errorf("MainPID error must fail verification: %v", err)
	}
	// PID 0 is a definite "no main process" and falls back to active state.
	if err := restartAndVerify(ctx, &fakeCtl{active: []bool{true, true}, pid: 0}, "x.service", "/x", time.Second); err != nil {
		t.Errorf("pid 0 should fall back to active: %v", err)
	}
}

func TestStageAndCopyErrors(t *testing.T) {
	dst := t.TempDir()
	if _, _, err := stage(filepath.Join(dst, "missing"), filepath.Join(dst, "caddy")); err == nil || !strings.Contains(err.Error(), "staging") {
		t.Errorf("missing source: %v", err)
	}
	src := filepath.Join(dst, "src")
	os.WriteFile(src, []byte("x"), 0o644)
	if _, err := copyToTemp(src, filepath.Join(dst, "no", "such", "dir"), "x-*", 0o644); err == nil {
		t.Error("unwritable destination should fail")
	}
	if _, err := copyToTemp(filepath.Join(dst, "nope"), dst, "x-*", 0o644); err == nil {
		t.Error("unreadable source should fail")
	}
}

func TestSwapErrors(t *testing.T) {
	dir := t.TempDir()
	// Target exists but the new file is missing: nothing to rename.
	target := filepath.Join(dir, "caddy")
	os.WriteFile(target, []byte("old"), 0o755)
	if _, err := swap(target, filepath.Join(dir, "missing"), nil); err == nil {
		t.Error("missing new binary should fail")
	}
	if _, err := os.Stat(target + ".previous"); !errors.Is(err, os.ErrNotExist) {
		t.Error("a failed rename must not leave .previous behind")
	}
	if got, _ := os.ReadFile(target); string(got) != "old" {
		t.Error("target must be untouched after a failed swap")
	}
}

func TestResolveTargetFollowsSymlinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "caddy-2.11.6")
	os.WriteFile(real, []byte("x"), 0o755)
	link := filepath.Join(dir, "caddy")
	os.Symlink(real, link)
	got, exists, err := resolveTarget(link)
	if err != nil || !exists || got != real {
		t.Errorf("symlink should resolve to the real file: %q %v %v", got, exists, err)
	}
	// A first install through a symlinked directory lands in the real one.
	realDir := filepath.Join(dir, "bin")
	os.Mkdir(realDir, 0o755)
	os.Symlink(realDir, filepath.Join(dir, "linkdir"))
	got, exists, err = resolveTarget(filepath.Join(dir, "linkdir", "caddy"))
	if err != nil || exists || got != filepath.Join(realDir, "caddy") {
		t.Errorf("first install via symlinked dir: %q %v %v", got, exists, err)
	}
	if _, _, err := resolveTarget(dir); err == nil {
		t.Error("a directory is not a valid target")
	}
	if _, _, err := resolveTarget(filepath.Join(dir, "no", "such", "caddy")); err == nil {
		t.Error("a missing directory is an error, not a first install")
	}
	// A dangling link is followed to where it points, so a first install
	// lands on the referent and the link then works; relative links are
	// taken from the link's own directory, and chains are followed.
	os.Symlink(filepath.Join(realDir, "caddy-2.12.0"), filepath.Join(realDir, "dangling-abs"))
	got, exists, err = resolveTarget(filepath.Join(dir, "linkdir", "dangling-abs"))
	if err != nil || exists || got != filepath.Join(realDir, "caddy-2.12.0") {
		t.Errorf("dangling absolute link: %q %v %v", got, exists, err)
	}
	os.Symlink("caddy-2.12.0", filepath.Join(realDir, "dangling-rel"))
	got, exists, err = resolveTarget(filepath.Join(realDir, "dangling-rel"))
	if err != nil || exists || got != filepath.Join(realDir, "caddy-2.12.0") {
		t.Errorf("dangling relative link: %q %v %v", got, exists, err)
	}
	os.Symlink("dangling-rel", filepath.Join(realDir, "chain"))
	if got, _, err := resolveTarget(filepath.Join(realDir, "chain")); err != nil || got != filepath.Join(realDir, "caddy-2.12.0") {
		t.Errorf("chain of dangling links: %q %v", got, err)
	}
	os.Symlink("loop-b", filepath.Join(realDir, "loop-a"))
	os.Symlink("loop-a", filepath.Join(realDir, "loop-b"))
	if _, _, err := resolveTarget(filepath.Join(realDir, "loop-a")); err == nil {
		t.Error("a symlink loop is an error")
	}
}

func TestSwapRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	os.WriteFile(real, []byte("old"), 0o755)
	link := filepath.Join(dir, "caddy")
	os.Symlink(real, link)
	staged := filepath.Join(dir, "staged")
	os.WriteFile(staged, []byte("new"), 0o755)
	if _, err := swap(link, staged, nil); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("swap on a symlink must refuse: %v", err)
	}
	if got, _ := os.ReadFile(real); string(got) != "old" {
		t.Error("referent must be untouched")
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the link must still be a link")
	}
}

func TestCommitLockfileUndoesTheSwapOnFailure(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "caddy")
	// First install: the lockfile cannot be installed (missing staged
	// file), so the just-placed binary must be removed again.
	os.WriteFile(target, []byte("new"), 0o755)
	err := commitLockfile(target, filepath.Join(dir, "missing.lock.json"), "")
	if err == nil || !strings.Contains(err.Error(), "removed from") {
		t.Errorf("first install: %v", err)
	}
	if _, statErr := os.Stat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("first install: the new binary must not be left behind")
	}
	// Upgrade: the previous binary is restored.
	os.WriteFile(target, []byte("new"), 0o755)
	os.WriteFile(target+".previous", []byte("old"), 0o755)
	err = commitLockfile(target, filepath.Join(dir, "missing.lock.json"), target+".previous")
	if err == nil || !strings.Contains(err.Error(), "rolled back to the previous binary") {
		t.Errorf("upgrade: %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "old" {
		t.Errorf("upgrade: target should be restored, got %q", got)
	}
	// Success leaves the lockfile in place.
	os.WriteFile(filepath.Join(dir, "staged.lock.json"), []byte("{}"), 0o644)
	if err := commitLockfile(target, filepath.Join(dir, "staged.lock.json"), ""); err != nil {
		t.Errorf("success: %v", err)
	}
}

func fakeEnv(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func fakeLookup(name, group string, extra []string) (*caddybin.Account, error) {
	var a *caddybin.Account
	switch name {
	case "caddy":
		a = &caddybin.Account{Name: "caddy", UID: 999, GID: 999, Groups: []uint32{999}}
	case "1000":
		a = &caddybin.Account{Name: "sean", UID: 1000, GID: 1000, Home: "/home/sean"}
	case "admin":
		a = &caddybin.Account{Name: "admin", UID: 0, GID: 0}
	case "0", "root":
		a = &caddybin.Account{Name: "root", UID: 0, GID: 0, Home: "/root", Groups: []uint32{0}}
	default:
		return nil, errors.New("unknown user " + name)
	}
	if group != "" {
		gid, err := fakeGroup(group)
		if err != nil {
			return nil, err
		}
		a.GID = gid
	}
	for _, g := range extra {
		gid, err := fakeGroup(g)
		if err != nil {
			return nil, err
		}
		a.Groups = append(a.Groups, gid)
	}
	return a, nil
}

func fakeGroup(name string) (uint32, error) {
	switch name {
	case "caddy":
		return 999, nil
	case "www-data":
		return 33, nil
	case "adm":
		return 4, nil
	}
	return 0, errors.New("unknown group " + name)
}

var fakeLookups = lookups{account: fakeLookup, group: fakeGroup}

func TestChooseAccounts(t *testing.T) {
	sudo := fakeEnv(map[string]string{"SUDO_UID": "1000", "SUDO_GID": "1000", "SUDO_USER": "sean"})
	noSudo := fakeEnv(nil)
	units := []systemd.Unit{
		{Name: "caddy.service", User: "caddy", Group: "caddy", Args: []string{"caddy", "run", "--config", "/etc/caddy/Caddyfile"}},
		{Name: "root.service", Args: []string{"caddy", "run", "--config", "/etc/root/Caddyfile"}},
		{Name: "dyn.service", User: "dyn", DynamicUser: true, Args: []string{"caddy", "run", "--config", "/etc/dyn/Caddyfile"}},
	}
	newPlan := func() *Plan {
		return &Plan{Units: units, Validations: validationsFromUnits(units)}
	}
	rootSelf := &caddybin.Account{Name: "root"}
	userSelf := &caddybin.Account{Name: "sean", UID: 1000, GID: 1000, Groups: []uint32{1000, 27}}
	caddySelf := &caddybin.Account{Name: "caddy", UID: 999, GID: 999}

	// Not root: nothing can be switched, so every validation that would
	// have to run as another account (root included) is a reason to need
	// root, named per config; a DynamicUser= unit and a --config run as
	// the caller, and so does a unit that runs as the caller.
	p := newPlan()
	reasons, err := p.chooseAccounts(userSelf, sudo, fakeLookups)
	if err != nil || p.RunAs != "" || p.runAs != nil {
		t.Errorf("as a normal user no inspection account is chosen: %v %+v", err, p)
	}
	if len(reasons) != 2 || !strings.Contains(reasons[0], "/etc/caddy/Caddyfile as caddy, the account caddy.service runs as") || !strings.Contains(reasons[1], "/etc/root/Caddyfile as root") {
		t.Errorf("root reasons for switching accounts: %q", reasons)
	}
	if a := p.Validations[0].account; a == nil || a.UID != 999 || p.Validations[0].User != "caddy" || p.Validations[2].account != nil {
		t.Errorf("accounts recorded for the plan: %+v", p.Validations)
	}
	p = newPlan()
	if reasons, err := p.chooseAccounts(caddySelf, sudo, fakeLookups); err != nil || len(reasons) != 1 || !strings.Contains(reasons[0], "root.service") {
		t.Errorf("a caller who is the unit user needs no root for it: %v %q", err, reasons)
	}
	// The same user with other groups is another identity: a unit with the
	// caller's User= but its own Group= or SupplementaryGroups= still
	// needs root to be validated as the service would read the config.
	grouped := []systemd.Unit{
		{Name: "g.service", User: "caddy", Group: "www-data", Args: []string{"caddy", "run", "--config", "/etc/g"}},
		{Name: "s.service", User: "caddy", SupplementaryGroups: []string{"adm"}, Args: []string{"caddy", "run", "--config", "/etc/s"}},
	}
	p = &Plan{Units: grouped, Validations: validationsFromUnits(grouped)}
	if reasons, err := p.chooseAccounts(caddySelf, noSudo, fakeLookups); err != nil || len(reasons) != 2 || !strings.Contains(reasons[0], "/etc/g as caddy") || !strings.Contains(reasons[1], "/etc/s as caddy") {
		t.Errorf("same user, other groups must need root: %v %q", err, reasons)
	}
	p = &Plan{Validations: []Validation{{Config: "/etc/x", From: "--config"}}}
	if reasons, err := p.chooseAccounts(userSelf, noSudo, fakeLookups); err != nil || len(reasons) != 0 {
		t.Errorf("--config as a normal user: %v %q", err, reasons)
	}
	ghostUnits := []systemd.Unit{{Name: "x.service", User: "ghost", Args: []string{"caddy", "run", "--config", "/etc/x"}}}
	p = &Plan{Units: ghostUnits, Validations: validationsFromUnits(ghostUnits)}
	if _, err := p.chooseAccounts(userSelf, noSudo, fakeLookups); err == nil || !strings.Contains(err.Error(), "x.service") {
		t.Errorf("an unknown unit user is a refusal for a normal user too: %v", err)
	}

	// Root through sudo: the new binary is inspected as the invoker, each
	// unit's config is validated as that unit's own account, a root unit as
	// root, and a DynamicUser= unit (whose account does not exist yet) as
	// the invoker.
	p = newPlan()
	if _, err := p.chooseAccounts(rootSelf, sudo, fakeLookups); err != nil {
		t.Fatal(err)
	}
	if p.runAs == nil || p.runAs.UID != 1000 || !strings.Contains(p.RunAs, "sean (uid 1000)") || !strings.Contains(p.RunAs, "sudo") {
		t.Errorf("inspection account: %+v %q", p.runAs, p.RunAs)
	}
	want := map[string]string{"caddy.service": "caddy", "root.service": "root", "dyn.service": "sean"}
	for _, v := range p.Validations {
		if v.User != want[v.From] {
			t.Errorf("%s validated as %q, want %q", v.From, v.User, want[v.From])
		}
	}
	if a := p.Validations[0].account; a == nil || a.UID != 999 {
		t.Errorf("caddy.service must be validated as caddy: %+v", a)
	}
	if p.Validations[1].account != nil {
		t.Errorf("a unit without User= runs as root and is validated as root: %+v", p.Validations[1].account)
	}
	if p.Validations[2].account != p.runAs {
		t.Errorf("a DynamicUser= unit is validated as the inspection account: %+v", p.Validations[2].account)
	}
	var text strings.Builder
	p.From, p.FromInfo = "/tmp/caddy", &caddybin.Info{}
	p.WriteText(&text)
	if s := text.String(); !strings.Contains(s, "Run as:   sean (uid 1000), the user who ran sudo") || !strings.Contains(s, "(from caddy.service, as caddy)") || !strings.Contains(s, "(from root.service, as root)") {
		t.Errorf("plan text must say who runs what:\n%s", s)
	}

	// Root without sudo: the first unit's user stands in for inspection.
	p = newPlan()
	if _, err := p.chooseAccounts(rootSelf, noSudo, fakeLookups); err != nil || p.runAs == nil || p.runAs.UID != 999 || !strings.Contains(p.RunAs, "service user") {
		t.Errorf("without sudo the service user is used: %v %+v %q", err, p.runAs, p.RunAs)
	}
	// Root through sudo from root is no invoker either.
	p = newPlan()
	if _, err := p.chooseAccounts(rootSelf, fakeEnv(map[string]string{"SUDO_UID": "0", "SUDO_GID": "0", "SUDO_USER": "root"}), fakeLookups); err != nil || p.runAs == nil || p.runAs.UID != 999 {
		t.Errorf("sudo from root is not an invoker to drop to: %v %+v", err, p.runAs)
	}
	// Root, no sudo, no units: root, said plainly, for a --config
	// validation too.
	p = &Plan{Validations: []Validation{{Config: "/etc/x", From: "--config"}}}
	if _, err := p.chooseAccounts(rootSelf, noSudo, fakeLookups); err != nil || p.runAs != nil || !strings.HasPrefix(p.RunAs, "root (") || p.Validations[0].User != "root" || p.Validations[0].account != nil {
		t.Errorf("nothing to drop to must be said, not hidden: %v %+v", err, p)
	}
	// With sudo, a --config validation runs as the invoker.
	p = &Plan{Validations: []Validation{{Config: "/etc/x", From: "--config"}}}
	if _, err := p.chooseAccounts(rootSelf, sudo, fakeLookups); err != nil || p.Validations[0].User != "sean" || p.Validations[0].account != p.runAs {
		t.Errorf("--config validation runs as the invoker: %v %+v", err, p.Validations[0])
	}
	// A unit user that cannot be looked up is a refusal, not a fall back
	// to root.
	ghost := []systemd.Unit{{Name: "x.service", User: "ghost", Args: []string{"caddy", "run", "--config", "/etc/x"}}}
	p = &Plan{Units: ghost, Validations: validationsFromUnits(ghost)}
	if _, err := p.chooseAccounts(rootSelf, sudo, fakeLookups); err == nil || !strings.Contains(err.Error(), "x.service") || !strings.Contains(err.Error(), "could not be looked up") {
		t.Errorf("unknown unit user must refuse: %v", err)
	}
	// User= naming an account with uid 0 is root under another name.
	admin := []systemd.Unit{{Name: "a.service", User: "admin", Args: []string{"caddy", "run", "--config", "/etc/a"}}}
	p = &Plan{Units: admin, Validations: validationsFromUnits(admin)}
	if _, err := p.chooseAccounts(rootSelf, noSudo, fakeLookups); err != nil || p.Validations[0].User != "root" || p.Validations[0].account != nil || p.runAs != nil {
		t.Errorf("uid 0 under another name is root: %v %+v", err, p)
	}
	// A root unit that sets Group= or SupplementaryGroups= still runs with
	// those groups, and a restricted root reads files by them: validate as
	// root with exactly those groups, nothing from the group database.
	grp := []systemd.Unit{
		{Name: "g.service", Group: "www-data", SupplementaryGroups: []string{"adm"}, Args: []string{"caddy", "run", "--config", "/etc/g"}},
		{Name: "s.service", SupplementaryGroups: []string{"adm"}, Args: []string{"caddy", "run", "--config", "/etc/s"}},
		{Name: "u.service", User: "admin", Group: "www-data", Args: []string{"caddy", "run", "--config", "/etc/u"}},
	}
	p = &Plan{Units: grp, Validations: validationsFromUnits(grp)}
	if _, err := p.chooseAccounts(rootSelf, noSudo, fakeLookups); err != nil {
		t.Fatal(err)
	}
	if a := p.Validations[0].account; a == nil || a.UID != 0 || a.GID != 33 || !reflect.DeepEqual(a.Groups, []uint32{4}) || p.Validations[0].User != "root" {
		t.Errorf("root with Group= and SupplementaryGroups=: %+v %q", a, p.Validations[0].User)
	}
	if a := p.Validations[1].account; a == nil || a.UID != 0 || a.GID != 0 || !reflect.DeepEqual(a.Groups, []uint32{4}) {
		t.Errorf("root with SupplementaryGroups= only: %+v", a)
	}
	if a := p.Validations[2].account; a == nil || a.UID != 0 || a.GID != 33 || len(a.Groups) != 0 {
		t.Errorf("uid 0 under another name with Group=: %+v", a)
	}
	if p.runAs != nil {
		t.Errorf("a root account is never the inspection fallback: %+v", p.runAs)
	}
	bad := []systemd.Unit{{Name: "b.service", Group: "nope", Args: []string{"caddy", "run", "--config", "/etc/b"}}}
	p = &Plan{Units: bad, Validations: validationsFromUnits(bad)}
	if _, err := p.chooseAccounts(rootSelf, noSudo, fakeLookups); err == nil || !strings.Contains(err.Error(), "b.service") {
		t.Errorf("an unknown group on a root unit must refuse: %v", err)
	}
}

func TestInvoker(t *testing.T) {
	failing := func(string, string, []string) (*caddybin.Account, error) { return nil, errors.New("nss unavailable") }
	// The invoker is looked up for groups and home, and when that fails
	// the IDs sudo gave are used on their own.
	a, how := invoker(fakeEnv(map[string]string{"SUDO_UID": "1000", "SUDO_GID": "1000", "SUDO_USER": "sean"}), fakeLookup)
	if a == nil || a.Home != "/home/sean" || !strings.Contains(how, "sudo") {
		t.Errorf("looked-up invoker: %+v %q", a, how)
	}
	a, _ = invoker(fakeEnv(map[string]string{"SUDO_UID": "1000", "SUDO_GID": "1001", "SUDO_USER": "sean"}), failing)
	if a == nil || a.UID != 1000 || a.GID != 1001 || a.Name != "sean" || a.Home != "" {
		t.Errorf("fallback to sudo's IDs: %+v", a)
	}
	a, _ = invoker(fakeEnv(map[string]string{"SUDO_UID": "1000", "SUDO_GID": "1001"}), failing)
	if a == nil || a.Name != "1000" {
		t.Errorf("without SUDO_USER the name is the ID: %+v", a)
	}
	// Missing or malformed variables mean no invoker.
	for name, vars := range map[string]map[string]string{
		"none":      nil,
		"garbage":   {"SUDO_UID": "abc", "SUDO_GID": "1000"},
		"no gid":    {"SUDO_UID": "1000"},
		"root":      {"SUDO_UID": "0", "SUDO_GID": "0"},
		"too large": {"SUDO_UID": "99999999999", "SUDO_GID": "0"},
	} {
		if a, _ := invoker(fakeEnv(vars), fakeLookup); a != nil {
			t.Errorf("%s: want no invoker, got %+v", name, a)
		}
	}
}
