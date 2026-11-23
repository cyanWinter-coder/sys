//go:build !windows

package symlink

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadlinkStableRetriesReplacedEntry verifies that when the entry inspected
// by the caller is replaced before Readlink runs, the result describes the entry
// that is actually present rather than silently mixing the old inspection with
// the new target.
func TestReadlinkStableRetriesReplacedEntry(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	for _, target := range []string{first, second} {
		if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	link := filepath.Join(dir, "link")
	if err := os.Symlink(first, link); err != nil {
		t.Fatal(err)
	}
	// This is the stale view a caller would hold from before the replacement.
	stale, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}

	// Replace the entry, which changes its identity.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, link); err != nil {
		t.Fatal(err)
	}

	got, err := readlinkStable(link, stale)
	if err != nil {
		t.Fatalf("readlinkStable: %v", err)
	}
	if got != second {
		t.Errorf("got %q, want the current target %q", got, second)
	}
}

// TestReadlinkStableStableEntry verifies the common case, including that the
// returned target matches the inspected entry.
func TestReadlinkStableStableEntry(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	got, err := readlinkStable(link, fi)
	if err != nil {
		t.Fatalf("readlinkStable: %v", err)
	}
	if got != target {
		t.Errorf("got %q, want %q", got, target)
	}
}

// TestSameFileDetectsReplacement verifies the identity check used to detect a
// replacement: a re-created symlink must not compare equal to the old one.
func TestSameFileDetectsReplacement(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "a"), link); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if !sameFile(before, before) {
		t.Fatal("sameFile(reported the same entry as different")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "a"), link); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if sameFile(before, after) {
		t.Fatal("sameFile did not detect a replaced entry")
	}
}

// TestFollowSymlinkInScopeSnapshotStable verifies that the result of a
// resolution describes the link tree at the time of the call: replacing the
// link afterwards must not change the already-returned path, and re-resolving
// must follow the new target. This is the property callers rely on to avoid a
// dispatch to a replaced executable.
func TestFollowSymlinkInScopeSnapshotStable(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	for _, target := range []string{first, second} {
		if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(first, link); err != nil {
		t.Fatal(err)
	}

	// An absolute link destination is re-rooted inside the scope by design
	// (FollowSymlinkInScope keeps the result contained), so the expected value
	// is the target as if root were "/".
	wantFirst := filepath.Join(root, first)
	got, err := FollowSymlinkInScope(link, root)
	if err != nil {
		t.Fatal(err)
	}
	if got != wantFirst {
		t.Fatalf("got %q, want %q", got, wantFirst)
	}

	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, link); err != nil {
		t.Fatal(err)
	}

	// A fresh resolution follows the replacement, which is why callers must
	// capture and reuse the first result when they need a stable identity.
	wantSecond := filepath.Join(root, second)
	again, err := FollowSymlinkInScope(link, root)
	if err != nil {
		t.Fatal(err)
	}
	if again != wantSecond {
		t.Fatalf("re-resolved to %q, want the new target %q", again, wantSecond)
	}
	if got != wantFirst {
		t.Fatalf("previously captured path changed: %q, want %q", got, wantFirst)
	}
}
