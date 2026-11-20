//go:build darwin

package atomicwriter

import (
	"errors"

	"golang.org/x/sys/unix"
)

// forceJournaled is flipped by tests to exercise the journaled fallback
// path on platforms that normally support atomic exchange.
var forceJournaled = false

// exchangeDir atomically swaps two directories on the same volume using
// renamex_np(2) with RENAME_SWAP: after the call, newPath and oldPath
// exchange places in a single operation, so an observer can only ever see
// one complete version at either name.
func exchangeDir(newPath, oldPath string) error {
	if forceJournaled {
		return errExchangeUnsupported
	}
	return unix.RenamexNp(newPath, oldPath, unix.RENAME_SWAP)
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
		errors.Is(err, unix.EOPNOTSUPP):
		return true
	default:
		return false
	}
}
