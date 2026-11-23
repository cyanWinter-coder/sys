//go:build !windows

package symlink

import "os"

// sameFile reports whether a and b refer to the same directory entry. On Unix
// this compares device and inode, which change when an entry is replaced by
// rename or unlink+create.
func sameFile(a, b os.FileInfo) bool {
	return os.SameFile(a, b)
}
