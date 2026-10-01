package install

import (
	"context"
	"errors"

	"github.com/seanthegeek/upgrade-caddy/internal/build"
	"github.com/seanthegeek/upgrade-caddy/internal/systemd"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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
		got, reasons := needsRoot(c.euid, c.writable, "/usr/bin", c.restart, "caddy.service", c.caps)
		if got != c.want || len(reasons) != c.reasons {
			t.Errorf("%s: got %v %v", c.name, got, reasons)
		}
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
	_, err := restartUnits(context.Background(), &fakeCtl{}, []string{"a.service"}, target, "", 50*time.Millisecond, &log)
	if err == nil || !strings.Contains(err.Error(), "removed from") {
		t.Errorf("first install restart failure: %v", err)
	}
	for _, f := range []string{target, target + ".lock.json"} {
		if _, statErr := os.Stat(f); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("%s must be removed again after a failed first-install restart", f)
		}
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
	if len(ctl.stops) != 1 || ctl.stops[0] != "a.service" {
		t.Errorf("the already-restarted unit must be stopped again: %v", ctl.stops)
	}
	if !strings.Contains(err.Error(), "1 already-restarted unit(s) stopped") {
		t.Errorf("the error should say what was undone: %v", err)
	}
	if _, statErr := os.Stat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("the new binary must be removed again")
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
	// And the operator-facing summary says the rollback succeeded.
	if msg := rolledBack(restoreErr); !strings.Contains(msg, "rolled back to the previous binary") {
		t.Errorf("summary for a successful restore with a failed diagnostic copy: %q", msg)
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
	// previous points at a file that does not exist, so the restore fails.
	var log strings.Builder
	_, err := restartUnits(context.Background(), &fakeCtl{}, []string{"a.service"}, target, filepath.Join(dir, "gone"), 50*time.Millisecond, &log)
	if err == nil || !strings.Contains(err.Error(), "ROLLBACK FAILED") || strings.Contains(err.Error(), "rolled back to") {
		t.Errorf("a failed restore must not be reported as rolled back: %v", err)
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
	activeErr     error
	pidErr        error
}

func (f *fakeCtl) Restart(_ context.Context, unit string) error {
	f.restarts = append(f.restarts, unit)
	return f.restartErr
}

func (f *fakeCtl) Stop(_ context.Context, unit string) error {
	f.stops = append(f.stops, unit)
	return nil
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
