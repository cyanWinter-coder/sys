//go:build linux

package mountinfo

import (
	"bytes"
	"reflect"
	"strconv"
	"sync"
	"testing"
)

// TestParseInfoLineUnknownOptionalFields verifies that optional fields
// which are not part of the well-known set (shared, master, ...) are
// kept verbatim in Info.Optional instead of causing a parse error.
func TestParseInfoLineUnknownOptionalFields(t *testing.T) {
	cases := []struct {
		name        string
		entry       string
		expOptional string
	}{
		{
			name:        "single unknown tag",
			entry:       `251 15 0:3573 / /mnt/point rw,relatime future:tag=42 - aufs none rw`,
			expOptional: "future:tag=42",
		},
		{
			name:        "known and unknown tags mixed",
			entry:       `251 15 0:3573 / /mnt/point rw,relatime shared:123 master:7 future:tag what:ever - aufs none rw`,
			expOptional: "shared:123 master:7 future:tag what:ever",
		},
		{
			name:        "tag without value",
			entry:       `251 15 0:3573 / /mnt/point rw,relatime unbindable newlyadded - aufs none rw`,
			expOptional: "unbindable newlyadded",
		},
		{
			name:        "unknown tag with escaped value is left raw",
			entry:       `251 15 0:3573 / /mnt/point rw,relatime weird:a\040b - aufs none rw`,
			expOptional: `weird:a\040b`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			infos, err := GetMountsFromReader(bytes.NewBufferString(tc.entry), nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(infos) != 1 {
				t.Fatalf("expected 1 entry, got %d", len(infos))
			}
			if infos[0].Optional != tc.expOptional {
				t.Errorf("expected Optional %q, got %q", tc.expOptional, infos[0].Optional)
			}
		})
	}
}

// TestParseInfoLineMandatoryFields verifies that a missing or malformed
// mandatory field always produces an error, while well-formed lines parse.
func TestParseInfoLineMandatoryFields(t *testing.T) {
	valid := `251 15 0:3573 / /mnt/point rw,relatime shared:1 - aufs none rw`
	invalidCases := []struct {
		name  string
		entry string
	}{
		{"non-numeric mount ID", `x 15 0:3573 / /mnt/point rw - aufs none rw`},
		{"non-numeric parent ID", `251 x 0:3573 / /mnt/point rw - aufs none rw`},
		{"missing colon in dev", `251 15 03573 / /mnt/point rw - aufs none rw`},
		{"non-numeric major", `251 15 x:3573 / /mnt/point rw - aufs none rw`},
		{"non-numeric minor", `251 15 0:x / /mnt/point rw - aufs none rw`},
		{"missing separator", `251 15 0:3573 / /mnt/point rw shared:1 aufs none rw`},
		{"not enough fields", `251 15 0:3573 / /mnt/point rw,relatime aufs none rw`},
	}
	if _, err := parseInfoLine(valid); err != nil {
		t.Fatalf("valid line rejected: %v", err)
	}
	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseInfoLine(tc.entry); err == nil {
				t.Error("expected parse error, got nil")
			}
		})
	}
}

// TestDeviceNumberUnescaping verifies that the device number field uses
// the same \NNN unescaping rules as root and mount point.
func TestDeviceNumberUnescaping(t *testing.T) {
	// \072 is the colon character, so 0\07235 decodes to 0:35.
	entry := `251 15 0\07235 / /mnt/point rw,relatime - aufs none rw`
	infos, err := GetMountsFromReader(bytes.NewBufferString(entry), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(infos))
	}
	if infos[0].Major != 0 || infos[0].Minor != 35 {
		t.Errorf("expected major:minor 0:35, got %d:%d", infos[0].Major, infos[0].Minor)
	}

	// A malformed device number must be reported, not silently decoded
	// into a zero value.
	bad := `251 15 not-a-device / /mnt/point rw,relatime - aufs none rw`
	if _, err := GetMountsFromReader(bytes.NewBufferString(bad), nil); err == nil {
		t.Error("expected error for malformed device number, got nil")
	}
}

// TestFilterRunsOnCompleteSnapshot verifies that parsing always finishes
// the whole input (producing a complete snapshot) before the filter is
// applied. In particular, a filter stop cannot hide a malformed entry
// appearing later in the input.
func TestFilterRunsOnCompleteSnapshot(t *testing.T) {
	input := `1 1 0:1 / /a rw,relatime - tmpfs tmpfs rw
2 1 0:2 / /b rw,relatime - tmpfs tmpfs rw
this line is clearly malformed`

	// SingleEntryFilter stops on the first entry; with filtering on the
	// partial parse the malformed line would never be reached.
	_, err := GetMountsFromReader(bytes.NewBufferString(input), SingleEntryFilter("/a"))
	if err == nil {
		t.Error("expected parse error from a later malformed line, got nil")
	}
}

// TestFilteredMatchesFullSnapshot verifies that every shipped filter
// produces exactly the result of filtering the unfiltered snapshot,
// i.e. no entry is lost or duplicated by the snapshot-then-filter path.
func TestFilteredMatchesFullSnapshot(t *testing.T) {
	filters := map[string]FilterFunc{
		"single":  SingleEntryFilter("/sys/fs/cgroup"),
		"prefix":  PrefixFilter("/sys"),
		"parents": ParentsFilter("/sys/fs/cgroup/cpu,cpuacct"),
		"fstype":  FSTypeFilter("proc", "sysfs"),
	}

	snapshot, err := GetMountsFromReader(bytes.NewBufferString(fedoraMountinfo), nil)
	if err != nil {
		t.Fatal(err)
	}

	for name, filter := range filters {
		t.Run(name, func(t *testing.T) {
			got, err := GetMountsFromReader(bytes.NewBufferString(fedoraMountinfo), filter)
			if err != nil {
				t.Fatal(err)
			}
			want := applyFilter(snapshot, filter)
			if len(got) != len(want) {
				t.Fatalf("expected %d entries, got %d", len(want), len(got))
			}
			for i := range want {
				if *got[i] != *want[i] {
					t.Errorf("entry %d: expected %#v, got %#v", i, want[i], got[i])
				}
			}
		})
	}
}

// TestParseIsDeterministic verifies repeated parsing of the same input
// yields identical results, providing a repeatable verification basis.
func TestParseIsDeterministic(t *testing.T) {
	buf := []byte(fedoraMountinfo + "\n" + mountInfoWithSpaces)
	var want []*Info
	for i := 0; i < 5; i++ {
		got, err := GetMountsFromReader(bytes.NewReader(buf), FSTypeFilter("ext4", "tmpfs", "cifs"))
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			want = got
			continue
		}
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("iteration %d produced a different result", i)
		}
	}
}

// TestGetMountsFromReaderConcurrent exercises concurrent parses with
// different filters to make sure no partial index or parse state is
// shared between calls (run with -race to make the check meaningful).
func TestGetMountsFromReaderConcurrent(t *testing.T) {
	buf := []byte(fedoraMountinfo)
	filters := []FilterFunc{
		nil,
		SingleEntryFilter("/run"),
		PrefixFilter("/sys"),
		FSTypeFilter("cgroup"),
	}
	expected := make([][]*Info, len(filters))
	for i, f := range filters {
		got, err := GetMountsFromReader(bytes.NewReader(buf), f)
		if err != nil {
			t.Fatal(err)
		}
		expected[i] = got
	}

	const goroutines = 32
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for k := 0; k < 10; k++ {
				idx := (n + k) % len(filters)
				got, err := GetMountsFromReader(bytes.NewReader(buf), filters[idx])
				if err != nil {
					errCh <- err
					return
				}
				if !reflect.DeepEqual(got, expected[idx]) {
					errCh <- &mismatchError{idx: idx}
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

type mismatchError struct {
	idx int
}

func (e *mismatchError) Error() string {
	return "concurrent parse produced a different result for filter index " + strconv.Itoa(e.idx)
}

// TestUnescapeSafety covers boundary and out-of-range conditions of the
// escape handling: decoded values stay below utf8.RuneSelf, truncated or
// malformed sequences are preserved verbatim, and the output can never
// exceed the input length.
func TestUnescapeSafety(t *testing.T) {
	cases := []struct {
		input, output string
	}{
		{`\000`, "\x00"},    // minimum value.
		{`\177`, "\x7f"},    // maximum in-range value.
		{`\200`, `\200`},    // value 128 is out of range: untouched.
		{`\377`, `\377`},    // value 255 is out of range: untouched.
		{`\12`, `\12`},      // truncated sequence at end: untouched.
		{`\07`, `\07`},      // truncated sequence at end: untouched.
		{`/x\1234`, "/xS4"}, // \123 decodes to 'S', trailing 4 kept.
		{`\400x`, `\400x`},  // leading 4 is not an octal digit.
		{`a\040b`, "a b"},   // ordinary escape still works.
		{`\040\040`, "  "},  // two escapes decode to two bytes.
		{`\04\040`, `\04 `}, // first sequence is truncated: only the second one decodes.
	}
	for _, tc := range cases {
		got := unescape(tc.input)
		if got != tc.output {
			t.Errorf("unescape(%q) = %q, want %q", tc.input, got, tc.output)
		}
		if len(got) > len(tc.input) {
			t.Errorf("unescape(%q) expanded input: %d > %d", tc.input, len(got), len(tc.input))
		}
	}
}
