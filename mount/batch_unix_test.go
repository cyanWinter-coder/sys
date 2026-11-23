//go:build !darwin && !windows

package mount

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/moby/sys/mountinfo"
)

func TestValidateMounts(t *testing.T) {
	target := t.TempDir()
	source := t.TempDir()

	tests := []struct {
		name    string
		specs   []MountSpec
		wantErr bool
	}{
		{
			name:  "valid tmpfs",
			specs: []MountSpec{{Source: "tmpfs", Target: target, FSType: "tmpfs", Options: "noexec"}},
		},
		{
			name:  "valid bind",
			specs: []MountSpec{{Source: source, Target: target, Options: "bind"}},
		},
		{
			name:  "valid bind ro propagation",
			specs: []MountSpec{{Source: source, Target: target, Options: "rbind,ro,rprivate"}},
		},
		{
			name:    "empty batch",
			specs:   nil,
			wantErr: false,
		},
		{
			name:    "empty target",
			specs:   []MountSpec{{Source: "tmpfs", FSType: "tmpfs"}},
			wantErr: true,
		},
		{
			name:    "relative target",
			specs:   []MountSpec{{Source: "tmpfs", Target: "relative/path", FSType: "tmpfs"}},
			wantErr: true,
		},
		{
			name:    "missing target",
			specs:   []MountSpec{{Source: "tmpfs", Target: filepath.Join(target, "no-such-dir"), FSType: "tmpfs"}},
			wantErr: true,
		},
		{
			name: "duplicate targets",
			specs: []MountSpec{
				{Source: "tmpfs", Target: target, FSType: "tmpfs"},
				{Source: "tmpfs", Target: target, FSType: "tmpfs"},
			},
			wantErr: true,
		},
		{
			name:    "conflicting propagation",
			specs:   []MountSpec{{Source: source, Target: target, Options: "bind,shared,private"}},
			wantErr: true,
		},
		{
			name:    "remount option",
			specs:   []MountSpec{{Source: "tmpfs", Target: target, FSType: "tmpfs", Options: "remount,ro"}},
			wantErr: true,
		},
		{
			name:    "empty source is a remount",
			specs:   []MountSpec{{Source: "", Target: target, Options: "ro"}},
			wantErr: true,
		},
		{
			name:    "none source is a remount",
			specs:   []MountSpec{{Source: "none", Target: target, Options: "shared"}},
			wantErr: true,
		},
		{
			name:    "bind without source",
			specs:   []MountSpec{{Target: target, Options: "bind"}},
			wantErr: true,
		},
		{
			name:    "missing bind source",
			specs:   []MountSpec{{Source: filepath.Join(source, "no-such-dir"), Target: target, Options: "bind"}},
			wantErr: true,
		},
		{
			name:    "empty option element",
			specs:   []MountSpec{{Source: "tmpfs", Target: target, FSType: "tmpfs", Options: "ro,,noexec"}},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateMounts(tc.specs)
			if tc.wantErr && err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

// mounted reports whether target is a mount point in the current namespace.
func mounted(t *testing.T, target string) bool {
	t.Helper()
	m, err := mountinfo.Mounted(target)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMountAll(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("root required")
	}

	target1 := t.TempDir()
	target2 := t.TempDir()
	specs := []MountSpec{
		{Source: "tmpfs", Target: target1, FSType: "tmpfs", Options: "noexec"},
		{Source: "tmpfs", Target: target2, FSType: "tmpfs", Options: "ro,size=128k"},
	}

	if err := MountAll(specs); err != nil {
		t.Fatal(err)
	}
	defer ensureUnmount(t, target1)
	defer ensureUnmount(t, target2)

	for _, target := range []string{target1, target2} {
		if !mounted(t, target) {
			t.Errorf("expected %s to be mounted", target)
		}
	}
}

func TestMountAllRollback(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("root required")
	}

	target1 := t.TempDir()
	target2 := t.TempDir()
	specs := []MountSpec{
		{Source: "tmpfs", Target: target1, FSType: "tmpfs"},
		{Source: "tmpfs", Target: target2, FSType: "a-nonexistent-fs"},
	}

	err := MountAll(specs)
	if err == nil {
		ensureUnmount(t, target1)
		ensureUnmount(t, target2)
		t.Fatal("expected an error, got nil")
	}

	var berr *BatchError
	if !errors.As(err, &berr) {
		t.Fatalf("expected a *BatchError, got %T: %v", err, err)
	}
	if berr.Index != 1 {
		t.Errorf("expected failing index 1, got %d", berr.Index)
	}
	if len(berr.Rollback) != 0 {
		t.Errorf("expected a clean rollback, got %v", berr.Rollback)
	}

	// The whole batch must be rolled back: nothing may remain mounted.
	for _, target := range []string{target1, target2} {
		if mounted(t, target) {
			t.Errorf("expected %s to be unmounted after rollback", target)
		}
	}

	// A failed batch leaves no trace, so it can be retried as is once
	// the cause is fixed.
	specs[1].FSType = "tmpfs"
	if err := MountAll(specs); err != nil {
		t.Fatal(err)
	}
	defer ensureUnmount(t, target1)
	defer ensureUnmount(t, target2)
	for _, target := range []string{target1, target2} {
		if !mounted(t, target) {
			t.Errorf("expected %s to be mounted after retry", target)
		}
	}
}

func TestMountAllValidationFailsBeforeAnyMount(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("root required")
	}

	target := t.TempDir()
	specs := []MountSpec{
		{Source: "tmpfs", Target: target, FSType: "tmpfs"},
		{Source: "tmpfs", Target: "relative/path", FSType: "tmpfs"},
	}

	if err := MountAll(specs); err == nil {
		ensureUnmount(t, target)
		t.Fatal("expected a validation error, got nil")
	}
	if mounted(t, target) {
		t.Error("nothing may be mounted when validation fails")
	}
}

func TestMountAllConcurrentUnmount(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("root required")
	}

	const batchSize = 4
	targets := make([]string, 0, batchSize)
	specs := make([]MountSpec, 0, batchSize)
	for i := 0; i < batchSize; i++ {
		target := t.TempDir()
		targets = append(targets, target)
		specs = append(specs, MountSpec{Source: "tmpfs", Target: target, FSType: "tmpfs"})
	}
	// An unrelated mount point that concurrent Unmount calls may
	// legitimately remove; it must not be disturbed by the batch.
	other := t.TempDir()
	if err := Mount("tmpfs", other, "tmpfs", ""); err != nil {
		t.Fatal(err)
	}

	var (
		wg       sync.WaitGroup
		batchErr error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		batchErr = MountAll(specs)
	}()
	go func() {
		defer wg.Done()
		// Races with the batch; must neither deadlock nor observe a
		// half-committed batch. Unmount of a not-yet/never mounted
		// target is a no-op.
		for i := 0; i < 10; i++ {
			if err := Unmount(other); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	wg.Wait()

	if mounted(t, other) {
		ensureUnmount(t, other)
	}

	if batchErr != nil {
		// Overall rollback: no batch target may remain mounted.
		for _, target := range targets {
			if mounted(t, target) {
				t.Errorf("expected %s to be unmounted after failed batch", target)
			}
		}
		return
	}
	// Full commit: every batch target is mounted.
	for _, target := range targets {
		if !mounted(t, target) {
			t.Errorf("expected %s to be mounted after successful batch", target)
		}
		ensureUnmount(t, target)
	}
}
