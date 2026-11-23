//go:build freebsd || openbsd || darwin

package mountinfo

import "golang.org/x/sys/unix"

// parseMountTable returns information about mounted filesystems
func parseMountTable(filter FilterFunc) ([]*Info, error) {
	count, err := unix.Getfsstat(nil, unix.MNT_WAIT)
	if err != nil {
		return nil, err
	}

	entries := make([]unix.Statfs_t, count)
	_, err = unix.Getfsstat(entries, unix.MNT_WAIT)
	if err != nil {
		return nil, err
	}

	// Build the complete snapshot first, then apply the filter, so the
	// same mount table version is used throughout one call.
	infos := make([]*Info, 0, count)
	for i := range entries {
		infos = append(infos, getMountinfo(&entries[i]))
	}
	return applyFilter(infos, filter), nil
}

func mounted(path string) (bool, error) {
	path, err := normalizePath(path)
	if err != nil {
		return false, err
	}
	// Fast path: compare st.st_dev fields.
	// This should always work for FreeBSD and OpenBSD.
	mounted, err := mountedByStat(path)
	if err == nil {
		return mounted, nil
	}

	// Fallback to parsing mountinfo
	return mountedByMountinfo(path)
}
