package fspath

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolve(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "bin")
	os.Mkdir(realDir, 0o755)
	os.Symlink(realDir, filepath.Join(dir, "linkdir"))
	file := filepath.Join(realDir, "caddy-2.11.6")
	os.WriteFile(file, []byte("x"), 0o755)
	os.Symlink(file, filepath.Join(realDir, "caddy"))
	os.Symlink(filepath.Join(realDir, "caddy-2.12.0"), filepath.Join(realDir, "dangling-abs"))
	os.Symlink("caddy-2.12.0", filepath.Join(realDir, "dangling-rel"))
	os.Symlink("dangling-rel", filepath.Join(realDir, "chain"))
	os.Symlink("loop-b", filepath.Join(realDir, "loop-a"))
	os.Symlink("loop-a", filepath.Join(realDir, "loop-b"))
	cases := []struct {
		name, path, want string
		exists, fails    bool
	}{
		{"plain file", file, file, true, false},
		{"link to a file", filepath.Join(realDir, "caddy"), file, true, false},
		{"missing file", filepath.Join(realDir, "new"), filepath.Join(realDir, "new"), false, false},
		{"missing file via linked dir", filepath.Join(dir, "linkdir", "new"), filepath.Join(realDir, "new"), false, false},
		{"dangling absolute link", filepath.Join(dir, "linkdir", "dangling-abs"), filepath.Join(realDir, "caddy-2.12.0"), false, false},
		{"dangling relative link", filepath.Join(realDir, "dangling-rel"), filepath.Join(realDir, "caddy-2.12.0"), false, false},
		{"chain of dangling links", filepath.Join(realDir, "chain"), filepath.Join(realDir, "caddy-2.12.0"), false, false},
		{"loop", filepath.Join(realDir, "loop-a"), "", false, true},
		{"missing directory", filepath.Join(dir, "no", "such", "caddy"), "", false, true},
	}
	for _, c := range cases {
		got, exists, err := Resolve(c.path)
		if (err != nil) != c.fails || got != c.want || exists != c.exists {
			t.Errorf("%s: got %q %v %v, want %q %v fails=%v", c.name, got, exists, err, c.want, c.exists, c.fails)
		}
	}
}
