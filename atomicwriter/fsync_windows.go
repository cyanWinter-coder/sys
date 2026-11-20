//go:build windows

package atomicwriter

// syncDir is a no-op on Windows, where directory handles cannot be used
// with FlushFileBuffers. File contents are already flushed by the
// individual syncing writes, and MoveFileEx renames are ordered
// transactionally enough for the recovery journal to reconcile a crash.
func syncDir(string) error {
	return nil
}
