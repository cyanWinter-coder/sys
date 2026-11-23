package user

import (
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func mustValidate(t *testing.T, m IdentityMapping) ValidatedIdentityMapping {
	t.Helper()
	v, err := ValidateIdentityMapping(m)
	if err != nil {
		t.Fatalf("ValidateIdentityMapping() error: %v", err)
	}
	return v
}

func TestValidateIDMaps(t *testing.T) {
	maxCount := int64(1<<32 - 1)

	for _, tc := range []struct {
		name   string
		maps   []IDMap
		want   error  // nil means valid
		detail string // substring expected in the error message
		idx    int    // expected IDMapError.Index
		conf   int    // expected IDMapError.ConflictIndex
	}{
		{
			name: "empty",
			maps: nil,
		},
		{
			name: "single standard range",
			maps: []IDMap{{ID: 0, ParentID: 100000, Count: 65536}},
		},
		{
			name: "multiple ordered non-overlapping ranges",
			maps: []IDMap{
				{ID: 0, ParentID: 100000, Count: 65536},
				{ID: 65536, ParentID: 200000, Count: 65536},
			},
		},
		{
			name: "host ranges may be unordered",
			maps: []IDMap{
				{ID: 0, ParentID: 200000, Count: 65536},
				{ID: 65536, ParentID: 100000, Count: 65536},
			},
		},
		{
			name: "largest valid range ends at max ID",
			maps: []IDMap{{ID: 0, ParentID: 0, Count: maxCount}},
		},
		{
			name: "range ending exactly at max valid ID",
			maps: []IDMap{{ID: 0, ParentID: 0, Count: maxCount}},
		},
		{
			name:   "zero count",
			maps:   []IDMap{{ID: 0, ParentID: 0, Count: 0}},
			want:   ErrIDMapInvalidCount,
			detail: "count must be positive",
			idx:    0,
			conf:   -1,
		},
		{
			name:   "negative count",
			maps:   []IDMap{{ID: 0, ParentID: 0, Count: -1}},
			want:   ErrIDMapInvalidCount,
			detail: "count must be positive",
			idx:    0,
			conf:   -1,
		},
		{
			name:   "negative container ID",
			maps:   []IDMap{{ID: -1, ParentID: 100000, Count: 1}},
			want:   ErrIDMapOutOfRange,
			detail: "IDs must not be negative",
			idx:    0,
			conf:   -1,
		},
		{
			name:   "negative host ID",
			maps:   []IDMap{{ID: 0, ParentID: -1, Count: 1}},
			want:   ErrIDMapOutOfRange,
			detail: "IDs must not be negative",
			idx:    0,
			conf:   -1,
		},
		{
			name:   "container ID is max",
			maps:   []IDMap{{ID: maxCount, ParentID: 0, Count: 1}},
			want:   ErrIDMapOutOfRange,
			detail: "does not fit in the unsigned 32-bit ID space",
			idx:    0,
			conf:   -1,
		},
		{
			name:   "host ID is max",
			maps:   []IDMap{{ID: 0, ParentID: maxCount, Count: 1}},
			want:   ErrIDMapOutOfRange,
			detail: "does not fit in the unsigned 32-bit ID space",
			idx:    0,
			conf:   -1,
		},
		{
			name:   "count exceeds 32-bit container space",
			maps:   []IDMap{{ID: 1, ParentID: 0, Count: maxCount}},
			want:   ErrIDMapOutOfRange,
			detail: "does not fit in the unsigned 32-bit ID space",
			idx:    0,
			conf:   -1,
		},
		{
			name:   "count exceeds 32-bit host space",
			maps:   []IDMap{{ID: 0, ParentID: 1, Count: maxCount}},
			want:   ErrIDMapOutOfRange,
			detail: "does not fit in the unsigned 32-bit ID space",
			idx:    0,
			conf:   -1,
		},
		{
			name:   "count overflows int64 arithmetic",
			maps:   []IDMap{{ID: math.MaxInt64, ParentID: 0, Count: math.MaxInt64}},
			want:   ErrIDMapOutOfRange,
			detail: "does not fit in the unsigned 32-bit ID space",
			idx:    0,
			conf:   -1,
		},
		{
			name: "descending container IDs",
			maps: []IDMap{
				{ID: 65536, ParentID: 100000, Count: 65536},
				{ID: 0, ParentID: 200000, Count: 65536},
			},
			want:   ErrIDMapNotSorted,
			detail: "is lower than entry 0",
			idx:    1,
			conf:   0,
		},
		{
			name: "duplicate container start",
			maps: []IDMap{
				{ID: 0, ParentID: 100000, Count: 65536},
				{ID: 0, ParentID: 200000, Count: 65536},
			},
			want:   ErrIDMapOverlap,
			detail: "container range overlaps entry 0",
			idx:    1,
			conf:   0,
		},
		{
			name: "adjacent container ranges are fine",
			maps: []IDMap{
				{ID: 0, ParentID: 100000, Count: 100},
				{ID: 100, ParentID: 200000, Count: 100},
			},
		},
		{
			name: "partially overlapping container ranges",
			maps: []IDMap{
				{ID: 0, ParentID: 100000, Count: 100},
				{ID: 50, ParentID: 200000, Count: 100},
			},
			want:   ErrIDMapOverlap,
			detail: "container range overlaps entry 0",
			idx:    1,
			conf:   0,
		},
		{
			name: "contained container range",
			maps: []IDMap{
				{ID: 0, ParentID: 100000, Count: 100},
				{ID: 10, ParentID: 200000, Count: 10},
			},
			want:   ErrIDMapOverlap,
			detail: "container range overlaps entry 0",
			idx:    1,
			conf:   0,
		},
		{
			// A long first range swallows the start of the third entry even
			// though the second entry does not overlap it. Only comparing
			// against the immediately preceding entry would miss this.
			name: "later range overlaps a non-adjacent earlier range",
			maps: []IDMap{
				{ID: 0, ParentID: 300000, Count: 100},
				{ID: 100, ParentID: 400000, Count: 10},
				{ID: 50, ParentID: 500000, Count: 10},
			},
			want:   ErrIDMapNotSorted,
			detail: "is lower than entry 1",
			idx:    2,
			conf:   1,
		},
		{
			name: "overlapping host ranges",
			maps: []IDMap{
				{ID: 0, ParentID: 100000, Count: 100},
				{ID: 100, ParentID: 100050, Count: 100},
			},
			want:   ErrIDMapOverlap,
			detail: "host range overlaps entry 0",
			idx:    1,
			conf:   0,
		},
		{
			name: "identical host ranges",
			maps: []IDMap{
				{ID: 0, ParentID: 100000, Count: 100},
				{ID: 100, ParentID: 100000, Count: 100},
			},
			want:   ErrIDMapOverlap,
			detail: "host range overlaps entry 0",
			idx:    1,
			conf:   0,
		},
		{
			name: "adjacent host ranges are fine",
			maps: []IDMap{
				{ID: 0, ParentID: 100000, Count: 100},
				{ID: 100, ParentID: 100100, Count: 100},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Copy the input so the test can verify it was not modified.
			orig := cloneIDMaps(tc.maps)
			err := ValidateIDMaps(IDMapUID, tc.maps)

			if tc.want == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error %v, got nil", tc.want)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("expected error %v, got %v", tc.want, err)
			}
			var idErr *IDMapError
			if !errors.As(err, &idErr) {
				t.Fatalf("expected *IDMapError, got %T", err)
			}
			if idErr.Kind != IDMapUID {
				t.Errorf("Kind: got %v, want %v", idErr.Kind, IDMapUID)
			}
			if idErr.Index != tc.idx {
				t.Errorf("Index: got %d, want %d", idErr.Index, tc.idx)
			}
			if idErr.ConflictIndex != tc.conf {
				t.Errorf("ConflictIndex: got %d, want %d", idErr.ConflictIndex, tc.conf)
			}
			if idErr.Entry != tc.maps[tc.idx] {
				t.Errorf("Entry: got %+v, want %+v", idErr.Entry, tc.maps[tc.idx])
			}
			if tc.conf >= 0 && idErr.Conflict != tc.maps[tc.conf] {
				t.Errorf("Conflict: got %+v, want %+v", idErr.Conflict, tc.maps[tc.conf])
			}
			if !strings.Contains(err.Error(), tc.detail) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.detail)
			}
			if !strings.Contains(err.Error(), "entry "+itoa(tc.idx)) {
				t.Errorf("error %q does not identify entry %d", err.Error(), tc.idx)
			}
			if !errors.Is(idErr.Err, tc.want) {
				t.Errorf("underlying error: got %v, want %v", idErr.Err, tc.want)
			}

			if got := cloneIDMaps(tc.maps); !idMapsEqual(got, orig) {
				t.Errorf("ValidateIDMaps modified its input: got %+v, want %+v", got, orig)
			}
		})
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

func idMapsEqual(a, b []IDMap) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// joinedErrors flattens an error produced by [errors.Join] into its elements.
func joinedErrors(err error) []error {
	if err == nil {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		return joined.Unwrap()
	}
	return []error{err}
}

func TestValidateIDMapsConflictPointsAtBothEntries(t *testing.T) {
	maps := []IDMap{
		{ID: 0, ParentID: 100000, Count: 1000},
		{ID: 500, ParentID: 200000, Count: 10},
	}
	err := ValidateIDMaps(IDMapGID, maps)
	var idErr *IDMapError
	if !errors.As(err, &idErr) {
		t.Fatalf("expected *IDMapError, got %v", err)
	}
	if idErr.Kind != IDMapGID {
		t.Errorf("Kind: got %v, want GID", idErr.Kind)
	}
	if idErr.Entry != maps[1] {
		t.Errorf("Entry: got %+v, want %+v", idErr.Entry, maps[1])
	}
	if idErr.Conflict != maps[0] {
		t.Errorf("Conflict: got %+v, want %+v", idErr.Conflict, maps[0])
	}
	msg := err.Error()
	if !strings.Contains(msg, "container [500, 510)") {
		t.Errorf("error %q does not describe the offending range", msg)
	}
	if !strings.Contains(msg, "container [0, 1000)") {
		t.Errorf("error %q does not describe the conflicting range", msg)
	}
}

func TestIdentityMappingValidateIndependent(t *testing.T) {
	validUID := []IDMap{{ID: 0, ParentID: 100000, Count: 65536}}
	badGID := []IDMap{{ID: 100, ParentID: 200000, Count: 10}, {ID: 0, ParentID: 300000, Count: 10}}

	err := (IdentityMapping{UIDMaps: validUID, GIDMaps: badGID}).Validate()
	if !errors.Is(err, ErrIDMapNotSorted) {
		t.Fatalf("expected ErrIDMapNotSorted, got %v", err)
	}
	var idErr *IDMapError
	if !errors.As(err, &idErr) {
		t.Fatalf("expected *IDMapError, got %T", err)
	}
	if idErr.Kind != IDMapGID {
		t.Errorf("Kind: got %v, want GID", idErr.Kind)
	}

	// Both invalid: both must be reported.
	badUID := []IDMap{{ID: 0, ParentID: 0, Count: 0}}
	err = (IdentityMapping{UIDMaps: badUID, GIDMaps: badGID}).Validate()
	if !errors.Is(err, ErrIDMapInvalidCount) {
		t.Errorf("expected joined error to include ErrIDMapInvalidCount, got %v", err)
	}
	if !errors.Is(err, ErrIDMapNotSorted) {
		t.Errorf("expected joined error to include ErrIDMapNotSorted, got %v", err)
	}
	// Both errors must be distinguishable and attributed to the right kind.
	kinds := map[IDMapKind]int{}
	for _, err := range joinedErrors(err) {
		var one *IDMapError
		if errors.As(err, &one) {
			kinds[one.Kind]++
		}
	}
	if kinds[IDMapUID] != 1 || kinds[IDMapGID] != 1 {
		t.Fatalf("expected one UID and one GID error, got %v", kinds)
	}

	// Valid mapping: no error.
	if err := (IdentityMapping{UIDMaps: validUID, GIDMaps: validUID}).Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateIdentityMappingSnapshotIsIndependent(t *testing.T) {
	uid := []IDMap{{ID: 0, ParentID: 100000, Count: 65536}}
	gid := []IDMap{{ID: 0, ParentID: 200000, Count: 65536}}

	v, err := ValidateIdentityMapping(IdentityMapping{UIDMaps: uid, GIDMaps: gid})
	if err != nil {
		t.Fatal(err)
	}

	// Mutating the source slices must not affect the snapshot.
	uid[0].ParentID = 42
	gid[0].Count = 1

	hostUID, hostGID, err := v.ToHost(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if hostUID != 100000 || hostGID != 200000 {
		t.Fatalf("snapshot changed after source mutation: got %d:%d, want 100000:200000", hostUID, hostGID)
	}

	// Mutating the returned copies must not affect the snapshot either.
	gotUID := v.UIDMaps()
	gotGID := v.GIDMaps()
	gotUID[0].ParentID = 7
	gotGID[0].ParentID = 7
	if u, g := v.UIDMaps()[0].ParentID, v.GIDMaps()[0].ParentID; u != 100000 || g != 200000 {
		t.Fatalf("accessor exposed internal state: got %d:%d", u, g)
	}

	// The snapshot can be validated repeatedly and identically.
	for range 3 {
		if err := v.Validate(); err != nil {
			t.Fatalf("Validate() on snapshot: %v", err)
		}
	}
}

func TestValidateIdentityMappingRejectsInvalid(t *testing.T) {
	m := IdentityMapping{
		UIDMaps: []IDMap{{ID: 0, ParentID: 0, Count: 0}},
	}
	v, err := ValidateIdentityMapping(m)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrIDMapInvalidCount) {
		t.Errorf("expected ErrIDMapInvalidCount, got %v", err)
	}
	if !v.Empty() {
		t.Error("no snapshot should be returned on error")
	}
}

func TestValidatedToHostToContainerRoundTrip(t *testing.T) {
	v := mustValidate(t, IdentityMapping{
		UIDMaps: []IDMap{
			{ID: 0, ParentID: 100000, Count: 65536},
			{ID: 65536, ParentID: 300000, Count: 10},
		},
		GIDMaps: []IDMap{
			{ID: 0, ParentID: 200000, Count: 65536},
		},
	})

	for _, tc := range []struct {
		contUID, contGID int
		hostUID, hostGID int
	}{
		{0, 0, 100000, 200000},
		{1000, 1000, 101000, 201000},
		{65535, 65535, 165535, 265535},
		{65536, 65536 - 1, 300000, 200000 + 65535},
	} {
		hostUID, hostGID, err := v.ToHost(tc.contUID, tc.contGID)
		if err != nil {
			t.Fatalf("ToHost(%d, %d): %v", tc.contUID, tc.contGID, err)
		}
		if hostUID != tc.hostUID || hostGID != tc.hostGID {
			t.Errorf("ToHost(%d, %d) = %d:%d, want %d:%d", tc.contUID, tc.contGID, hostUID, hostGID, tc.hostUID, tc.hostGID)
		}

		backUID, backGID, err := v.ToContainer(hostUID, hostGID)
		if err != nil {
			t.Fatalf("ToContainer(%d, %d): %v", hostUID, hostGID, err)
		}
		if backUID != tc.contUID || backGID != tc.contGID {
			t.Errorf("ToContainer(%d, %d) = %d:%d, want %d:%d", hostUID, hostGID, backUID, backGID, tc.contUID, tc.contGID)
		}
	}
}

func TestValidatedToHostUnmapped(t *testing.T) {
	v := mustValidate(t, IdentityMapping{
		UIDMaps: []IDMap{{ID: 0, ParentID: 100000, Count: 10}},
		GIDMaps: []IDMap{{ID: 0, ParentID: 100000, Count: 10}},
	})

	for _, id := range []int{10, 1000, -1} {
		_, _, err := v.ToHost(id, 0)
		if !errors.Is(err, ErrIDMapUnmapped) {
			t.Errorf("ToHost(%d, 0): expected ErrIDMapUnmapped, got %v", id, err)
		}
		_, _, err = v.ToHost(0, id)
		if !errors.Is(err, ErrIDMapUnmapped) {
			t.Errorf("ToHost(0, %d): expected ErrIDMapUnmapped, got %v", id, err)
		}
		_, _, err = v.ToContainer(id+100000, 100000)
		if !errors.Is(err, ErrIDMapUnmapped) {
			t.Errorf("ToContainer(%d, 100000): expected ErrIDMapUnmapped, got %v", id+100000, err)
		}
	}
}

func TestValidatedRootPair(t *testing.T) {
	v := mustValidate(t, IdentityMapping{
		UIDMaps: []IDMap{{ID: 0, ParentID: 100000, Count: 65536}},
		GIDMaps: []IDMap{{ID: 0, ParentID: 200000, Count: 65536}},
	})
	uid, gid, err := v.RootPair()
	if err != nil {
		t.Fatal(err)
	}
	if uid != 100000 || gid != 200000 {
		t.Fatalf("RootPair() = %d:%d, want 100000:200000", uid, gid)
	}

	// A mapping that does not cover container 0 has no root pair.
	v = mustValidate(t, IdentityMapping{
		UIDMaps: []IDMap{{ID: 1, ParentID: 100000, Count: 10}},
		GIDMaps: []IDMap{{ID: 1, ParentID: 100000, Count: 10}},
	})
	if _, _, err := v.RootPair(); !errors.Is(err, ErrIDMapUnmapped) {
		t.Fatalf("expected ErrIDMapUnmapped, got %v", err)
	}
}

func TestValidatedEmptyMapping(t *testing.T) {
	v := mustValidate(t, IdentityMapping{})
	if !v.Empty() {
		t.Fatal("expected snapshot to be empty")
	}

	if !idMappingSupported {
		if _, _, err := v.ToHost(1, 1); !errors.Is(err, ErrIDMapUnsupported) {
			t.Fatalf("expected ErrIDMapUnsupported, got %v", err)
		}
		return
	}

	uid, gid, err := v.ToHost(1234, 5678)
	if err != nil {
		t.Fatal(err)
	}
	if uid != 1234 || gid != 5678 {
		t.Fatalf("empty mapping should be identity: got %d:%d", uid, gid)
	}
	uid, gid, err = v.ToContainer(1234, 5678)
	if err != nil {
		t.Fatal(err)
	}
	if uid != 1234 || gid != 5678 {
		t.Fatalf("empty mapping should be identity: got %d:%d", uid, gid)
	}
}

func TestValidatedEmptyMappingUnsupportedPlatform(t *testing.T) {
	orig := idMappingSupported
	idMappingSupported = false
	t.Cleanup(func() { idMappingSupported = orig })

	v := mustValidate(t, IdentityMapping{})

	// Even with a non-empty UID mapping but an empty GID mapping, an empty
	// mapping on an unsupported platform must not silently fall back to
	// identity.
	if _, _, err := v.ToHost(0, 0); !errors.Is(err, ErrIDMapUnsupported) {
		t.Fatalf("ToHost: expected ErrIDMapUnsupported, got %v", err)
	}
	if _, _, err := v.ToContainer(0, 0); !errors.Is(err, ErrIDMapUnsupported) {
		t.Fatalf("ToContainer: expected ErrIDMapUnsupported, got %v", err)
	}
	if _, _, err := v.RootPair(); !errors.Is(err, ErrIDMapUnsupported) {
		t.Fatalf("RootPair: expected ErrIDMapUnsupported, got %v", err)
	}

	// A non-empty mapping still converts: only the empty fallback is gated.
	v = mustValidate(t, IdentityMapping{
		UIDMaps: []IDMap{{ID: 0, ParentID: 100000, Count: 10}},
		GIDMaps: []IDMap{{ID: 0, ParentID: 100000, Count: 10}},
	})
	if _, _, err := v.ToHost(0, 0); err != nil {
		t.Fatalf("ToHost with non-empty mapping: %v", err)
	}
}

func TestIDMappingSupported(t *testing.T) {
	// The package must report support consistently with the gate it uses.
	if got, want := IDMappingSupported(), idMappingSupported; got != want {
		t.Fatalf("IDMappingSupported() = %v, want %v", got, want)
	}
}

func TestIdentityMappingStoreUpdateIsTransactional(t *testing.T) {
	v1 := IdentityMapping{
		UIDMaps: []IDMap{{ID: 0, ParentID: 100000, Count: 65536}},
		GIDMaps: []IDMap{{ID: 0, ParentID: 200000, Count: 65536}},
	}
	store, err := NewIdentityMappingStore(v1)
	if err != nil {
		t.Fatal(err)
	}

	// An invalid update must be rejected and must not change anything.
	bad := IdentityMapping{
		UIDMaps: []IDMap{{ID: 0, ParentID: 300000, Count: 65536}},
		GIDMaps: []IDMap{{ID: 0, ParentID: 1, Count: 0}},
	}
	if err := store.Update(bad); !errors.Is(err, ErrIDMapInvalidCount) {
		t.Fatalf("expected ErrIDMapInvalidCount, got %v", err)
	}
	uid, gid, err := store.ToHost(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if uid != 100000 || gid != 200000 {
		t.Fatalf("failed update changed the snapshot: got %d:%d", uid, gid)
	}

	// A valid update replaces both mappings atomically.
	good := IdentityMapping{
		UIDMaps: []IDMap{{ID: 0, ParentID: 400000, Count: 65536}},
		GIDMaps: []IDMap{{ID: 0, ParentID: 500000, Count: 65536}},
	}
	if err := store.Update(good); err != nil {
		t.Fatal(err)
	}
	uid, gid, err = store.ToHost(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if uid != 400000 || gid != 500000 {
		t.Fatalf("snapshot not updated: got %d:%d", uid, gid)
	}

	// Mutating the source after Update must not affect the store.
	good.UIDMaps[0].ParentID = 1
	good.GIDMaps[0].ParentID = 1
	uid, gid, err = store.ToHost(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if uid != 400000 || gid != 500000 {
		t.Fatalf("source mutation changed the snapshot: got %d:%d", uid, gid)
	}
}

func TestNewIdentityMappingStoreRejectsInvalid(t *testing.T) {
	_, err := NewIdentityMappingStore(IdentityMapping{
		UIDMaps: []IDMap{{ID: 0, ParentID: 0, Count: 0}},
	})
	if !errors.Is(err, ErrIDMapInvalidCount) {
		t.Fatalf("expected ErrIDMapInvalidCount, got %v", err)
	}
}

func TestIdentityMappingStoreZeroValue(t *testing.T) {
	var store IdentityMappingStore
	// The zero-value store behaves like a store with an empty snapshot.
	if !store.Snapshot().Empty() {
		t.Fatal("zero-value store snapshot should be empty")
	}
	if !idMappingSupported {
		t.Skip("identity fallback is platform-dependent")
	}
	uid, gid, err := store.ToHost(2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if uid != 2 || gid != 3 {
		t.Fatalf("zero-value store: got %d:%d, want 2:3", uid, gid)
	}
}

// TestIdentityMappingStoreConcurrentUpdates verifies that conversions never
// observe a mixed mapping: every successful ToHost must correspond to a single
// validated snapshot, never a combination of two updates.
func TestIdentityMappingStoreConcurrentUpdates(t *testing.T) {
	// Each snapshot maps container 0 to the same base for UID and GID. A
	// mixed snapshot would produce a UID from one update and a GID from
	// another, which the check below rejects.
	store, err := NewIdentityMappingStore(IdentityMapping{
		UIDMaps: []IDMap{{ID: 0, ParentID: 100000, Count: 10}},
		GIDMaps: []IDMap{{ID: 0, ParentID: 100000, Count: 10}},
	})
	if err != nil {
		t.Fatal(err)
	}

	const iterations = 2000
	var wg sync.WaitGroup
	var failures atomic.Int64

	// Writers publish a stream of consistently paired snapshots.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range iterations {
			base := int64(100000 + i*10)
			m := IdentityMapping{
				UIDMaps: []IDMap{{ID: 0, ParentID: base, Count: 10}},
				GIDMaps: []IDMap{{ID: 0, ParentID: base, Count: 10}},
			}
			if err := store.Update(m); err != nil {
				t.Errorf("Update: %v", err)
				return
			}
		}
	}()

	// Readers must always see a UID and GID derived from the same base.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				uid, gid, err := store.ToHost(0, 0)
				if err != nil {
					// ErrIDMapUnmapped cannot happen: every published
					// snapshot covers container 0, and the initial one does
					// too.
					t.Errorf("ToHost: %v", err)
					return
				}
				if uid != gid || uid < 100000 {
					failures.Add(1)
				}
			}
		}()
	}

	wg.Wait()
	if n := failures.Load(); n != 0 {
		t.Fatalf("observed %d mixed or invalid snapshot conversions", n)
	}
}

func TestValidateIDMapsRepeatable(t *testing.T) {
	maps := []IDMap{
		{ID: 0, ParentID: 100000, Count: 65536},
		{ID: 100, ParentID: 100050, Count: 10}, // overlapping host range
	}
	var first string
	for range 5 {
		err := ValidateIDMaps(IDMapUID, maps)
		if err == nil {
			t.Fatal("expected an error")
		}
		if first == "" {
			first = err.Error()
			continue
		}
		if got := err.Error(); got != first {
			t.Fatalf("validation result changed between runs:\nfirst:  %s\nlater:  %s", first, got)
		}
	}
}

// TestLegacyIdentityMappingRejectsInvalid checks that the compatibility API
// also refuses malformed mappings instead of returning an order-dependent
// result.
func TestLegacyIdentityMappingRejectsInvalid(t *testing.T) {
	m := IdentityMapping{
		UIDMaps: []IDMap{
			{ID: 0, ParentID: 100000, Count: 100},
			{ID: 50, ParentID: 200000, Count: 100},
		},
		GIDMaps: []IDMap{{ID: 0, ParentID: 100000, Count: 100}},
	}
	if _, _, err := m.ToHost(0, 0); !errors.Is(err, ErrIDMapOverlap) {
		t.Fatalf("ToHost: expected ErrIDMapOverlap, got %v", err)
	}
	if _, _, err := m.ToContainer(100000, 100000); !errors.Is(err, ErrIDMapOverlap) {
		t.Fatalf("ToContainer: expected ErrIDMapOverlap, got %v", err)
	}
	if uid, gid := m.RootPair(); uid != -1 || gid != -1 {
		t.Fatalf("RootPair: expected -1:-1 for an invalid mapping, got %d:%d", uid, gid)
	}
}

func TestLegacyIdentityMappingAcceptsValid(t *testing.T) {
	m := IdentityMapping{
		UIDMaps: []IDMap{{ID: 0, ParentID: 100000, Count: 65536}},
		GIDMaps: []IDMap{{ID: 0, ParentID: 200000, Count: 65536}},
	}
	uid, gid, err := m.ToHost(1000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if uid != 101000 || gid != 201000 {
		t.Fatalf("ToHost = %d:%d, want 101000:201000", uid, gid)
	}
}

func TestNormalizeIDMaps(t *testing.T) {
	// A uid_map with at most five extents is returned by the kernel in write
	// order, not sorted order, so this is a realistic input.
	maps := []IDMap{
		{ID: 65536, ParentID: 200000, Count: 65536},
		{ID: 0, ParentID: 100000, Count: 65536},
	}
	norm := NormalizeIDMaps(maps)
	if want := []IDMap{
		{ID: 0, ParentID: 100000, Count: 65536},
		{ID: 65536, ParentID: 200000, Count: 65536},
	}; !idMapsEqual(norm, want) {
		t.Fatalf("NormalizeIDMaps() = %+v, want %+v", norm, want)
	}
	// The input must not be modified.
	if maps[0].ID != 65536 || maps[1].ID != 0 {
		t.Fatalf("NormalizeIDMaps modified its input: %+v", maps)
	}
	// The original order is rejected by validation, the normalized one is
	// accepted, and conversions agree with the entry that owns the ID.
	if err := ValidateIDMaps(IDMapUID, maps); !errors.Is(err, ErrIDMapNotSorted) {
		t.Fatalf("expected ErrIDMapNotSorted for unsorted input, got %v", err)
	}
	if err := ValidateIDMaps(IDMapUID, norm); err != nil {
		t.Fatalf("normalized mapping should validate: %v", err)
	}
	v := mustValidate(t, IdentityMapping{UIDMaps: norm, GIDMaps: norm})
	uid, _, err := v.ToHost(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if uid != 100000 {
		t.Fatalf("ToHost(0, 0) uid = %d, want 100000", uid)
	}
	if got := NormalizeIDMaps(nil); got != nil {
		t.Fatalf("NormalizeIDMaps(nil) = %+v, want nil", got)
	}
}

func TestNormalizeIDMapsStableForDuplicateContainerIDs(t *testing.T) {
	maps := []IDMap{
		{ID: 0, ParentID: 300000, Count: 10},
		{ID: 0, ParentID: 100000, Count: 10},
	}
	norm := NormalizeIDMaps(maps)
	if norm[0].ParentID != 300000 || norm[1].ParentID != 100000 {
		t.Fatalf("duplicate container IDs must keep their relative order, got %+v", norm)
	}
	// Validation then reports the second entry as the conflict, deterministically.
	err := ValidateIDMaps(IDMapGID, norm)
	var idErr *IDMapError
	if !errors.As(err, &idErr) || idErr.Index != 1 || idErr.ConflictIndex != 0 {
		t.Fatalf("expected conflict between entries 1 and 0, got %v", err)
	}
}

func TestValidateIDMapsEveryBadIndexIsReported(t *testing.T) {
	// For every position, an entry with a zero count must be reported at that
	// position, not only at the head or tail.
	base := []IDMap{
		{ID: 0, ParentID: 100000, Count: 10},
		{ID: 10, ParentID: 200000, Count: 10},
		{ID: 20, ParentID: 300000, Count: 10},
	}
	for i := range base {
		maps := cloneIDMaps(base)
		maps[i].Count = 0
		err := ValidateIDMaps(IDMapUID, maps)
		var idErr *IDMapError
		if !errors.As(err, &idErr) {
			t.Fatalf("index %d: expected *IDMapError, got %v", i, err)
		}
		if idErr.Index != i {
			t.Errorf("index %d: reported entry %d", i, idErr.Index)
		}
		if !errors.Is(err, ErrIDMapInvalidCount) {
			t.Errorf("index %d: expected ErrIDMapInvalidCount, got %v", i, err)
		}
	}
}
