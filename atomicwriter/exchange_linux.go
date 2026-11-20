//go:build linux

package atomicwriter

import (
	"errors"

	"golang.org/x/sys/unix"
)

// forceJournaled is flipped by tests to exercise the journaled fallback
// path on platforms that normally support atomic exchange.
var forceJournaled = false

// exchangeDir atomically swaps two directories on the same filesystem
// using renameat2(2) with RENAME_EXCHANGE: after the call, newPath and
// oldPath exchange places in a single operation, so an observer can only
// ever see one complete version at either name.
func exchangeDir(newPath, oldPath string) error {
	if forceJournaled {
		return errExchangeUnsupported
	}
	return unix.Renameat2(unix.AT_FDCWD, newPath, unix.AT_FDCWD, oldPath, unix.RENAME_EXCHANGE)
}

// isExchangeUnsupported reports whether the failure of exchangeDir means
// the platform or filesystem cannot atomically exchange directories, in
// which case the caller falls back to the journaled sequence.
func isExchangeUnsupported(err error) bool {
	switch {
	case errors.Is(err, errExchangeUnsupported),
		errors.Is(err, unix.ENOSYS),
		errors.Is(err, unix.EINVAL),
		errors.Is(err, unix.EXDEV),
		errors.Is(err, unix.EPERM):
		return true
	default:
		return false
	}
}
