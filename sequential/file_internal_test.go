package sequential

import (
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"
)

// fakeHandle is an in-memory fileHandle used to drive the File state machine
// deterministically: short writes, zero-progress writes, injected errors and
// concurrent closes can all be arranged without touching the filesystem.
type fakeHandle struct {
	mu sync.Mutex

	buf        []byte
	pos        int64
	closed     bool
	closeCount int
	closeErr   error
	syncErr    error
	writeErr   error
	failAtN    int // fail write once this many bytes have been accepted
	failed     bool
	maxChunk   int  // cap bytes accepted per Write call, forcing short writes
	zeroWrite  bool // return (0, nil) on the next write
	statInfo   os.FileInfo
	statErr    error
}

func (h *fakeHandle) Read(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return 0, errors.New("fake: read on closed handle")
	}
	if h.pos >= int64(len(h.buf)) {
		return 0, io.EOF
	}
	n := copy(p, h.buf[h.pos:])
	h.pos += int64(n)
	return n, nil
}

func (h *fakeHandle) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return 0, errors.New("fake: write on closed handle")
	}
	if h.zeroWrite {
		h.zeroWrite = false
		return 0, nil
	}
	if h.writeErr != nil {
		return 0, h.writeErr
	}
	n := len(p)
	if h.maxChunk > 0 && n > h.maxChunk {
		n = h.maxChunk
	}
	// A bounded write may still report fewer bytes than the bound allows.
	if h.failAtN > 0 && !h.failed && int64(h.failAtN) <= h.pos+int64(n) {
		n = max(0, h.failAtN-len(h.buf))
		h.failed = true
		h.buf = append(h.buf, p[:n]...)
		h.pos += int64(n)
		return n, errors.New("fake: injected write failure")
	}
	if h.pos+int64(n) > int64(len(h.buf)) {
		h.buf = append(h.buf, make([]byte, h.pos+int64(n)-int64(len(h.buf)))...)
	}
	copy(h.buf[h.pos:h.pos+int64(n)], p[:n])
	h.pos += int64(n)
	return n, nil
}

func (h *fakeHandle) Seek(offset int64, whence int) (int64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch whence {
	case io.SeekStart:
		h.pos = offset
	case io.SeekCurrent:
		h.pos += offset
	case io.SeekEnd:
		h.pos = int64(len(h.buf)) + offset
	}
	return h.pos, nil
}

func (h *fakeHandle) Sync() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.syncErr
}

func (h *fakeHandle) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closeCount++
	h.closed = true
	return h.closeErr
}

func (h *fakeHandle) Stat() (os.FileInfo, error) {
	if h.statErr != nil {
		return nil, h.statErr
	}
	return h.statInfo, nil
}

// fakeFileInfo supplies just enough os.FileInfo for Version comparisons.
type fakeFileInfo struct {
	name string
}

func (i fakeFileInfo) Name() string       { return i.name }
func (i fakeFileInfo) Size() int64        { return 0 }
func (i fakeFileInfo) Mode() os.FileMode  { return 0o600 }
func (i fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (i fakeFileInfo) IsDir() bool        { return false }
func (i fakeFileInfo) Sys() any           { return nil }

func newFakeFile(t *testing.T, h *fakeHandle) *File {
	t.Helper()
	return &File{
		name:    "fake",
		h:       h,
		version: Version{fi: fakeFileInfo{name: "fake"}},
	}
}

func TestWriteRetriesShortWrites(t *testing.T) {
	h := &fakeHandle{maxChunk: 3}
	f := newFakeFile(t, h)

	payload := []byte("short writes must be retried until fully landed")
	n, err := f.Write(payload)
	if err != nil {
		t.Fatalf("Write returned unexpected error: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("Write returned n=%d, want %d", n, len(payload))
	}
	if string(h.buf) != string(payload) {
		t.Fatalf("stored %q, want %q", h.buf, payload)
	}
}

func TestWriteZeroProgressIsShortWrite(t *testing.T) {
	h := &fakeHandle{zeroWrite: true}
	f := newFakeFile(t, h)

	n, err := f.Write([]byte("data"))
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("expected io.ErrShortWrite, got n=%d err=%v", n, err)
	}
	if n != 0 {
		t.Fatalf("expected zero accepted bytes, got %d", n)
	}
}

func TestWriteErrorPoisonsStateChain(t *testing.T) {
	injected := errors.New("disk on fire")
	h := &fakeHandle{writeErr: injected}
	f := newFakeFile(t, h)

	if _, err := f.Write([]byte("first")); !errors.Is(err, injected) {
		t.Fatalf("expected injected error, got %v", err)
	}
	// The handle must not be touched again; the recorded error is sticky.
	if _, err := f.Write([]byte("second")); !errors.Is(err, injected) {
		t.Fatalf("expected sticky injected error, got %v", err)
	}
	if err := f.Barrier(); !errors.Is(err, injected) {
		t.Fatalf("expected barrier to surface sticky error, got %v", err)
	}
}

func TestWriteFailureMidBufferReportsAcceptedBytes(t *testing.T) {
	h := &fakeHandle{failAtN: 5, maxChunk: 4}
	f := newFakeFile(t, h)

	payload := []byte("0123456789")
	n, err := f.Write(payload)
	if err == nil {
		t.Fatal("expected an error after mid-buffer failure")
	}
	firstErr := err
	if n != len(h.buf) {
		t.Fatalf("reported %d accepted bytes but handle holds %d", n, len(h.buf))
	}
	if string(h.buf) != string(payload[:n]) {
		t.Fatalf("stored prefix %q, want %q", h.buf, payload[:n])
	}
	// After the failure the barrier is closed to further writes, and the
	// underlying handle is not touched again.
	if _, err := f.Write([]byte("more")); !errors.Is(err, firstErr) {
		t.Fatalf("expected sticky %v, got %v", firstErr, err)
	}
	if len(h.buf) != n {
		t.Fatalf("handle accepted %d extra bytes after poisoning", len(h.buf)-n)
	}
}

func TestBarrierFailurePoisonsWrites(t *testing.T) {
	syncErr := errors.New("flush failed")
	h := &fakeHandle{syncErr: syncErr}
	f := newFakeFile(t, h)

	if _, err := f.Write([]byte("before barrier")); err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}
	if err := f.Barrier(); !errors.Is(err, syncErr) {
		t.Fatalf("expected sync error from barrier, got %v", err)
	}
	if _, err := f.Write([]byte("after barrier")); !errors.Is(err, syncErr) {
		t.Fatalf("expected post-barrier write to fail with %v, got %v", syncErr, err)
	}
}

func TestCloseIsUniqueTerminalState(t *testing.T) {
	h := &fakeHandle{}
	f := newFakeFile(t, h)

	if err := f.Close(); err != nil {
		t.Fatalf("first Close failed: %v", err)
	}
	if !h.closed {
		t.Fatal("underlying handle was not closed")
	}

	// Every operation, including Close itself, now reports the same error.
	ops := map[string]func() error{
		"read":    func() error { _, err := f.Read(make([]byte, 1)); return err },
		"write":   func() error { _, err := f.Write([]byte("x")); return err },
		"seek":    func() error { _, err := f.Seek(0, io.SeekStart); return err },
		"sync":    func() error { return f.Sync() },
		"barrier": func() error { return f.Barrier() },
		"close":   func() error { return f.Close() },
	}
	for op, run := range ops {
		err := run()
		var pe *os.PathError
		if !errors.As(err, &pe) {
			t.Fatalf("%s after Close returned %T (%v), want *os.PathError", op, err, err)
		}
		if pe.Op != op {
			t.Fatalf("%s error Op=%q, want %q", op, pe.Op, op)
		}
		if pe.Path != f.name {
			t.Fatalf("%s error Path=%q, want %q", op, pe.Path, f.name)
		}
		if !errors.Is(err, errClosed) {
			t.Fatalf("%s error does not wrap errClosed: %v", op, err)
		}
	}

	// The underlying handle must see exactly one Close even under retries.
	if err := f.Close(); err == nil {
		t.Fatal("second Close unexpectedly succeeded")
	}
	if err := f.Close(); err == nil {
		t.Fatal("third Close unexpectedly succeeded")
	}
	if h.closeCount != 1 {
		t.Fatalf("underlying handle closed %d times, want exactly 1", h.closeCount)
	}
}

func TestConcurrentCloseSerializesOperations(t *testing.T) {
	h := &fakeHandle{maxChunk: 7}
	f := newFakeFile(t, h)

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = f.Write([]byte("concurrent payload"))
		}()
	}
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = f.Read(make([]byte, 4))
		}()
	}
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = f.Close()
		}()
	}
	wg.Wait()

	if !h.closed {
		t.Fatal("handle not closed after concurrent shutdown")
	}
	// Post-termination calls keep returning the canonical closed error.
	if _, err := f.Write([]byte("x")); !errors.Is(err, errClosed) {
		t.Fatalf("write after concurrent close: %v, want errClosed", err)
	}
}

func TestReadServesFrozenHandleAfterClose(t *testing.T) {
	h := &fakeHandle{}
	f := newFakeFile(t, h)
	if _, err := f.Write([]byte("frozen")); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("seek failed: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close failed: %v", err)
	}
	if _, err := f.Read(make([]byte, 2)); !errors.Is(err, errClosed) {
		t.Fatalf("read after close: %v, want errClosed", err)
	}
}
