package sequential

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// errClosed is the Err carried by the [os.PathError] returned from any
// operation attempted on a [File] after it has been closed. It is
// [os.ErrClosed] on every platform so post-close errors have exactly the
// shape callers already get from a closed [os.File], on Windows and Unix
// alike.
var errClosed = os.ErrClosed

// Version identifies a particular incarnation of a file, captured at a point
// in time. It becomes stale when the file at the observed path is replaced
// (for example by an atomic rename), which is the situation a plain path
// reopen cannot distinguish from an in-place update.
//
// The zero value is invalid and compares unequal to every other version,
// including another zero value.
type Version struct {
	fi os.FileInfo
}

// Valid reports whether v was captured from an existing file.
func (v Version) Valid() bool {
	return v.fi != nil
}

// Equal reports whether v and other refer to the same file incarnation.
// Comparison uses [os.SameFile], that is device and inode on Unix and
// volume serial number and file index on Windows, so two versions of the
// same path across an atomic replacement compare unequal.
func (v Version) Equal(other Version) bool {
	if v.fi == nil || other.fi == nil {
		return false
	}
	return os.SameFile(v.fi, other.fi)
}

// String renders a stable, human-readable description of the version. It
// deliberately does not depend on platform-specific identity fields so the
// output is the same on Windows and Unix.
func (v Version) String() string {
	if v.fi == nil {
		return "sequential.Version(invalid)"
	}
	return fmt.Sprintf(
		"sequential.Version(name=%q, size=%d, modtime=%s)",
		v.fi.Name(),
		v.fi.Size(),
		v.fi.ModTime().UTC().Format(time.RFC3339Nano),
	)
}

// FileVersion returns the current [Version] of the file at name. It is a
// point-in-time observation: call it again after an expected replacement to
// detect that the path now points at a different file.
func FileVersion(name string) (Version, error) {
	fi, err := os.Stat(name)
	if err != nil {
		return Version{}, err
	}
	return Version{fi: fi}, nil
}

// fileHandle is the subset of [os.File] the [File] state machine drives.
// Keeping it as an interface lets the state transitions, including short
// writes and injected errors, be exercised deterministically in tests.
type fileHandle interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Seek(offset int64, whence int) (int64, error)
	Close() error
	Sync() error
	Stat() (os.FileInfo, error)
}

// File wraps an opened file with a versioned, concurrency-safe state chain.
//
// The underlying file identity is frozen when the [File] is created: even if
// the path is later removed and replaced with another file, reads and writes
// keep targeting the originally opened incarnation, while [File.Replaced]
// and [File.CurrentVersion] report that the path moved on.
//
// A single mutex serializes Read, Write, Seek, Sync, Barrier and Close, so
// the wrapper has one terminal state: once Close returns, every further
// operation fails with an [os.PathError] wrapping [os.ErrClosed], and the
// underlying handle is closed exactly once.
//
// Write is a full write: short writes are retried internally and a zero
// progress write fails with [io.ErrShortWrite]. Any write or barrier
// (Sync/Barrier) failure poisons the state chain — subsequent Write calls
// return the recorded error without touching the handle — so data preceding
// a barrier is fully written before later writes are accepted.
//
// The zero value is not usable; create values with [NewFile].
type File struct {
	mu sync.Mutex

	name    string
	h       fileHandle
	version Version

	closed   bool
	writeErr error
}

// NewFile opens name with the sequential-access open path (see [OpenFile])
// and freezes the identity of the opened incarnation into the returned
// [File]. On failure no handle is leaked.
//
// The flag and perm arguments are identical to [os.OpenFile]; on Windows the
// open additionally requests FILE_FLAG_SEQUENTIAL_SCAN.
func NewFile(name string, flag int, perm os.FileMode) (*File, error) {
	h, err := openFileSequential(name, flag, perm)
	if err != nil {
		return nil, err
	}
	fi, err := h.Stat()
	if err != nil {
		_ = h.Close()
		return nil, err
	}
	return &File{
		name:    name,
		h:       h,
		version: Version{fi: fi},
	}, nil
}

// Name returns the path the file was opened with. It is safe to call at any
// time, including after Close.
func (f *File) Name() string {
	return f.name
}

// Version returns the file incarnation captured when the [File] was opened.
// It never changes for the lifetime of the [File].
func (f *File) Version() Version {
	return f.version
}

// CurrentVersion returns the version the path currently points at, which
// differs from the frozen [File.Version] after an in-place replacement.
func (f *File) CurrentVersion() (Version, error) {
	return FileVersion(f.name)
}

// Replaced reports whether the path now points at a different file than the
// incarnation the [File] was opened on. A missing path is reported as an
// error rather than as a replacement, since deletion is not a new version.
func (f *File) Replaced() (bool, error) {
	current, err := f.CurrentVersion()
	if err != nil {
		return false, err
	}
	return !f.version.Equal(current), nil
}

// Read implements [io.Reader]. Reads are serialized with every other
// operation and fail once the [File] has been closed.
func (f *File) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, f.closedErr("read")
	}
	return f.h.Read(p)
}

// Write implements [io.Writer] with full-write semantics: the whole of p is
// written, retrying short writes, before the call succeeds. A zero-progress
// write fails with [io.ErrShortWrite]; any failure, including a failed
// [File.Sync] or [File.Barrier], poisons the state chain and is returned by
// every later Write until Close, so a barrier can never be followed by
// writes that silently lost their predecessor.
//
// The returned count is len(p) on success. On error it is the number of
// bytes accepted before the failure.
func (f *File) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, f.closedErr("write")
	}
	if f.writeErr != nil {
		return 0, f.writeErr
	}

	var n int
	for n < len(p) {
		nn, err := f.h.Write(p[n:])
		n += nn
		if err != nil {
			f.writeErr = err
			return n, err
		}
		if nn <= 0 {
			f.writeErr = io.ErrShortWrite
			return n, io.ErrShortWrite
		}
	}
	return n, nil
}

// Seek implements [io.Seeker]. Seeks are serialized with every other
// operation and fail once the [File] has been closed.
func (f *File) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, f.closedErr("seek")
	}
	return f.h.Seek(offset, whence)
}

// Sync commits the current contents of the file to stable storage and acts
// as the write barrier of the state chain: all writes issued before the call
// are flushed first, and a failed Sync poisons the [File], making later
// Write calls fail with the same error until Close. If the state chain is
// already poisoned by an earlier write failure, Sync returns that recorded
// error without touching the handle: a barrier can never report success
// over unflushed or failed writes.
func (f *File) Sync() error {
	return f.sync("sync")
}

// Barrier is the explicit write barrier: it performs the same flush as
// [File.Sync] but, when the [File] is already closed, surfaces the failure
// with its own operation name instead of "sync".
func (f *File) Barrier() error {
	return f.sync("barrier")
}

func (f *File) sync(op string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return f.closedErr(op)
	}
	if f.writeErr != nil {
		return f.writeErr
	}
	if err := f.h.Sync(); err != nil {
		f.writeErr = err
		return err
	}
	return nil
}

// Close closes the underlying handle exactly once. Concurrent and repeated
// calls share one terminal state: the first one drives the handle close and
// every later operation, including further Close calls, fails with the same
// [os.PathError].
func (f *File) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return f.closedErr("close")
	}
	// Mark closed before the underlying call so a blocking or failing
	// Close still leaves the wrapper in its unique terminal state.
	f.closed = true
	return f.h.Close()
}

// closedErr builds the cross-platform error shape for post-close operations.
// The caller must hold f.mu.
func (f *File) closedErr(op string) error {
	return &os.PathError{Op: op, Path: f.name, Err: errClosed}
}
