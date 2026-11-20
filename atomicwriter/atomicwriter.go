// Package atomicwriter provides utilities to perform atomic writes to a
// file or set of files.
package atomicwriter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/moby/sys/sequential"
)

func validateDestination(fileName string) error {
	if fileName == "" {
		return errors.New("file name is empty")
	}
	if dir := filepath.Dir(fileName); dir != "" && dir != "." && dir != ".." {
		di, err := os.Stat(dir)
		if err != nil {
			return fmt.Errorf("invalid output path: %w", err)
		}
		if !di.IsDir() {
			return fmt.Errorf("invalid output path: %w", &os.PathError{Op: "stat", Path: dir, Err: syscall.ENOTDIR})
		}
	}

	// Deliberately using Lstat here to match the behavior of [os.Rename],
	// which is used when completing the write and does not resolve symlinks.
	fi, err := os.Lstat(fileName)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to stat output path: %w", err)
	}

	switch mode := fi.Mode(); {
	case mode.IsRegular():
		return nil // Regular file
	case mode&os.ModeDir != 0:
		return errors.New("cannot write to a directory")
	case mode&os.ModeSymlink != 0:
		return errors.New("cannot write to a symbolic link directly")
	case mode&os.ModeNamedPipe != 0:
		return errors.New("cannot write to a named pipe (FIFO)")
	case mode&os.ModeSocket != 0:
		return errors.New("cannot write to a socket")
	case mode&os.ModeDevice != 0:
		if mode&os.ModeCharDevice != 0 {
			return errors.New("cannot write to a character device file")
		}
		return errors.New("cannot write to a block device file")
	case mode&os.ModeSetuid != 0:
		return errors.New("cannot write to a setuid file")
	case mode&os.ModeSetgid != 0:
		return errors.New("cannot write to a setgid file")
	case mode&os.ModeSticky != 0:
		return errors.New("cannot write to a sticky bit file")
	default:
		return fmt.Errorf("unknown file mode: %[1]s (%#[1]o)", mode)
	}
}

// New returns a WriteCloser so that writing to it writes to a
// temporary file and closing it atomically changes the temporary file to
// destination path. Writing and closing concurrently is not allowed.
// NOTE: umask is not considered for the file's permissions.
//
// New uses [sequential.CreateTemp] to use sequential file access on Windows,
// avoiding depleting the standby list un-necessarily. On Linux, this equates to
// a regular [os.CreateTemp]. Refer to the [Win32 API documentation] for details
// on sequential file access.
//
// [Win32 API documentation]: https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-createfilea#FILE_FLAG_SEQUENTIAL_SCAN
func New(filename string, perm os.FileMode) (io.WriteCloser, error) {
	if err := validateDestination(filename); err != nil {
		return nil, err
	}
	abspath, err := filepath.Abs(filename)
	if err != nil {
		return nil, err
	}

	f, err := sequential.CreateTemp(filepath.Dir(abspath), ".tmp-"+filepath.Base(filename))
	if err != nil {
		return nil, err
	}
	return &atomicFileWriter{
		f:    f,
		fn:   abspath,
		perm: perm,
	}, nil
}

// WriteFile atomically writes data to a file named by filename and with the
// specified permission bits. The given filename is created if it does not exist,
// but the destination directory must exist. It can be used as a drop-in replacement
// for [os.WriteFile], but currently does not allow the destination path to be
// a symlink. WriteFile is implemented using [New] for its implementation.
//
// NOTE: umask is not considered for the file's permissions.
func WriteFile(filename string, data []byte, perm os.FileMode) error {
	f, err := New(filename, perm)
	if err != nil {
		return err
	}
	n, err := f.Write(data)
	if err == nil && n < len(data) {
		err = io.ErrShortWrite
		f.(*atomicFileWriter).writeErr = err
	}
	if err1 := f.Close(); err == nil {
		err = err1
	}
	return err
}

type atomicFileWriter struct {
	f        *os.File
	fn       string
	writeErr error
	written  bool
	perm     os.FileMode
}

func (w *atomicFileWriter) Write(dt []byte) (int, error) {
	w.written = true
	n, err := w.f.Write(dt)
	if err != nil {
		w.writeErr = err
	}
	return n, err
}

func (w *atomicFileWriter) Close() (retErr error) {
	defer func() {
		if err := os.Remove(w.f.Name()); !errors.Is(err, os.ErrNotExist) && retErr == nil {
			retErr = err
		}
	}()
	if err := w.f.Sync(); err != nil {
		_ = w.f.Close()
		return err
	}
	if err := w.f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(w.f.Name(), w.perm); err != nil {
		return err
	}
	if w.writeErr == nil && w.written {
		return os.Rename(w.f.Name(), w.fn)
	}
	return nil
}

// Phase identifies the stage of a replacement transaction in which an
// error occurred. It allows callers to tell preparation failures, switch
// failures and rollback failures apart.
type Phase string

const (
	// PhasePrepare covers building and syncing the complete new
	// directory, before the existing target directory is touched.
	PhasePrepare Phase = "prepare"

	// PhaseSwitch covers the atomic switch (or the journaled rename
	// sequence on platforms without an atomic exchange primitive).
	PhaseSwitch Phase = "switch"

	// PhaseRollback covers restoring the original directory after a
	// failed switch.
	PhaseRollback Phase = "rollback"

	// PhaseRecover covers reconciling leftovers of an interrupted
	// transaction from a previous process.
	PhaseRecover Phase = "recover"
)

// PhaseError wraps an error with the transaction phase it occurred in.
// Use [errors.As] to inspect the phase, and [errors.Is] for the wrapped
// error.
type PhaseError struct {
	// Phase is the stage of the replacement transaction that failed.
	Phase Phase
	// Err is the underlying error.
	Err error
}

func (e *PhaseError) Error() string {
	return fmt.Sprintf("atomicwriter: %s phase failed: %v", e.Phase, e.Err)
}

func (e *PhaseError) Unwrap() error {
	return e.Err
}

// ErrWriteSetClosed is returned when writing to or committing a write set
// that has already been committed or canceled.
var ErrWriteSetClosed = errors.New("write set is already committed or canceled")

// On-disk artifact names. All artifacts live in the parent directory of the
// target and carry a fixed, package-specific prefix or suffix so that crash
// recovery can recognize them without ever touching unrelated content.
const (
	// stagingPrefix is the prefix of temporary directories holding a
	// prepared new version, and of exchange-mode backup directories.
	stagingPrefix = ".atomicwriter-new-"
	// backupSuffix marks the journaled-mode backup of the old version.
	backupSuffix = ".atomicwriter-backup"
	// journalSuffix marks the recoverable switch journal.
	journalSuffix = ".atomicwriter-journal"
	// lockSuffix marks the per-target advisory transaction lock.
	lockSuffix = ".atomicwriter-lock"

	journalVersion = 1
)

func stagingPattern(target string) string {
	return stagingPrefix + filepath.Base(target) + "-"
}

func backupPath(target string) string {
	return target + backupSuffix
}

func journalPath(target string) string {
	return target + journalSuffix
}

func lockPath(target string) string {
	return filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+lockSuffix)
}

// WriteSet is used to atomically write a set of files and ensure they are
// visible at the same time.
//
// A write set created with [NewWriteSet] must be committed to a new,
// non-existing directory with [WriteSet.Commit].
//
// A write set created with [NewWriteSetForReplace] instead replaces the
// content of an existing directory transactionally: observers only ever see
// the complete old version or the complete new version, a failed switch
// restores the original directory, and an interrupted transaction is rolled
// back or cleaned up when the next write set is opened (or explicitly via
// [Recover]).
type WriteSet struct {
	root string

	// Replacement-transaction state. The zero values denote a legacy
	// write set created with NewWriteSet.
	mu         sync.Mutex
	target     string      // absolute target directory; "" for legacy sets
	targetMode os.FileMode // mode of the original target directory
	lock       io.Closer   // advisory lock serializing transactions on target
	done       bool        // Commit or Cancel has finalized the set
	doneErr    error       // terminal error, returned again on repeated Commit

	// testHook, when non-nil, is invoked at well-defined switch points.
	// It exists for in-package tests only and is never set in production.
	testHook func(stage string)
	// testRollbackErr, when non-nil, is returned by restoreBackup to
	// simulate a failed rollback. Test-only.
	testRollbackErr error
}

// NewWriteSet creates a new atomic write set to atomically create a set of
// files. The given directory is used as the base directory for storing
// files before commit. If no temporary directory is given the system
// default is used.
func NewWriteSet(tmpDir string) (*WriteSet, error) {
	td, err := os.MkdirTemp(tmpDir, "write-set-")
	if err != nil {
		return nil, err
	}

	return &WriteSet{
		root: td,
	}, nil
}

// NewWriteSetForReplace creates an atomic write set that replaces the
// content of an existing directory, target, transactionally.
//
// The target must be an existing directory; it is not modified until
// [WriteSet.Commit] switches the prepared new version into place. If a
// previous process was interrupted while committing to the same target,
// its leftovers are reconciled first: the old version is restored or the
// completed new version is kept, and all temporary artifacts are removed.
// Content in the parent directory that does not belong to this package is
// never removed.
//
// All transactions touching the same target are serialized through an
// advisory lock held until Commit or Cancel. If tmpDir is empty the
// staging directory is created next to the target so the switch can use a
// same-volume atomic rename. A tmpDir on another volume is supported as
// well; the staged tree is copied across during the switch before any
// rename happens.
func NewWriteSetForReplace(target, tmpDir string) (*WriteSet, error) {
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return nil, &PhaseError{Phase: PhasePrepare, Err: err}
	}

	lock, err := acquireTxnLock(lockPath(absTarget))
	if err != nil {
		return nil, &PhaseError{Phase: PhaseRecover, Err: err}
	}

	// Reconcile an interrupted transaction from a previous process while
	// holding the lock so it cannot race a still-live committer. Recovery
	// must run before validating the target: inside the journaled switch
	// window the target name is briefly absent and only the backup plus
	// journal describe how to bring it back.
	if err := recoverLocked(absTarget); err != nil {
		_ = lock.Close()
		return nil, &PhaseError{Phase: PhaseRecover, Err: err}
	}

	fi, err := os.Lstat(absTarget)
	if err != nil {
		_ = lock.Close()
		return nil, &PhaseError{Phase: PhasePrepare, Err: fmt.Errorf("target directory: %w", err)}
	}
	if !fi.IsDir() {
		_ = lock.Close()
		return nil, &PhaseError{Phase: PhasePrepare, Err: &fs.PathError{Op: "stat", Path: absTarget, Err: syscall.ENOTDIR}}
	}

	if tmpDir == "" {
		tmpDir = filepath.Dir(absTarget)
	}
	root, err := os.MkdirTemp(tmpDir, stagingPattern(absTarget))
	if err != nil {
		_ = lock.Close()
		return nil, &PhaseError{Phase: PhasePrepare, Err: err}
	}

	return &WriteSet{
		root:       root,
		target:     absTarget,
		targetMode: fi.Mode().Perm(),
		lock:       lock,
	}, nil
}

// WriteFile writes a file to the set, guaranteeing the file has been synced.
func (ws *WriteSet) WriteFile(filename string, data []byte, perm os.FileMode) error {
	if ws.isDone() {
		return ErrWriteSetClosed
	}
	f, err := ws.FileWriter(filename, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	n, err := f.Write(data)
	if err == nil && n < len(data) {
		err = io.ErrShortWrite
	}
	if err1 := f.Close(); err == nil {
		err = err1
	}
	return err
}

type syncFileCloser struct {
	*os.File
}

func (w syncFileCloser) Close() error {
	err := w.File.Sync()
	if err1 := w.File.Close(); err == nil {
		err = err1
	}
	return err
}

// FileWriter opens a file writer inside the set. The file should be synced
// and closed before calling commit.
//
// FileWriter uses [sequential.OpenFile] to use sequential file access on Windows,
// avoiding depleting the standby list un-necessarily. On Linux, this equates to
// a regular [os.OpenFile]. Refer to the [Win32 API documentation] for details
// on sequential file access.
//
// [Win32 API documentation]: https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-createfilea#FILE_FLAG_SEQUENTIAL_SCAN
func (ws *WriteSet) FileWriter(name string, flag int, perm os.FileMode) (io.WriteCloser, error) {
	if ws.isDone() {
		return nil, ErrWriteSetClosed
	}
	f, err := sequential.OpenFile(filepath.Join(ws.root, name), flag, perm)
	if err != nil {
		return nil, err
	}
	return syncFileCloser{f}, nil
}

// Cancel cancels the set and removes all temporary data created in the set.
// It is safe to call more than once, and after a successful Commit; in both
// cases it is a no-op.
func (ws *WriteSet) Cancel() error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.done {
		return nil
	}
	ws.done = true
	ws.doneErr = ErrWriteSetClosed

	var errs []error
	if err := os.RemoveAll(ws.root); err != nil {
		errs = append(errs, err)
	}
	if ws.lock != nil {
		if err := ws.lock.Close(); err != nil {
			errs = append(errs, err)
		}
		ws.lock = nil
	}
	return errors.Join(errs...)
}

// Commit moves all created files to the target directory.
//
// For write sets created with [NewWriteSet], the target directory must not
// exist and the parent of the target directory must exist.
//
// For write sets created with [NewWriteSetForReplace], target must be the
// same directory given to the constructor; its whole content is replaced by
// the prepared set. Commit is idempotent: calling it again after a
// successful commit is a no-op, and a failed commit keeps returning the
// same [PhaseError]. The context is observed until the switch starts; once
// the atomic switch is in progress, cancellation can no longer change the
// outcome and a single deterministic terminal state is reached.
func (ws *WriteSet) Commit(target string) error {
	return ws.CommitWithContext(context.Background(), target)
}

// CommitWithContext is like [WriteSet.Commit] but honors ctx until the
// point where the switch becomes irreversible. A canceled context before
// that point leaves the target untouched and the write set usable for a
// retry.
func (ws *WriteSet) CommitWithContext(ctx context.Context, target string) error {
	if ws.target == "" {
		return ws.commitLegacy(target)
	}
	return ws.commitReplace(ctx, target)
}

func (ws *WriteSet) commitLegacy(target string) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.done {
		return ws.doneErr
	}
	if err := os.Rename(ws.root, target); err != nil {
		return err
	}
	ws.done = true
	return nil
}

func (ws *WriteSet) commitReplace(ctx context.Context, target string) error {
	if err := ctx.Err(); err != nil {
		// Nothing has been touched yet; leave the set retryable.
		return &PhaseError{Phase: PhasePrepare, Err: err}
	}

	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.done {
		return ws.doneErr
	}

	absTarget, err := filepath.Abs(target)
	if err != nil {
		return ws.fail(PhasePrepare, err)
	}
	if absTarget != ws.target {
		return ws.fail(PhasePrepare, fmt.Errorf("commit target %q does not match replacement target %q", absTarget, ws.target))
	}
	if err := ctx.Err(); err != nil {
		return &PhaseError{Phase: PhasePrepare, Err: err}
	}

	// Prepare phase: the new directory must be complete and durable
	// before the existing target is touched. Keep the original directory
	// mode so a configuration update does not silently change it.
	if err := os.Chmod(ws.root, ws.targetMode); err != nil {
		return ws.fail(PhasePrepare, err)
	}
	if err := syncTree(ws.root); err != nil {
		return ws.fail(PhasePrepare, err)
	}

	// Switch phase. Any error returned from here is classified as a
	// switch or rollback failure; the target has either been replaced or
	// restored, never left half-populated by us.
	phase, err := ws.switchIntoPlace(ctx)
	if err != nil {
		_ = ws.releaseLock()
		return ws.fail(phase, err)
	}

	ws.done = true
	if err := ws.releaseLock(); err != nil {
		// The switch itself succeeded; a lock release problem is a
		// cleanup issue, not a failed replacement.
		return ws.fail(PhaseSwitch, err)
	}
	return nil
}

// fail records the terminal state of the set and returns the matching
// PhaseError. Caller must hold ws.mu.
func (ws *WriteSet) fail(phase Phase, err error) error {
	ws.done = true
	ws.doneErr = &PhaseError{Phase: phase, Err: err}
	return ws.doneErr
}

func (ws *WriteSet) releaseLock() error {
	if ws.lock == nil {
		return nil
	}
	err := ws.lock.Close()
	ws.lock = nil
	return err
}

func (ws *WriteSet) isDone() bool {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.done
}

// String returns the location the set is writing to.
func (ws *WriteSet) String() string {
	return ws.root
}

// switchIntoPlace relocates the staged tree next to the target (same
// volume) and performs the atomic exchange, falling back to a journaled
// rename sequence when the platform or filesystem cannot exchange two
// directories atomically.
func (ws *WriteSet) switchIntoPlace(ctx context.Context) (Phase, error) {
	parent := filepath.Dir(ws.target)

	staged, err := ws.relocateStaging(parent)
	if err != nil {
		return PhaseSwitch, err
	}

	if err := exchangeDir(staged, ws.target); err != nil {
		if !isExchangeUnsupported(err) {
			// The target was not modified. The staged directory stays in
			// the parent until Recover/Cancel sweeps it.
			return PhaseSwitch, err
		}
		// No atomic exchange available (unsupported syscall or
		// filesystem). Use the recoverable journal.
		return ws.switchJournaled(ctx, staged)
	}

	// Atomic exchange succeeded: target now holds the new version and
	// staged holds the old one. A crash between the exchange and this
	// cleanup is reconciled by recoverLocked.
	if ws.testHook != nil {
		ws.testHook("after-exchange")
	}
	if err := os.RemoveAll(staged); err != nil {
		// Leftover is recognized and swept on the next open.
		return "", nil
	}
	_ = syncDir(parent)
	return "", nil
}

// relocateStaging makes the prepared tree available under a unique,
// package-prefixed name in the target's parent directory, which is the
// only location rename-based switching can operate atomically from.
func (ws *WriteSet) relocateStaging(parent string) (string, error) {
	empty, err := os.MkdirTemp(parent, stagingPattern(ws.target))
	if err != nil {
		return "", err
	}
	staged := empty
	if err := os.Remove(empty); err != nil {
		return "", err
	}

	if err := os.Rename(ws.root, staged); err == nil {
		_ = syncDir(parent)
		return staged, nil
	} else if !isCrossDeviceRename(err) {
		return "", err
	}

	// tmpDir lives on another volume than the target: copy the complete
	// tree while still in the prepare window, then drop the original.
	if err := copyTree(ws.root, staged); err != nil {
		_ = os.RemoveAll(staged)
		return "", err
	}
	if err := os.RemoveAll(ws.root); err != nil {
		return "", err
	}
	_ = syncDir(parent)
	return staged, nil
}

// Journal phases persisted to disk.
const (
	journalMovingOld = "moving-old"
	journalMovingNew = "moving-new"
	journalSwitched  = "switched"
)

type journalState struct {
	Version int    `json:"version"`
	Target  string `json:"target"`
	Staging string `json:"staging"`
	Backup  string `json:"backup"`
	Phase   string `json:"phase"`
}

// switchJournaled replaces the target using ordinary renames driven by a
// durable journal. Observers may briefly find the target name absent
// during the sequence, but every state transition is recorded in the
// journal so a crash can be rolled back to the complete old version (or
// completed to the new version if the point of no return was passed).
//
// staged is the prepared tree, already located next to the target.
func (ws *WriteSet) switchJournaled(ctx context.Context, staged string) (Phase, error) {
	if err := ctx.Err(); err != nil {
		return PhasePrepare, err
	}
	target := ws.target
	parent := filepath.Dir(target)
	backup := backupPath(target)
	journal := journalPath(target)

	state := journalState{
		Version: journalVersion,
		Target:  target,
		Staging: staged,
		Backup:  backup,
		Phase:   journalMovingOld,
	}

	// 1. Move the old version aside.
	if err := writeJournal(journal, state); err != nil {
		_ = os.RemoveAll(staged)
		return PhaseSwitch, err
	}
	if ws.testHook != nil {
		ws.testHook("before-move-old")
	}
	if err := os.Rename(target, backup); err != nil {
		_ = os.Remove(journal)
		_ = os.RemoveAll(staged)
		return PhaseSwitch, err
	}
	_ = syncDir(parent)
	if ws.testHook != nil {
		ws.testHook("after-move-old")
	}

	// 2. Move the new version into place.
	state.Phase = journalMovingNew
	if err := writeJournal(journal, state); err != nil {
		if rerr := ws.restoreBackupWs(target, backup); rerr != nil {
			return PhaseRollback, rerr
		}
		_ = os.Remove(journal)
		_ = os.RemoveAll(staged)
		_ = syncDir(parent)
		return PhaseSwitch, err
	}
	if err := os.Rename(staged, target); err != nil {
		if rerr := ws.restoreBackupWs(target, backup); rerr != nil {
			return PhaseRollback, rerr
		}
		_ = os.Remove(journal)
		_ = os.RemoveAll(staged)
		_ = syncDir(parent)
		return PhaseSwitch, err
	}
	_ = syncDir(parent)

	// Point of no return: target holds the complete new version.
	state.Phase = journalSwitched
	_ = writeJournal(journal, state) // best effort; recovery handles the missing update
	_ = os.RemoveAll(backup)
	_ = os.Remove(journal)
	_ = os.Remove(journal + ".tmp")
	_ = syncDir(parent)
	return "", nil
}

// restoreBackup puts the old version back in place after an interrupted
// move. It is a no-op when the target name is already occupied.
func restoreBackup(target, backup string) error {
	if _, err := os.Lstat(target); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Rename(backup, target); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// restoreBackupWs wraps the free function with the test-only failure
// injection point.
func (ws *WriteSet) restoreBackupWs(target, backup string) error {
	if ws.testRollbackErr != nil {
		return ws.testRollbackErr
	}
	return restoreBackup(target, backup)
}

func writeJournal(journal string, state journalState) error {
	data, err := json.MarshalIndent(&state, "", " ")
	if err != nil {
		return err
	}
	// WriteFile itself is atomic; the extra rename makes the journal
	// update atomic and the parent sync makes it durable.
	if err := WriteFile(journal+".tmp", data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(journal+".tmp", journal); err != nil {
		return err
	}
	return syncDir(filepath.Dir(journal))
}

func readJournal(journal string) (journalState, error) {
	var state journalState
	data, err := os.ReadFile(journal)
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, err
	}
	return state, nil
}

// Recover reconciles leftovers of an interrupted replacement transaction
// against target. It is normally not necessary to call it: opening a write
// set with [NewWriteSetForReplace] performs the same recovery. It is
// provided for processes that only need to repair a directory without
// preparing a new version.
//
// Depending on the recorded state, the original directory is restored or
// the completed new directory is kept; backups, journals and staging
// directories belonging to this package are removed. Files and directories
// not created by this package are never touched.
func Recover(target string) error {
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return &PhaseError{Phase: PhaseRecover, Err: err}
	}
	lock, err := acquireTxnLock(lockPath(absTarget))
	if err != nil {
		return &PhaseError{Phase: PhaseRecover, Err: err}
	}
	defer lock.Close()

	if err := recoverLocked(absTarget); err != nil {
		return &PhaseError{Phase: PhaseRecover, Err: err}
	}
	return nil
}

// recoverLocked implements recovery. The caller must hold the target lock.
func recoverLocked(target string) error {
	parent := filepath.Dir(target)
	journal := journalPath(target)
	backup := backupPath(target)

	if fi, err := os.Lstat(journal); err == nil && !fi.IsDir() {
		state, err := readJournal(journal)
		if err != nil {
			return err
		}
		if err := recoverFromJournal(target, backup, state); err != nil {
			return err
		}
		if state.Staging != "" {
			_ = os.RemoveAll(state.Staging)
		}
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	} else {
		// No journal: either an exchange-mode leftover (the old version
		// parked under a staging-prefixed name) or a stale staging dir.
		if fi, err := os.Lstat(backup); err == nil && fi.IsDir() {
			// The fixed-suffix backup only exists in journal mode, where
			// the journal would have named it; reaching here means an
			// interrupted state outside the journal: restore defensively
			// only when the target is missing.
			if _, terr := os.Lstat(target); errors.Is(terr, fs.ErrNotExist) {
				if err := os.Rename(backup, target); err != nil {
					return err
				}
			} else if terr == nil {
				_ = os.RemoveAll(backup)
			}
		}
	}

	// Sweep every staging-prefixed directory for this target, including
	// the old-version directory left by a post-exchange crash. The prefix
	// is package-specific, so unrelated content is never removed.
	if err := sweepStaging(parent, target); err != nil {
		return err
	}

	// Remove journal artifacts that may remain in any recovery path.
	for _, p := range []string{journal, journal + ".tmp"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return syncDir(parent)
}

func recoverFromJournal(target, backup string, state journalState) error {
	switch state.Phase {
	case journalMovingOld:
		// The old version was being moved to the backup. If the target
		// name is still occupied the move did not happen; otherwise
		// restore the backup.
		if err := restoreBackup(target, backup); err != nil {
			return err
		}
		_ = os.RemoveAll(backup)
	case journalMovingNew, journalSwitched:
		// Either the new version made it into the target, in which case
		// the backup is stale, or it did not and the old version must be
		// restored.
		fi, err := os.Lstat(target)
		switch {
		case err == nil && fi.IsDir():
			_ = os.RemoveAll(backup)
		case errors.Is(err, fs.ErrNotExist):
			if err := os.Rename(backup, target); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		case err != nil:
			return err
		}
	default:
		return fmt.Errorf("unknown journal phase %q", state.Phase)
	}
	return nil
}

func sweepStaging(parent, target string) error {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return err
	}
	prefix := stagingPattern(target)
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), prefix) {
			if err := os.RemoveAll(filepath.Join(parent, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// syncTree fsyncs every directory of the staged tree, deepest first, so
// all file entries created in the set are durable before the switch.
// Individual files are already synced when their writers are closed.
func syncTree(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := syncDir(dirs[i]); err != nil {
			return err
		}
	}
	return nil
}

// copyTree recursively copies src into dst, preserving directory and file
// modes and symbolic links. It is only used to move a staged tree across
// volumes, before the target directory is touched.
func copyTree(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if err := os.Mkdir(dst, info.Mode().Perm()); err != nil {
		return err
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		sp := filepath.Join(src, entry.Name())
		dp := filepath.Join(dst, entry.Name())

		switch {
		case entry.IsDir():
			if err := copyTree(sp, dp); err != nil {
				return err
			}
		case entry.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(sp)
			if err != nil {
				return err
			}
			if err := os.Symlink(link, dp); err != nil {
				return err
			}
		default:
			fi, err := entry.Info()
			if err != nil {
				return err
			}
			if fi.Mode()&os.ModeType != 0 {
				return &fs.PathError{Op: "copy", Path: sp, Err: fmt.Errorf("unsupported file type %v", fi.Mode().Type())}
			}
			if err := copyFile(sp, dp, fi.Mode().Perm()); err != nil {
				return err
			}
		}
	}
	return syncDir(dst)
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	out, err := sequential.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		_ = in.Close()
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = in.Close()
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = in.Close()
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		_ = in.Close()
		return err
	}
	return in.Close()
}
