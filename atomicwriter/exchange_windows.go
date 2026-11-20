//go:build windows

package atomicwriter

// forceJournaled exists for symmetry with the Unix variants; the journaled
// path is always used on Windows.
var forceJournaled = false

// exchangeDir is unsupported on Windows, which has no system call for
// atomically exchanging two directories. The journaled rename sequence is
// used instead.
func exchangeDir(newPath, oldPath string) error {
	return errExchangeUnsupported
}

func isExchangeUnsupported(err error) bool {
	return err == errExchangeUnsupported
}
