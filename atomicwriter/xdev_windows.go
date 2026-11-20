//go:build windows

package atomicwriter

import (
	"errors"
	"syscall"
)

// errNotSameDevice is returned by MoveFile when source and destination
// live on different volumes. It is not part of the portable syscall.Errno
// table, so define it explicitly.
var errNotSameDevice = syscall.Errno(17) // ERROR_NOT_SAME_DEVICE

// isCrossDeviceRename reports whether err is the cross-device rename
// error, meaning the staged tree must be copied rather than renamed onto
// the target's filesystem.
func isCrossDeviceRename(err error) bool {
	return errors.Is(err, errNotSameDevice)
}
