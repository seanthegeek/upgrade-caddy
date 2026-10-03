package build

import "os"

func writeFile(path string) error { return os.WriteFile(path, []byte("x"), 0o755) }
