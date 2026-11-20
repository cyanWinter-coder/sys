//go:build unix

package atomicwriter

import (
	"errors"
	"syscall"
)

// isCrossDeviceRename reports whether err is the cross-device rename
// error, meaning the staged tree must be copied rather than renamed onto
// the target's filesystem.
func isCrossDeviceRename(err error) bool {
	return errors.Is(err, syscall.EXDEV)
}
