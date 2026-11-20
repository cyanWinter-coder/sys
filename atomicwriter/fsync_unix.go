//go:build unix

package atomicwriter

import (
	"os"

	"golang.org/x/sys/unix"
)

// syncDir flushes the directory's in-memory entries to disk so that
// renames and unlinks performed inside it survive a power loss.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return unix.Fsync(int(f.Fd()))
}
