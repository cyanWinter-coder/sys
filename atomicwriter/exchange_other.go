//go:build !linux && !darwin && !windows

package atomicwriter

// forceJournaled exists for symmetry with the Linux and Darwin variants;
// the journaled path is always used on this platform.
var forceJournaled = false

// exchangeDir is unsupported on this platform, which has no system call
// for atomically exchanging two directories. The journaled rename
// sequence is used instead.
func exchangeDir(newPath, oldPath string) error {
	return errExchangeUnsupported
}

func isExchangeUnsupported(err error) bool {
	return err == errExchangeUnsupported
}
