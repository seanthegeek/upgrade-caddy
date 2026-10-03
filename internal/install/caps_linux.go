package install

import (
	"errors"
	"fmt"
	"syscall"
)

// capXattr is where Linux stores file capabilities. Copying the raw value
// between files is exactly what `cp --preserve=xattr` does and needs no
// getcap/setcap binaries; setting it requires CAP_SETFCAP (root).
const capXattr = "security.capability"

// readCaps returns the target's file capabilities as raw bytes, nil when it
// has none, and an error when that could not be determined, so a caller
// never mistakes "could not read" for "no capabilities".
func readCaps(path string) ([]byte, error) {
	buf := make([]byte, 256)
	n, err := syscall.Getxattr(path, capXattr, buf)
	switch {
	case err == nil:
		return append([]byte(nil), buf[:n]...), nil
	case errors.Is(err, syscall.ENODATA), errors.Is(err, syscall.ENOTSUP), errors.Is(err, syscall.EOPNOTSUPP):
		return nil, nil
	}
	return nil, fmt.Errorf("reading file capabilities of %s: %w", path, err)
}

// writeCaps applies raw file capabilities to path.
func writeCaps(path string, caps []byte) error {
	if err := syscall.Setxattr(path, capXattr, caps, 0); err != nil {
		return fmt.Errorf("setting file capabilities on %s: %w", path, err)
	}
	return nil
}
