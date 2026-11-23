package reexec

import (
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// maxCapture bounds how much of a child's output is retained on [Result].
const maxCapture = 64 << 10

// streamForwarder owns the child's stdout and stderr.
//
// The child writes to pipes created here. That ownership is what makes the
// drain bounded: after the child exits, [streamForwarder.drain] waits at most
// the termination timeout for the pipes to reach EOF, then closes their read
// ends to unblock the readers. A grandchild that inherits the child's
// descriptors would otherwise keep the parent blocked indefinitely (see
// https://go.dev/issue/23019). [exec.Cmd.StdoutPipe] is not used because it
// hands the wait to [exec.Cmd.Wait], which closes the pipes as soon as the
// child exits and so cannot report that the drain timed out.
//
// A bounded prefix of each stream is retained on the [Result], so the outcome
// of a re-exec is inspectable without racing separate pipes against the exit
// status.
type streamForwarder struct {
	writers []*os.File // child ends, closed in the parent after Start

	readers []*os.File
	drained sync.WaitGroup

	mu     sync.Mutex
	closed bool
	muOut  sync.Mutex
	muErr  sync.Mutex
	outBuf []byte
	errBuf []byte
}

func newStreamForwarder(cmd *exec.Cmd) (*streamForwarder, error) {
	f := &streamForwarder{}
	for _, s := range []struct {
		set  func(io.Writer)
		copy func(io.Reader)
	}{
		{func(w io.Writer) { cmd.Stdout = w }, f.copyOut},
		{func(w io.Writer) { cmd.Stderr = w }, f.copyErr},
	} {
		pr, pw, err := os.Pipe()
		if err != nil {
			f.closePipes()
			return nil, err
		}
		s.set(pw)
		f.writers = append(f.writers, pw)
		f.readers = append(f.readers, pr)
		f.drained.Add(1)
		go func(copyFn func(io.Reader), src *os.File) {
			defer f.drained.Done()
			copyFn(src)
		}(s.copy, pr)
	}
	return f, nil
}

// started closes the parent's copies of the child ends. It must be called after
// a successful [exec.Cmd.Start]; until then the child ends must stay open.
func (f *streamForwarder) started() {
	for _, w := range f.writers {
		_ = w.Close()
	}
	f.writers = nil
}

// drain waits up to timeout for the readers to observe EOF. When the timeout
// expires, the read ends are closed to unblock them and drain reports true,
// meaning the child's output was not fully drained.
func (f *streamForwarder) drain(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		f.drained.Wait()
		close(done)
	}()

	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-done:
			return false
		case <-timer.C:
		}
	}

	f.closeReaders()
	<-done
	return true
}

// closeReaders closes the read ends to unblock any reader still waiting on a
// descriptor held open by another process.
func (f *streamForwarder) closeReaders() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	readers := f.readers
	f.mu.Unlock()

	for _, r := range readers {
		_ = r.Close()
	}
}

// closePipes closes both ends. It is used when Start fails, before any goroutine
// depends on the pipes.
func (f *streamForwarder) closePipes() {
	for _, w := range f.writers {
		_ = w.Close()
	}
	for _, r := range f.readers {
		_ = r.Close()
	}
	f.writers, f.readers = nil, nil
}

func (f *streamForwarder) copyOut(src io.Reader) {
	f.copyStream(src, &f.muOut, &f.outBuf)
}

func (f *streamForwarder) copyErr(src io.Reader) {
	f.copyStream(src, &f.muErr, &f.errBuf)
}

func (f *streamForwarder) copyStream(src io.Reader, mu *sync.Mutex, buf *[]byte) {
	b := make([]byte, 32<<10)
	for {
		n, err := src.Read(b)
		if n > 0 {
			mu.Lock()
			if len(*buf) < maxCapture {
				room := maxCapture - len(*buf)
				if room > n {
					room = n
				}
				*buf = append(*buf, b[:room]...)
			}
			mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// output returns a copy of the retained output prefixes.
func (f *streamForwarder) output() (stdout, stderr []byte) {
	if f == nil {
		return nil, nil
	}
	f.muOut.Lock()
	stdout = append([]byte(nil), f.outBuf...)
	f.muOut.Unlock()
	f.muErr.Lock()
	stderr = append([]byte(nil), f.errBuf...)
	f.muErr.Unlock()
	return stdout, stderr
}
