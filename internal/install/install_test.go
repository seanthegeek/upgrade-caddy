package install

import (
	"context"
	"errors"

	"github.com/seanthegeek/upgrade-caddy/internal/build"
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

	previous, err := swap(target, newPath, "")
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

	if err := rollback(target, previous); err != nil {
		t.Fatal(err)
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

func TestSwapFirstInstall(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "caddy")
	newPath := filepath.Join(dir, "staged")
	os.WriteFile(newPath, []byte("new"), 0o755)
	previous, err := swap(target, newPath, "")
	if err != nil || previous != "" {
		t.Fatalf("first install: previous=%q err=%v", previous, err)
	}
	if err := rollback(target, previous); err == nil {
		t.Error("rollback with no previous must fail")
	}
}

// fakeCtl records calls and answers IsActive from a script.
type fakeCtl struct {
	restarts []string
	active   []bool
	pid      int
}

func (f *fakeCtl) Restart(_ context.Context, unit string) error {
	f.restarts = append(f.restarts, unit)
	return nil
}

func (f *fakeCtl) IsActive(_ context.Context, _ string) (bool, error) {
	if len(f.active) == 0 {
		return false, nil
	}
	a := f.active[0]
	f.active = f.active[1:]
	return a, nil
}

func (f *fakeCtl) MainPID(_ context.Context, _ string) (int, error) { return f.pid, nil }

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
	p := &Plan{Config: "/etc/caddy/Caddyfile", Adapter: "caddyfile", EnvFiles: []string{"/a.env", "/b.env"}, WorkDir: dir}
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
