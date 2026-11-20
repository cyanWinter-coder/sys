package atomicwriter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// writeVersion creates a write set at target whose single file records
// the given version, and commits it.
func writeVersion(t *testing.T, target, version string, mode os.FileMode) {
	t.Helper()
	ws, err := NewWriteSetForReplace(target, "")
	if err != nil {
		t.Fatalf("NewWriteSetForReplace: %v", err)
	}
	if err := ws.WriteFile("config.txt", []byte(version), testMode()); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := ws.Commit(target); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	assertFile(t, filepath.Join(target, "config.txt"), []byte(version), testMode())
	if mode != 0 {
		fi, err := os.Stat(target)
		if err != nil {
			t.Fatalf("stat target: %v", err)
		}
		if fi.Mode().Perm() != mode {
			t.Fatalf("target mode: got %o, want %o", fi.Mode().Perm(), mode)
		}
	}
}

func TestReplaceExistingDirectory(t *testing.T) {
	target := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "old.txt"), []byte("old"), testMode()); err != nil {
		t.Fatal(err)
	}

	ws, err := NewWriteSetForReplace(target, "")
	if err != nil {
		t.Fatalf("NewWriteSetForReplace: %v", err)
	}
	if err := ws.WriteFile("new.txt", []byte("new"), testMode()); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Before commit the old version is fully intact.
	assertFile(t, filepath.Join(target, "old.txt"), []byte("old"), testMode())
	if _, err := os.Stat(filepath.Join(target, "new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new file must not be visible before commit, got err=%v", err)
	}

	if err := ws.Commit(target); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Only the new version is visible.
	assertFile(t, filepath.Join(target, "new.txt"), []byte("new"), testMode())
	if _, err := os.Stat(filepath.Join(target, "old.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old file must be gone after commit, got err=%v", err)
	}
	assertFileCount(t, target, 1)

	// Only the target directory and the lock file remain in the parent.
	assertFileCount(t, filepath.Dir(target), 2)
}

func TestReplaceRepeatedVersions(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "config")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	writeVersion(t, target, "v1", 0o750)
	writeVersion(t, target, "v2", 0o750)
	writeVersion(t, target, "v3", 0o750)
	assertFileCount(t, parent, 2) // target dir + lock file
}

func TestReplacePreservesTargetMode(t *testing.T) {
	target := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	writeVersion(t, target, "v1", 0o700)
	writeVersion(t, target, "v2", 0o700)
}

func TestReplaceInvalidTargets(t *testing.T) {
	t.Run("target does not exist", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "missing")
		_, err := NewWriteSetForReplace(target, "")
		var pe *PhaseError
		if !errors.As(err, &pe) || pe.Phase != PhasePrepare {
			t.Fatalf("expected prepare PhaseError, got %[1]T (%[1]v)", err)
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expected not-exist error, got %v", err)
		}
	})
	t.Run("target is a file", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "a-file")
		if err := os.WriteFile(target, []byte("x"), testMode()); err != nil {
			t.Fatal(err)
		}
		_, err := NewWriteSetForReplace(target, "")
		var pe *PhaseError
		if !errors.As(err, &pe) || pe.Phase != PhasePrepare {
			t.Fatalf("expected prepare PhaseError, got %v", err)
		}
	})
	t.Run("target is a symlink", func(t *testing.T) {
		dir := t.TempDir()
		real := filepath.Join(dir, "real")
		if err := os.Mkdir(real, 0o750); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "link")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		_, err := NewWriteSetForReplace(link, "")
		var pe *PhaseError
		if !errors.As(err, &pe) || pe.Phase != PhasePrepare {
			t.Fatalf("expected prepare PhaseError, got %v", err)
		}
	})
}

func TestReplaceCommitTargetMismatch(t *testing.T) {
	target := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWriteSetForReplace(target, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.Cancel() }()
	if err := ws.WriteFile("a", []byte("a"), testMode()); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "elsewhere")
	if err := ws.Commit(other); err == nil {
		t.Fatal("expected mismatch error")
	}
	// The failed commit must not have touched the target.
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("target damaged: %v", err)
	}
}

func TestReplaceWriteAfterCommit(t *testing.T) {
	target := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWriteSetForReplace(target, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteFile("a", []byte("a"), testMode()); err != nil {
		t.Fatal(err)
	}
	if err := ws.Commit(target); err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteFile("b", []byte("b"), testMode()); !errors.Is(err, ErrWriteSetClosed) {
		t.Fatalf("expected ErrWriteSetClosed, got %v", err)
	}
	if _, err := ws.FileWriter("c", os.O_CREATE|os.O_WRONLY, testMode()); !errors.Is(err, ErrWriteSetClosed) {
		t.Fatalf("expected ErrWriteSetClosed, got %v", err)
	}
}

func TestReplaceIdempotentCommitAndCancel(t *testing.T) {
	t.Run("repeated commit", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "config")
		if err := os.Mkdir(target, 0o750); err != nil {
			t.Fatal(err)
		}
		ws, err := NewWriteSetForReplace(target, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := ws.WriteFile("a", []byte("a"), testMode()); err != nil {
			t.Fatal(err)
		}
		if err := ws.Commit(target); err != nil {
			t.Fatal(err)
		}
		if err := ws.Commit(target); err != nil {
			t.Fatalf("second Commit must be a no-op, got %v", err)
		}
		if err := ws.Cancel(); err != nil {
			t.Fatalf("Cancel after Commit must be a no-op, got %v", err)
		}
		assertFileCount(t, target, 1)
	})

	t.Run("repeated cancel", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "config")
		if err := os.Mkdir(target, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(target, "kept"), []byte("kept"), testMode()); err != nil {
			t.Fatal(err)
		}
		ws, err := NewWriteSetForReplace(target, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := ws.WriteFile("a", []byte("a"), testMode()); err != nil {
			t.Fatal(err)
		}
		if err := ws.Cancel(); err != nil {
			t.Fatal(err)
		}
		if err := ws.Cancel(); err != nil {
			t.Fatalf("second Cancel must be a no-op, got %v", err)
		}
		if err := ws.Commit(target); !errors.Is(err, ErrWriteSetClosed) {
			t.Fatalf("Commit after Cancel must fail closed, got %v", err)
		}
		// Cancel never touches the existing target directory.
		assertFile(t, filepath.Join(target, "kept"), []byte("kept"), testMode())
		assertFileCount(t, target, 1)
	})
}

func TestReplacePrepareFailureLeavesOldVersion(t *testing.T) {
	target := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "old"), []byte("old"), testMode()); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWriteSetForReplace(target, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.Cancel() }()
	// A write into a non-existing sub-directory fails during the prepare
	// phase; the target must remain untouched.
	if err := ws.WriteFile(filepath.Join("nope", "file"), []byte("x"), testMode()); err == nil {
		t.Fatal("expected prepare failure")
	}
	if err := ws.Cancel(); err != nil {
		t.Fatal(err)
	}
	assertFile(t, filepath.Join(target, "old"), []byte("old"), testMode())
	assertFileCount(t, target, 1)
}

func TestReplaceContextCanceledBeforeSwitch(t *testing.T) {
	target := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "old"), []byte("old"), testMode()); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWriteSetForReplace(target, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteFile("new", []byte("new"), testMode()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = ws.CommitWithContext(ctx, target)
	var pe *PhaseError
	if !errors.As(err, &pe) || pe.Phase != PhasePrepare {
		t.Fatalf("expected prepare PhaseError, got %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	// Nothing was switched; the old version survives and is reusable.
	assertFile(t, filepath.Join(target, "old"), []byte("old"), testMode())
	if err := ws.Commit(target); err != nil {
		t.Fatalf("retry after canceled commit failed: %v", err)
	}
	assertFile(t, filepath.Join(target, "new"), []byte("new"), testMode())
}

func TestReplaceConcurrentCommitters(t *testing.T) {
	target := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ws, err := NewWriteSetForReplace(target, "")
			if err != nil {
				errs <- err
				return
			}
			data := []byte(strings.Repeat(string(rune('a'+i)), 16))
			if err := ws.WriteFile("config.txt", data, testMode()); err != nil {
				errs <- err
				return
			}
			if err := ws.Commit(target); err != nil {
				errs <- err
				return
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent committer failed: %v", err)
	}

	// Exactly one complete version is present.
	got, err := os.ReadFile(filepath.Join(target, "config.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 16 || strings.TrimLeft(string(got), string(got[0])) != "" {
		t.Fatalf("target contains a mixed-up version: %q", got)
	}
	assertFileCount(t, target, 1)
	// Only the lock file may remain next to the target.
	assertFileCount(t, filepath.Dir(target), 2)
}

func TestReplaceRecoversInterruptedExchange(t *testing.T) {
	target := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "old"), []byte("old"), testMode()); err != nil {
		t.Fatal(err)
	}

	ws, err := NewWriteSetForReplace(target, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteFile("new", []byte("new"), testMode()); err != nil {
		t.Fatal(err)
	}
	ws.testHook = func(stage string) {
		if stage == "after-exchange" {
			// Simulate a crash after the atomic exchange but before the
			// old-version directory is removed. The lock is dropped as
			// the process would have died.
			if err := ws.releaseLock(); err != nil {
				t.Errorf("release lock: %v", err)
			}
			runtime.Goexit()
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = ws.Commit(target)
	}()
	<-done

	// The target already holds the new version; an old-version directory
	// is parked under a staging prefix.
	entries := assertFileCount(t, filepath.Dir(target), 3) // target, lock, backup
	var foundBackup bool
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), stagingPattern(target)) {
			foundBackup = true
		}
	}
	if !foundBackup {
		t.Fatal("expected exchange backup directory after simulated crash")
	}
	assertFile(t, filepath.Join(target, "new"), []byte("new"), testMode())

	// Reopening the same target reconciles the leftovers.
	if err := Recover(target); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	assertFileCount(t, target, 1)
	assertFileCount(t, filepath.Dir(target), 2) // target, lock
	if _, err := os.Stat(filepath.Join(target, "old")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old version must not be restored after a completed exchange: %v", err)
	}
}

func TestReplaceRecoversInterruptedJournal(t *testing.T) {
	target := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "old"), []byte("old"), testMode()); err != nil {
		t.Fatal(err)
	}

	ws, err := NewWriteSetForReplace(target, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteFile("new", []byte("new"), testMode()); err != nil {
		t.Fatal(err)
	}
	forceJournaled = true
	t.Cleanup(func() { forceJournaled = false })
	ws.testHook = func(stage string) {
		if stage == "after-move-old" {
			if err := ws.releaseLock(); err != nil {
				t.Errorf("release lock: %v", err)
			}
			runtime.Goexit()
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = ws.Commit(target)
	}()
	<-done

	// The target name is momentarily absent; journal and backup record
	// how to roll back.
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target should be absent at this crash point, got %v", err)
	}
	if _, err := os.Stat(journalPath(target)); err != nil {
		t.Fatalf("journal missing: %v", err)
	}
	if _, err := os.Stat(backupPath(target)); err != nil {
		t.Fatalf("backup missing: %v", err)
	}

	// Reopening rolls back to the complete old version and removes every
	// artifact belonging to the package.
	ws2, err := NewWriteSetForReplace(target, "")
	if err != nil {
		t.Fatalf("reopen after crash: %v", err)
	}
	if err := ws2.Cancel(); err != nil {
		t.Fatal(err)
	}
	assertFile(t, filepath.Join(target, "old"), []byte("old"), testMode())
	assertFileCount(t, target, 1)
	for _, name := range []string{
		filepath.Base(journalPath(target)),
		filepath.Base(backupPath(target)),
	} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(target), name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("artifact %q was not cleaned up", name)
		}
	}
}

func TestReplaceRecoversSwitchedJournal(t *testing.T) {
	target := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "old"), []byte("old"), testMode()); err != nil {
		t.Fatal(err)
	}

	// Forge the on-disk state of a crash right after the new directory
	// was renamed onto the target, with the backup still present and the
	// journal in the "moving-new" phase.
	parent := filepath.Dir(target)
	backup := backupPath(target)
	if err := os.Rename(target, backup); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(parent, stagingPattern(target)+"deadbeef")
	if err := os.Mkdir(staged, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, "new"), []byte("new"), testMode()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staged, target); err != nil {
		t.Fatal(err)
	}
	state := journalState{
		Version: journalVersion,
		Target:  target,
		Staging: staged,
		Backup:  backup,
		Phase:   journalMovingNew,
	}
	if err := writeJournal(journalPath(target), state); err != nil {
		t.Fatal(err)
	}

	if err := Recover(target); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	// The new version had reached the target; recovery keeps it and
	// drops the backup.
	assertFile(t, filepath.Join(target, "new"), []byte("new"), testMode())
	if _, err := os.Stat(backup); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backup should have been removed: %v", err)
	}
	if _, err := os.Stat(journalPath(target)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal should have been removed: %v", err)
	}
}

func TestReplaceRollbackPhaseError(t *testing.T) {
	forceJournaled = true
	t.Cleanup(func() { forceJournaled = false })

	target := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "old"), []byte("old"), testMode()); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWriteSetForReplace(target, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteFile("new", []byte("new"), testMode()); err != nil {
		t.Fatal(err)
	}
	// Make the second rename fail (staged directory removed beforehand),
	// and make the rollback fail as well to exercise PhaseRollback.
	ws.testHook = func(stage string) {
		if stage == "after-move-old" {
			entries, err := os.ReadDir(filepath.Dir(target))
			if err != nil {
				t.Errorf("read parent: %v", err)
				return
			}
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), stagingPattern(target)) && e.IsDir() {
					if err := os.RemoveAll(filepath.Join(filepath.Dir(target), e.Name())); err != nil {
						t.Errorf("remove staged: %v", err)
					}
				}
			}
		}
	}
	ws.testRollbackErr = errors.New("injected rollback failure")

	err = ws.Commit(target)
	var pe *PhaseError
	if !errors.As(err, &pe) {
		t.Fatalf("expected PhaseError, got %[1]T (%[1]v)", err)
	}
	if pe.Phase != PhaseRollback {
		t.Fatalf("expected rollback phase, got %q: %v", pe.Phase, err)
	}
	if !errors.Is(err, ws.testRollbackErr) {
		t.Fatalf("expected injected error to be wrapped, got %v", err)
	}
}

func TestReplaceDoesNotTouchUnrelatedContent(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "config")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	// Caller-maintained sibling files and directories must survive both
	// commit and recovery sweeps.
	siblingFile := filepath.Join(parent, "keep-me.txt")
	if err := os.WriteFile(siblingFile, []byte("mine"), testMode()); err != nil {
		t.Fatal(err)
	}
	siblingDir := filepath.Join(parent, "keep-me-dir")
	if err := os.Mkdir(siblingDir, 0o750); err != nil {
		t.Fatal(err)
	}
	// A directory sharing only part of the prefix must not be swept.
	lookalike := filepath.Join(parent, stagingPrefix+"other-target-xyz")
	if err := os.Mkdir(lookalike, 0o750); err != nil {
		t.Fatal(err)
	}

	ws, err := NewWriteSetForReplace(target, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteFile("a", []byte("a"), testMode()); err != nil {
		t.Fatal(err)
	}
	if err := ws.Commit(target); err != nil {
		t.Fatal(err)
	}
	if err := Recover(target); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{siblingFile, siblingDir, lookalike} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("unrelated content %q was removed: %v", p, err)
		}
	}
}

func TestReplaceStaleStagingSweptOnOpen(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "config")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(parent, stagingPattern(target)+"stale")
	if err := os.Mkdir(stale, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "junk"), []byte("junk"), testMode()); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWriteSetForReplace(target, "")
	if err != nil {
		t.Fatalf("opening should sweep stale staging dir: %v", err)
	}
	if err := ws.Cancel(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale staging directory not removed: %v", err)
	}
}

func TestReplaceCrossVolumeFallback(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions used by this test")
	}
	// A tmpDir on a different filesystem forces relocateStaging through
	// its copy-tree branch. /dev/shm is tmpfs and hence a different
	// filesystem from the test temp dir on the root filesystem; skip
	// gracefully when unavailable or read-only.
	tmp, err := os.MkdirTemp("/dev/shm", "atomicwriter-xvol-")
	if err != nil {
		t.Skipf("cannot create cross-volume tmp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })

	target := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "old"), []byte("old"), testMode()); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(target, "link")
	if err := os.Symlink("old", link); err != nil {
		t.Fatal(err)
	}

	ws, err := NewWriteSetForReplace(target, tmp)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(strings.Repeat("cross-volume payload\n", 128))
	if err := ws.WriteFile("new", payload, testMode()); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("new", filepath.Join(ws.root, "new-link")); err != nil {
		t.Fatal(err)
	}
	if err := ws.Commit(target); err != nil {
		t.Fatalf("Commit across volumes: %v", err)
	}
	assertFile(t, filepath.Join(target, "new"), payload, testMode())
	gotLink, err := os.Readlink(filepath.Join(target, "new-link"))
	if err != nil || gotLink != "new" {
		t.Fatalf("symlink not preserved: %q, %v", gotLink, err)
	}
	assertFileCount(t, target, 2)
	assertFileCount(t, tmp, 0)
}

func TestReplaceLockSerializes(t *testing.T) {
	target := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	// Hold the lock externally: construction must block, then proceed
	// once it is released.
	external, err := acquireTxnLock(lockPath(target))
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan struct{})
	go func() {
		ws, err := NewWriteSetForReplace(target, "")
		if err != nil {
			panic(err)
		}
		_ = ws.Cancel()
		close(got)
	}()
	select {
	case <-got:
		t.Fatal("constructor returned while external lock was held")
	case <-time.After(50 * time.Millisecond):
	}
	if err := external.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("constructor did not proceed after lock release")
	}
}

func TestPhaseErrorWrapping(t *testing.T) {
	pe := &PhaseError{Phase: PhaseRollback, Err: os.ErrPermission}
	if !errors.Is(pe, os.ErrPermission) {
		t.Fatal("errors.Is must unwrap PhaseError")
	}
	if pe.Error() == "" || !strings.Contains(pe.Error(), string(PhaseRollback)) {
		t.Fatalf("unexpected error text: %q", pe.Error())
	}
}

func TestLegacyWriteSetUnchanged(t *testing.T) {
	// Legacy sets have no target/lock and keep the old Commit-to-new-dir
	// behavior, including a non-idempotent second attempt erroring on the
	// existing destination.
	tmp := t.TempDir()
	ws, err := NewWriteSet(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteFile("a", []byte("a"), testMode()); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := ws.Commit(target); err != nil {
		t.Fatal(err)
	}
	assertFile(t, filepath.Join(target, "a"), []byte("a"), testMode())
}
