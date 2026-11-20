//go:build unix

package atomicwriter

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// unixLock wraps a file holding an exclusive advisory flock.
type unixLock struct {
	f *os.File
}

func (l *unixLock) Close() error {
	err := unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	if err1 := l.f.Close(); err == nil {
		err = err1
	}
	return err
}

// acquireTxnLock takes an exclusive advisory lock on path, creating the
// lock file first. The call blocks until the lock is available. The lock
// is released when the returned closer is closed, or automatically by the
// kernel if the process dies, which is what makes crash recovery safe.
func acquireTxnLock(path string) (io.Closer, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &unixLock{f: f}, nil
}
