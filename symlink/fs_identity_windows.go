//go:build windows

package symlink

import "os"

// sameFile reports whether a and b refer to the same file. os.SameFile uses the
// volume serial number and file index on Windows, which change when an entry is
// replaced, so a re-created symlink does not compare equal to the old one.
func sameFile(a, b os.FileInfo) bool {
	return os.SameFile(a, b)
}
