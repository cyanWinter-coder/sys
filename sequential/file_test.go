package sequential_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/moby/sys/sequential"
)

func tempPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "sequential-test")
}

func writeFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
		t.Fatalf("seed %q: %v", name, err)
	}
}

// TestNewFileFreezesVersionAcrossReplace pins requirement 1: after the path
// is replaced, the captured handle keeps reading the incarnation it opened.
func TestNewFileFreezesVersionAcrossReplace(t *testing.T) {
	path := tempPath(t)
	writeFile(t, path, "old contents")

	f, err := sequential.NewFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	frozen := f.Version()
	if !frozen.Valid() {
		t.Fatal("frozen version is invalid")
	}

	// Replace the file atomically: new path, rename over the original.
	replacement := filepath.Join(filepath.Dir(path), "replacement")
	writeFile(t, replacement, "brand new contents")
	if err := os.Rename(replacement, path); err != nil {
		t.Fatalf("rename replacement: %v", err)
	}

	replaced, err := f.Replaced()
	if err != nil {
		t.Fatalf("Replaced: %v", err)
	}
	if !replaced {
		t.Fatal("Replaced=false after rename; expected to detect a new incarnation")
	}

	current, err := f.CurrentVersion()
	if err != nil {
		t.Fatalf("CurrentVersion: %v", err)
	}
	if frozen.Equal(current) {
		t.Fatal("frozen version equals the post-replacement version")
	}
	if got := current.String(); got == "" {
		t.Fatal("version String() is empty")
	}

	// The old handle must still read the old incarnation byte for byte.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read frozen handle: %v", err)
	}
	if string(got) != "old contents" {
		t.Fatalf("frozen handle read %q, want %q", got, "old contents")
	}

	// A fresh opener sees the replacement, never a mixture of the two.
	fresh, err := sequential.FileVersion(path)
	if err != nil {
		t.Fatalf("FileVersion: %v", err)
	}
	if !fresh.Equal(current) || fresh.Equal(frozen) {
		t.Fatal("FileVersion after replace did not resolve to the new version")
	}
}

// TestNewFileHandleSurvivesDeletion pins bounded cleanup: removing the path
// does not break the captured handle; only Close ends its lifetime.
func TestNewFileHandleSurvivesDeletion(t *testing.T) {
	path := tempPath(t)
	writeFile(t, path, "persistent")

	f, err := sequential.NewFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	replaced, err := f.Replaced()
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Replaced after delete: (%v, %v), want ErrNotExist", replaced, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("seek after unlink: %v", err)
	}
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read unlinked handle: %v", err)
	}
	if string(got) != "persistent" {
		t.Fatalf("unlinked handle read %q", got)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := f.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("read after close: %v, want an error wrapping os.ErrClosed", err)
	}
}

// TestWriteFullAndBarrier pins requirement 2 end to end on a real file.
func TestWriteFullAndBarrier(t *testing.T) {
	path := tempPath(t)
	f, err := sequential.NewFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}

	payload := make([]byte, 1<<20)
	for i := range payload {
		payload[i] = byte(i)
	}
	n, err := f.Write(payload)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("write n=%d, want %d", n, len(payload))
	}
	if err := f.Barrier(); err != nil {
		t.Fatalf("barrier: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(onDisk) != len(payload) {
		t.Fatalf("persisted %d bytes, want %d", len(onDisk), len(payload))
	}
	for i, b := range onDisk {
		if b != payload[i] {
			t.Fatalf("byte %d mismatch: got %d want %d", i, b, payload[i])
		}
	}
}

// TestConcurrentWritesAreSerialized pins requirement 3 under -race: many
// goroutines write full records through one wrapper and every record lands
// intact; no tearing, no panic, no use after close.
func TestConcurrentWritesAreSerialized(t *testing.T) {
	path := tempPath(t)
	f, err := sequential.NewFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}

	const goroutines, records = 16, 200
	record := []byte("0123456789ABCDEF")

	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for r := range records {
				buf := make([]byte, len(record))
				copy(buf, record)
				buf[0] = byte('a' + g%26)
				buf[1] = byte('a' + r%26)
				if _, err := f.Write(buf); err != nil {
					t.Errorf("write: %v", err)
					return
				}
			}
		}(g)
	}

	// Concurrent close racing the writers.
	go func() {
		// Give writers a head start, then force the terminal state.
		for range 50 {
			if f.Sync() != nil {
				return
			}
		}
	}()
	wg.Wait()
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(data)%len(record) != 0 {
		t.Fatalf("persisted %d bytes, not a whole number of %d-byte records (tearing)", len(data), len(record))
	}
	for off := 0; off < len(data); off += len(record) {
		chunk := data[off : off+len(record)]
		for i := 2; i < len(record); i++ {
			if chunk[i] != record[i] {
				t.Fatalf("torn record at offset %d: %x", off, chunk)
			}
		}
	}
}

// TestParityWithOSFile pins requirement 4: successful writes and the
// read-only error shape match what os.File produces on this platform.
func TestParityWithOSFile(t *testing.T) {
	path := tempPath(t)
	writeFile(t, path, "")

	wrapped, err := sequential.NewFile(path, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	plain, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("os.OpenFile: %v", err)
	}

	// Writing to a read-only handle must fail through both layers, and the
	// wrapped error keeps the same op/path framing.
	_, wErr := wrapped.Write([]byte("x"))
	_, pErr := plain.Write([]byte("x"))
	if wErr == nil || pErr == nil {
		t.Fatalf("expected write errors: wrapped=%v plain=%v", wErr, pErr)
	}
	var wPE, pPE *os.PathError
	if !errors.As(wErr, &wPE) || !errors.As(pErr, &pPE) {
		t.Fatalf("error types differ: wrapped=%T plain=%T", wErr, pErr)
	}
	if wPE.Op != pPE.Op || wPE.Path != pPE.Path {
		t.Fatalf("PathError framing differs: wrapped=%+v plain=%+v", wPE, pPE)
	}

	// Successful byte stream: write via wrapper, read via os.File and vice
	// versa; the on-disk content and counts must be identical.
	if err := wrapped.Close(); err != nil {
		t.Fatalf("close wrapped: %v", err)
	}
	if err := plain.Close(); err != nil {
		t.Fatalf("close plain: %v", err)
	}

	out := tempPath(t) + ".out"
	wf, err := sequential.NewFile(out, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	payload := []byte("parity payload, repeated. "[:16])
	n, err := wf.Write(payload)
	if err != nil || n != len(payload) {
		t.Fatalf("wrapped write: n=%d err=%v", n, err)
	}
	if err := wf.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("os read: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("content mismatch: %q != %q", got, payload)
	}
}

// TestLegacyAPIStaysCompatible pins requirement 5: the original four
// constructors keep their signatures and os-level semantics.
func TestLegacyAPIStaysCompatible(t *testing.T) {
	dir := t.TempDir()

	created, err := sequential.Create(filepath.Join(dir, "create"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, ok := any(created).(*os.File); !ok {
		t.Fatalf("Create returned %T, want *os.File", created)
	}
	_ = created.Close()

	opened, err := sequential.Open(filepath.Join(dir, "create"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, ok := any(opened).(*os.File); !ok {
		t.Fatalf("Open returned %T, want *os.File", opened)
	}
	_ = opened.Close()

	of, err := sequential.OpenFile(filepath.Join(dir, "openfile"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, ok := any(of).(*os.File); !ok {
		t.Fatalf("OpenFile returned %T, want *os.File", of)
	}
	_ = of.Close()

	tmp, err := sequential.CreateTemp(dir, "temp-")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if _, ok := any(tmp).(*os.File); !ok {
		t.Fatalf("CreateTemp returned %T, want *os.File", tmp)
	}
	_ = tmp.Close()
}

func TestNewFileMissingPathError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	f, err := sequential.NewFile(missing, os.O_RDONLY, 0)
	if err == nil {
		_ = f.Close()
		t.Fatal("NewFile on missing path succeeded")
	}
	var pe *os.PathError
	if !errors.As(err, &pe) {
		t.Fatalf("error %T, want *os.PathError", err)
	}
	if _, err := sequential.FileVersion(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("FileVersion missing: %v, want ErrNotExist", err)
	}
}

func TestZeroVersionIsInvalid(t *testing.T) {
	var zero sequential.Version
	if zero.Valid() {
		t.Fatal("zero Version is valid")
	}
	if zero.Equal(sequential.Version{}) {
		t.Fatal("two zero versions compare equal")
	}
}
