package user

import (
	"fmt"
	"math"
	"os"
)

// MkdirOpt is a type for options to pass to Mkdir calls
type MkdirOpt func(*mkdirOptions)

type mkdirOptions struct {
	onlyNew bool
}

// WithOnlyNew configures [MkdirAllAndChown] and [MkdirAndChown] to leave
// the requested directory unchanged if it already exists. Newly created
// directories are still assigned the requested ownership and permissions.
func WithOnlyNew(o *mkdirOptions) {
	o.onlyNew = true
}

// MkdirAllAndChown creates a directory named path, along with any necessary
// parents, and applies the requested ownership and permission bits to all
// directories it creates.
//
// If path already exists as a directory, its ownership and permission bits
// are updated unless [WithOnlyNew] is specified. Existing parent directories
// are not modified.
//
// MkdirAllAndChown is similar to [os.MkdirAll], but also applies ownership and
// reapplies the requested permission bits after creation, so the resulting
// permissions are not affected by the process umask.
func MkdirAllAndChown(path string, mode os.FileMode, uid, gid int, opts ...MkdirOpt) error {
	return mkdirAs(path, mode, uid, gid, true, opts...)
}

// MkdirAndChown creates a directory named path and applies the requested
// ownership and permission bits.
//
// If path already exists as a directory, its ownership and permission bits
// are updated unless [WithOnlyNew] is specified.
//
// MkdirAndChown is similar to [os.Mkdir], but returns nil if path already
// exists as a directory. The resulting permission bits are not affected by
// the process umask.
func MkdirAndChown(path string, mode os.FileMode, uid, gid int, opts ...MkdirOpt) error {
	return mkdirAs(path, mode, uid, gid, false, opts...)
}

// getRootUIDGID retrieves the remapped root uid/gid pair from the set of maps.
// If the maps are empty, then the root uid/gid will default to "real" 0/0
func getRootUIDGID(uidMap, gidMap []IDMap) (int, int, error) {
	uid, err := toHost(0, uidMap)
	if err != nil {
		return -1, -1, err
	}
	gid, err := toHost(0, gidMap)
	if err != nil {
		return -1, -1, err
	}
	return uid, gid, nil
}

// toContainer takes an id mapping, and uses it to translate a
// host ID to the remapped ID. If no map is provided (nil or empty), then the
// translation assumes a 1-to-1 mapping and returns the passed in id
func toContainer(hostID int, idMap []IDMap) (int, error) {
	if len(idMap) == 0 {
		return hostID, nil
	}
	contID, ok := lookupContainer(int64(hostID), idMap)
	if !ok {
		return -1, fmt.Errorf("host ID %d cannot be mapped to a container ID", hostID)
	}
	if contID > math.MaxInt {
		return -1, fmt.Errorf("host ID %d maps to container ID %d, which does not fit in int", hostID, contID)
	}
	return int(contID), nil
}

// toHost takes an id mapping and a remapped ID, and translates the
// ID to the mapped host ID. If no map is provided (nil or empty), then the
// translation assumes a 1-to-1 mapping and returns the passed in id #
func toHost(contID int, idMap []IDMap) (int, error) {
	if len(idMap) == 0 {
		return contID, nil
	}
	hostID, ok := lookupHost(int64(contID), idMap)
	if !ok {
		return -1, fmt.Errorf("container ID %d cannot be mapped to a host ID", contID)
	}
	if hostID > math.MaxInt {
		return -1, fmt.Errorf("container ID %d maps to host ID %d, which does not fit in int", contID, hostID)
	}
	return int(hostID), nil
}

// lookupHost finds the host ID mapped to contID. It reports false when no
// entry covers contID. Entries are scanned in order and the first match wins,
// which preserves the historical behavior for mappings that were not passed
// through [ValidateIDMaps]; validated mappings have ordered, non-overlapping
// container ranges, so there is at most one match.
func lookupHost(contID int64, idMap []IDMap) (int64, bool) {
	for _, m := range idMap {
		// Compare the offset rather than start+count so an unvalidated entry
		// with a huge count cannot overflow.
		if contID >= m.ID && contID-m.ID < m.Count {
			return m.ParentID + (contID - m.ID), true
		}
	}
	return 0, false
}

// lookupContainer finds the container ID mapped to hostID. It reports false
// when no entry covers hostID. Host ranges are assumed to be non-overlapping,
// as enforced by [ValidateIDMaps], so at most one entry can match; they are
// not required to be ordered.
func lookupContainer(hostID int64, idMap []IDMap) (int64, bool) {
	for _, m := range idMap {
		// Compare the offset rather than start+count so an unvalidated entry
		// with a huge count cannot overflow.
		if hostID >= m.ParentID && hostID-m.ParentID < m.Count {
			return m.ID + (hostID - m.ParentID), true
		}
	}
	return 0, false
}

// IdentityMapping contains a mappings of UIDs and GIDs.
// The zero value represents an empty mapping.
type IdentityMapping struct {
	UIDMaps []IDMap `json:"UIDMaps"`
	GIDMaps []IDMap `json:"GIDMaps"`
}

// RootPair returns a uid and gid pair for the root user. The error is ignored
// because a root user always exists, and the defaults are correct when the uid
// and gid maps are empty.
//
// If either mapping is malformed, RootPair returns the pair -1, -1 rather than
// deriving ownership from an ambiguous range. Use [IdentityMapping.Validate]
// (or [ValidateIdentityMapping]) to obtain a detailed error.
func (i IdentityMapping) RootPair() (int, int) {
	if err := i.Validate(); err != nil {
		return -1, -1
	}
	uid, gid, _ := getRootUIDGID(i.UIDMaps, i.GIDMaps)
	return uid, gid
}

// ToHost returns the host UID and GID for the container uid and gid.
// An empty mapping is treated as identity.
//
// The mapping is validated before it is used, so a malformed mapping (entries
// that are unordered, overlap, or do not fit in the 32-bit ID space) is
// rejected with an [*IDMapError] instead of producing a result that depends on
// which conflicting entry happens to come first.
//
// To validate once and reuse a single immutable snapshot for many conversions,
// use [ValidateIdentityMapping] and [ValidatedIdentityMapping.ToHost].
func (i IdentityMapping) ToHost(uid, gid int) (int, int, error) {
	if err := i.Validate(); err != nil {
		return -1, -1, err
	}
	ruid, err := toHost(uid, i.UIDMaps)
	if err != nil {
		return -1, -1, err
	}
	rgid, err := toHost(gid, i.GIDMaps)
	return ruid, rgid, err
}

// ToContainer returns the container UID and GID for the host uid and gid.
//
// The mapping is validated before it is used; see [IdentityMapping.ToHost]
// for details.
func (i IdentityMapping) ToContainer(uid, gid int) (int, int, error) {
	if err := i.Validate(); err != nil {
		return -1, -1, err
	}
	ruid, err := toContainer(uid, i.UIDMaps)
	if err != nil {
		return -1, -1, err
	}
	rgid, err := toContainer(gid, i.GIDMaps)
	return ruid, rgid, err
}

// Empty returns true if there are no id mappings
func (i IdentityMapping) Empty() bool {
	return len(i.UIDMaps) == 0 && len(i.GIDMaps) == 0
}
