// Package fspath resolves file paths through symbolic links, including a
// final link whose referent does not exist yet.
package fspath

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// maxLinks is how many links a chain may have before it is taken for a
// loop, the same bound the kernel uses.
const maxLinks = 40

// Resolve follows every symbolic link in path. exists reports whether the
// final file is there; when it is not, real is where it would be: the
// directory resolved, and a dangling final link followed to its referent
// (a relative link from the link's own directory, chains followed, a loop
// refused). A missing directory on the way is an error, not "does not
// exist": nothing could be created there.
func Resolve(path string) (real string, exists bool, err error) {
	return resolve(path, 0)
}

func resolve(path string, depth int) (real string, exists bool, err error) {
	real, err = filepath.EvalSymlinks(path)
	if err == nil {
		return real, true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", false, fmt.Errorf("%s: %w", filepath.Dir(path), err)
	}
	final := filepath.Join(dir, filepath.Base(path))
	fi, err := os.Lstat(final)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return final, false, nil
	case err != nil:
		// Only "not there" means a first install; a permission or I/O
		// failure is an error, never "absent".
		return "", false, err
	case fi.Mode()&os.ModeSymlink == 0:
		// It appeared between the two looks: it exists after all.
		return final, true, nil
	}
	if depth >= maxLinks {
		return "", false, fmt.Errorf("%s: too many levels of symbolic links", path)
	}
	dest, err := os.Readlink(final)
	if err != nil {
		return "", false, err
	}
	if !filepath.IsAbs(dest) {
		dest = filepath.Join(dir, dest)
	}
	return resolve(dest, depth+1)
}
