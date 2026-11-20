//go:build windows

package atomicwriter

import (
	"io"

	"golang.org/x/sys/windows"
)

// windowsLock wraps a file handle holding an exclusive byte-range lock.
type windowsLock struct {
	handle windows.Handle
}

func (l *windowsLock) Close() error {
	var ol windows.Overlapped
	err := windows.UnlockFileEx(l.handle, 0, 1, 0, &ol)
	if err1 := windows.CloseHandle(l.handle); err == nil {
		err = err1
	}
	return err
}

// acquireTxnLock takes an exclusive lock on the first byte of path,
// creating the lock file first. The call blocks until the lock is
// available. A process death releases the lock, which is what makes crash
// recovery safe.
func acquireTxnLock(path string) (io.Closer, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		pathPtr,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, err
	}
	var ol windows.Overlapped
	if err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &ol); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	return &windowsLock{handle: handle}, nil
}
