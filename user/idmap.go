// Package user provides utilities for parsing and working with user and group
// databases, including the UID/GID mappings used by Linux user namespaces.
//
// # ID mapping validation
//
// [IdentityMapping] describes a user-namespace UID and GID mapping. Mappings
// read from untrusted sources, or assembled by hand, can be inconsistent:
// entries may be out of order, overlap each other, or exceed the 32-bit ID
// space. Converting IDs with such a mapping would produce results that depend
// on which conflicting entry happens to come first.
//
// [ValidateIDMaps] and [IdentityMapping.Validate] reject such mappings and
// report the offending entry together with the earlier entry it conflicts
// with. [ValidateIdentityMapping] additionally freezes the validated result
// into a [ValidatedIdentityMapping], an immutable snapshot with private copies
// of both mappings. Conversions through a snapshot always use exactly one
// validated mapping, which makes them repeatable and safe for concurrent use.
//
// [IdentityMappingStore] publishes validated snapshots atomically, so a
// concurrent update never mixes the UID mapping of one update with the GID
// mapping of another. On platforms without user-namespace ID mapping support,
// an empty mapping is not silently treated as identity: operations return
// [ErrIDMapUnsupported] instead.
package user

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"sync/atomic"
)

// maxIDMapID is the upper bound of a mapped ID. The kernel cannot map ID
// 4294967295 ((uid_t)-1), so the exclusive end of a range must not exceed
// 4294967295. This matches the checks in the kernel's map_write:
//
//	(extent.first + extent.count) <= extent.first   /* would wrap */
//
// See https://github.com/torvalds/linux/blob/v6.18/kernel/user_namespace.c
const maxIDMapID = int64(1<<32 - 1)

// IDMapKind identifies the kind of an ID mapping (UID or GID). It is used
// in validation errors so callers can tell which of the two mappings of an
// [IdentityMapping] is invalid.
type IDMapKind uint8

const (
	// IDMapUID identifies the UID mapping.
	IDMapUID IDMapKind = iota + 1
	// IDMapGID identifies the GID mapping.
	IDMapGID
)

// String returns "UID", "GID", or "unknown".
func (k IDMapKind) String() string {
	switch k {
	case IDMapUID:
		return "UID"
	case IDMapGID:
		return "GID"
	default:
		return "unknown"
	}
}

// Errors returned (possibly wrapped in an [*IDMapError]) by the ID mapping
// validation functions. They can be tested with [errors.Is].
var (
	// ErrIDMapNotSorted is reported when a mapping entry's container ID is
	// lower than the container ID of the entry before it.
	ErrIDMapNotSorted = errors.New("mapping entries are not ordered by container ID")

	// ErrIDMapOverlap is reported when two mapping entries cover the same
	// container ID or the same host ID.
	ErrIDMapOverlap = errors.New("mapping entry overlaps an earlier entry")

	// ErrIDMapOutOfRange is reported when a mapping entry does not fit in the
	// unsigned 32-bit ID space.
	ErrIDMapOutOfRange = errors.New("mapping entry is outside the 32-bit ID range")

	// ErrIDMapInvalidCount is reported when a mapping entry has a count that
	// is zero or negative.
	ErrIDMapInvalidCount = errors.New("mapping entry has a non-positive count")

	// ErrIDMapUnmapped is reported by conversion functions when an ID is not
	// covered by any mapping range.
	ErrIDMapUnmapped = errors.New("ID is not covered by any mapping range")

	// ErrIDMapUnsupported is the uniform error returned by operations that
	// require Linux user-namespace support when the platform does not provide
	// it. In particular, an empty mapping is never silently treated as an
	// identity mapping on such platforms.
	ErrIDMapUnsupported = errors.New("ID mapping is not supported on this platform")
)

// IDMapError describes an invalid entry of a UID or GID mapping. It
// identifies the kind of mapping, the offending entry, and, for conflicts,
// the earlier entry it conflicts with.
//
// The error wraps one of [ErrIDMapNotSorted], [ErrIDMapOverlap],
// [ErrIDMapOutOfRange], or [ErrIDMapInvalidCount], so both [errors.Is] and
// [errors.As] can be used to inspect it.
type IDMapError struct {
	// Kind is the mapping that contains the offending entry.
	Kind IDMapKind
	// Index is the position of the offending entry in the mapping slice.
	Index int
	// ConflictIndex is the position of the earlier entry that the offending
	// entry conflicts with, or -1 if the failure is not a conflict.
	ConflictIndex int
	// Entry is the offending mapping entry.
	Entry IDMap
	// Conflict is the earlier entry identified by ConflictIndex. It is the
	// zero value when ConflictIndex is -1.
	Conflict IDMap
	// Err is the underlying sentinel error.
	Err error

	detail string
}

// Error implements the error interface.
func (e *IDMapError) Error() string {
	detail := e.detail
	if detail == "" {
		if e.Err != nil {
			detail = e.Err.Error()
		} else {
			detail = "invalid mapping"
		}
	}
	return fmt.Sprintf("user: %s mapping: entry %d [%s]: %s",
		e.Kind, e.Index, formatIDMapRange(e.Entry), detail)
}

// Unwrap returns the underlying sentinel error.
func (e *IDMapError) Unwrap() error {
	return e.Err
}

// formatIDMapRange renders an entry as a pair of half-open ranges.
func formatIDMapRange(m IDMap) string {
	return fmt.Sprintf("container [%d, %d) -> host [%d, %d)",
		m.ID, saturatingAdd(m.ID, m.Count), m.ParentID, saturatingAdd(m.ParentID, m.Count))
}

// saturatingAdd returns a+b, clamping at math.MaxInt64 instead of wrapping.
// It is only used to render ranges in error messages.
func saturatingAdd(a, b int64) int64 {
	if sum := a + b; sum >= a {
		return sum
	}
	return math.MaxInt64
}

func newIDMapError(kind IDMapKind, index, conflictIndex int, maps []IDMap, err error, detail string) *IDMapError {
	e := &IDMapError{
		Kind:          kind,
		Index:         index,
		ConflictIndex: conflictIndex,
		Err:           err,
		detail:        detail,
	}
	if index >= 0 && index < len(maps) {
		e.Entry = maps[index]
	}
	if conflictIndex >= 0 && conflictIndex < len(maps) {
		e.Conflict = maps[conflictIndex]
	}
	return e
}

// ValidateIDMaps validates a single UID or GID mapping. It reports an error
// if:
//   - an entry has a non-positive count;
//   - an entry maps IDs outside the unsigned 32-bit ID space, that is an ID
//     is negative or the exclusive end of a range exceeds 4294967295;
//   - entries are not ordered by container ID (container ranges must not
//     overlap); or
//   - an entry's host range overlaps the host range of an earlier entry.
//
// Host ranges do not need to be ordered, but they must not overlap, because
// the host-to-container conversion would otherwise be ambiguous.
//
// The checks mirror the ones the kernel applies when a uid_map or gid_map is
// written (see user_namespaces(7)), so a mapping accepted here can be handed
// to the kernel.
//
// The returned error, if any, is an [*IDMapError] identifying the mapping
// kind, the offending entry, and, for conflicts, the earlier entry it
// conflicts with. Validation is deterministic and does not modify maps.
func ValidateIDMaps(kind IDMapKind, maps []IDMap) error {
	// maxContainerEnd is the exclusive end of the container range covered by
	// the entries seen so far. Because entries are ordered, it is enough to
	// compare each entry with this running maximum: any earlier entry that
	// overlaps must end at or before it, and an entry starting before it
	// overlaps the entry that produced it.
	var (
		maxContainerEnd int64
		maxContainerIdx int
	)

	for i, m := range maps {
		if m.Count <= 0 {
			return newIDMapError(kind, i, -1, maps, ErrIDMapInvalidCount,
				fmt.Sprintf("count must be positive, got %d", m.Count))
		}
		if m.ID < 0 || m.ParentID < 0 {
			return newIDMapError(kind, i, -1, maps, ErrIDMapOutOfRange,
				"IDs must not be negative")
		}
		// The exclusive end of each range must stay within maxIDMapID. The
		// kernel rejects a range that wraps or whose end is (uid_t)-1.
		if m.ID >= maxIDMapID || m.Count > maxIDMapID-m.ID ||
			m.ParentID >= maxIDMapID || m.Count > maxIDMapID-m.ParentID {
			return newIDMapError(kind, i, -1, maps, ErrIDMapOutOfRange,
				fmt.Sprintf("%s does not fit in the unsigned 32-bit ID space", formatIDMapRange(m)))
		}

		if i > 0 {
			if m.ID < maps[i-1].ID {
				return newIDMapError(kind, i, i-1, maps, ErrIDMapNotSorted,
					fmt.Sprintf("container ID %d is lower than entry %d [%s]", m.ID, i-1, formatIDMapRange(maps[i-1])))
			}
			if m.ID < maxContainerEnd {
				return newIDMapError(kind, i, maxContainerIdx, maps, ErrIDMapOverlap,
					fmt.Sprintf("container range overlaps entry %d [%s]", maxContainerIdx, formatIDMapRange(maps[maxContainerIdx])))
			}
		}
		if end := m.ID + m.Count; end > maxContainerEnd {
			maxContainerEnd = end
			maxContainerIdx = i
		}

		for j := range i {
			prev := maps[j]
			if idMapRangesOverlap(m.ParentID, m.Count, prev.ParentID, prev.Count) {
				return newIDMapError(kind, i, j, maps, ErrIDMapOverlap,
					fmt.Sprintf("host range overlaps entry %d [%s]", j, formatIDMapRange(prev)))
			}
		}
	}
	return nil
}

// idMapRangesOverlap reports whether the half-open ranges [s1, s1+c1) and
// [s2, s2+c2) intersect. Both counts must be positive.
func idMapRangesOverlap(start1, count1, start2, count2 int64) bool {
	return start1 < start2+count2 && start2 < start1+count1
}

// NormalizeIDMaps returns a copy of maps sorted by container ID. Entries with
// equal container IDs keep their relative order, so a subsequent
// [ValidateIDMaps] reports the original conflict deterministically.
//
// Normalization is needed because the kernel preserves the order in which
// extents were written to uid_map/gid_map when there are at most five of them,
// so [ParseIDMap] of /proc/PID/uid_map may return entries that are valid but
// not ordered. NormalizeIDMaps never modifies its argument.
func NormalizeIDMaps(maps []IDMap) []IDMap {
	out := cloneIDMaps(maps)
	slices.SortStableFunc(out, func(a, b IDMap) int {
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		default:
			return 0
		}
	})
	return out
}

// Validate validates both the UID and the GID mappings of i. The two mappings
// are validated independently, so an invalid GID mapping is reported even
// when the UID mapping is also invalid (both errors are joined).
//
// Validate does not modify i and can be called any number of times; the
// result is the same for the same input.
func (i IdentityMapping) Validate() error {
	var errs []error
	if err := ValidateIDMaps(IDMapUID, i.UIDMaps); err != nil {
		errs = append(errs, err)
	}
	if err := ValidateIDMaps(IDMapGID, i.GIDMaps); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// ValidatedIdentityMapping is an immutable snapshot of a validated
// [IdentityMapping]. It can only be created through
// [ValidateIdentityMapping] (the zero value is an empty mapping), and it
// keeps private copies of the UID and GID mappings, so modifying the
// original [IdentityMapping] after validation does not affect the snapshot.
//
// All conversion methods operate on the single snapshot they are called on,
// which makes conversions repeatable and safe to use concurrently.
//
// On platforms without user-namespace ID mapping support, conversions of an
// empty snapshot return [ErrIDMapUnsupported] instead of silently treating
// the empty mapping as an identity mapping. See [IDMappingSupported].
type ValidatedIdentityMapping struct {
	uidMaps []IDMap
	gidMaps []IDMap
}

// ValidateIdentityMapping validates m and returns an immutable snapshot of
// it. The UID and GID mappings are validated independently; if either is
// invalid, an [*IDMapError] (or a joined error containing one error per
// invalid mapping) is returned and no snapshot is created.
//
// The returned snapshot does not share memory with m; later modifications of
// m's mapping slices have no effect on the snapshot.
func ValidateIdentityMapping(m IdentityMapping) (ValidatedIdentityMapping, error) {
	if err := m.Validate(); err != nil {
		return ValidatedIdentityMapping{}, err
	}
	return ValidatedIdentityMapping{
		uidMaps: cloneIDMaps(m.UIDMaps),
		gidMaps: cloneIDMaps(m.GIDMaps),
	}, nil
}

// cloneIDMaps returns a copy of maps, normalizing empty slices to nil.
func cloneIDMaps(maps []IDMap) []IDMap {
	if len(maps) == 0 {
		return nil
	}
	out := make([]IDMap, len(maps))
	copy(out, maps)
	return out
}

// Validate re-runs validation on the snapshot. It returns nil for a
// consistent snapshot and can be used to verify a snapshot repeatedly.
func (v ValidatedIdentityMapping) Validate() error {
	return IdentityMapping{UIDMaps: v.uidMaps, GIDMaps: v.gidMaps}.Validate()
}

// UIDMaps returns a copy of the snapshot's UID mapping.
func (v ValidatedIdentityMapping) UIDMaps() []IDMap {
	return cloneIDMaps(v.uidMaps)
}

// GIDMaps returns a copy of the snapshot's GID mapping.
func (v ValidatedIdentityMapping) GIDMaps() []IDMap {
	return cloneIDMaps(v.gidMaps)
}

// Empty reports whether the snapshot contains no mapping entries.
func (v ValidatedIdentityMapping) Empty() bool {
	return len(v.uidMaps) == 0 && len(v.gidMaps) == 0
}

// ToHost returns the host UID and GID for the container uid and gid, using
// this snapshot only.
//
// An empty snapshot is treated as an identity mapping on platforms that
// support ID mappings; on other platforms [ErrIDMapUnsupported] is returned
// instead. An ID that is not covered by a non-empty mapping results in an
// error wrapping [ErrIDMapUnmapped].
func (v ValidatedIdentityMapping) ToHost(uid, gid int) (int, int, error) {
	ruid, err := v.toHost(uid, v.uidMaps, IDMapUID)
	if err != nil {
		return -1, -1, err
	}
	rgid, err := v.toHost(gid, v.gidMaps, IDMapGID)
	if err != nil {
		return -1, -1, err
	}
	return ruid, rgid, nil
}

// ToContainer returns the container UID and GID for the host uid and gid,
// using this snapshot only.
//
// An empty snapshot is treated as an identity mapping on platforms that
// support ID mappings; on other platforms [ErrIDMapUnsupported] is returned
// instead. An ID that is not covered by a non-empty mapping results in an
// error wrapping [ErrIDMapUnmapped].
func (v ValidatedIdentityMapping) ToContainer(uid, gid int) (int, int, error) {
	ruid, err := v.toContainer(uid, v.uidMaps, IDMapUID)
	if err != nil {
		return -1, -1, err
	}
	rgid, err := v.toContainer(gid, v.gidMaps, IDMapGID)
	if err != nil {
		return -1, -1, err
	}
	return ruid, rgid, nil
}

// RootPair returns the host UID and GID that container root is mapped to.
// It returns an error if either mapping does not cover container ID 0.
func (v ValidatedIdentityMapping) RootPair() (int, int, error) {
	uid, err := v.toHost(0, v.uidMaps, IDMapUID)
	if err != nil {
		return -1, -1, err
	}
	gid, err := v.toHost(0, v.gidMaps, IDMapGID)
	if err != nil {
		return -1, -1, err
	}
	return uid, gid, nil
}

func (v ValidatedIdentityMapping) toHost(id int, maps []IDMap, kind IDMapKind) (int, error) {
	if len(maps) == 0 {
		if !idMappingSupported {
			return -1, ErrIDMapUnsupported
		}
		return id, nil
	}
	if id < 0 {
		return -1, fmt.Errorf("%w: %s container ID %d cannot be mapped to a host ID", ErrIDMapUnmapped, kind, id)
	}

	hostID, ok := lookupHost(int64(id), maps)
	if !ok {
		return -1, fmt.Errorf("%w: %s container ID %d cannot be mapped to a host ID", ErrIDMapUnmapped, kind, id)
	}
	if hostID > math.MaxInt {
		return -1, fmt.Errorf("%w: %s container ID %d maps to host ID %d, which does not fit in int", ErrIDMapOutOfRange, kind, id, hostID)
	}
	return int(hostID), nil
}

func (v ValidatedIdentityMapping) toContainer(id int, maps []IDMap, kind IDMapKind) (int, error) {
	if len(maps) == 0 {
		if !idMappingSupported {
			return -1, ErrIDMapUnsupported
		}
		return id, nil
	}
	if id < 0 {
		return -1, fmt.Errorf("%w: %s host ID %d cannot be mapped to a container ID", ErrIDMapUnmapped, kind, id)
	}

	contID, ok := lookupContainer(int64(id), maps)
	if !ok {
		return -1, fmt.Errorf("%w: %s host ID %d cannot be mapped to a container ID", ErrIDMapUnmapped, kind, id)
	}
	if contID > math.MaxInt {
		return -1, fmt.Errorf("%w: %s host ID %d maps to container ID %d, which does not fit in int", ErrIDMapOutOfRange, kind, id, contID)
	}
	return int(contID), nil
}

// IdentityMappingStore holds a validated [ValidatedIdentityMapping] and
// publishes new snapshots atomically. Readers always observe a complete
// snapshot: concurrent updates never mix the UID mapping of one update with
// the GID mapping of another.
//
// The zero value is a store with an empty mapping.
type IdentityMappingStore struct {
	current atomic.Pointer[ValidatedIdentityMapping]
}

// NewIdentityMappingStore validates m and returns a store holding that
// snapshot. An invalid mapping is rejected with an [*IDMapError].
func NewIdentityMappingStore(m IdentityMapping) (*IdentityMappingStore, error) {
	v, err := ValidateIdentityMapping(m)
	if err != nil {
		return nil, err
	}
	s := &IdentityMappingStore{}
	s.current.Store(&v)
	return s, nil
}

// Update validates m and atomically publishes it as the new snapshot. The
// update is transactional: if validation fails, the previously published
// snapshot remains in effect and the error is returned.
func (s *IdentityMappingStore) Update(m IdentityMapping) error {
	v, err := ValidateIdentityMapping(m)
	if err != nil {
		return err
	}
	s.current.Store(&v)
	return nil
}

// Snapshot returns the currently published snapshot. The returned value is
// immutable and remains valid even if the store is updated concurrently.
func (s *IdentityMappingStore) Snapshot() ValidatedIdentityMapping {
	if p := s.current.Load(); p != nil {
		return *p
	}
	return ValidatedIdentityMapping{}
}

// ToHost returns the host UID and GID for the container uid and gid using a
// single snapshot.
func (s *IdentityMappingStore) ToHost(uid, gid int) (int, int, error) {
	return s.Snapshot().ToHost(uid, gid)
}

// ToContainer returns the container UID and GID for the host uid and gid
// using a single snapshot.
func (s *IdentityMappingStore) ToContainer(uid, gid int) (int, int, error) {
	return s.Snapshot().ToContainer(uid, gid)
}

// IDMappingSupported reports whether the current platform supports Linux
// user-namespace ID mappings. It is true on Linux and false elsewhere.
//
// On unsupported platforms, operations that depend on ID mappings return
// [ErrIDMapUnsupported] instead of falling back to an identity mapping.
func IDMappingSupported() bool {
	return idMappingSupported
}
