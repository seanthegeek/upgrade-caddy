//go:build !linux

package install

// File capabilities are a Linux concept; other systems have none to carry
// over, and that is a definite answer rather than an unknown one.
func readCaps(string) ([]byte, error) { return nil, nil }
func writeCaps(string, []byte) error  { return nil }
