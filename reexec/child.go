package reexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultTerminationTimeout bounds how long [Start] and [Run] let a canceled
// child run before it is killed, and how long the child's output pipes may stay
// open after it exits before they are force-closed. It is used when
// [Options.TerminationTimeout] is zero.
const DefaultTerminationTimeout = 5 * time.Second

// ErrTerminated is reported when a child was killed because it did not exit
// within its termination grace period.
var ErrTerminated = errors.New("reexec: child terminated")

// ErrWaitDelay is the cause recorded in a [Result] when the child exited but
// its output pipes were still open when the termination timeout expired, and
// had to be force-closed. It wraps [exec.ErrWaitDelay], so both errors.Is
// checks match.
var ErrWaitDelay = fmt.Errorf("reexec: output not drained: %w", exec.ErrWaitDelay)

// Result is the immutable, terminal outcome of one re-executed child process.
//
// A Result is published exactly once, after the child has exited and its
// output has been drained (or force-closed); waiting for the same child again
// returns the identical value. Callers therefore never observe a partially
// updated result, and a child killed around the same time as a normal exit is
// reported with the status the kernel actually recorded, not with an invented
// one.
//
// A non-zero exit status is not an error: it is reported through ExitCode so
// the parent never loses the child's real exit code. Err is reserved for
// failures to start the child and for termination performed by this package.
type Result struct {
	// PID is the child's process ID, or 0 if it never started.
	PID int
	// ExitCode is the child's exit status as reported by the OS. It is -1 when
	// the child was terminated by a signal or never started.
	ExitCode int
	// Signal is the signal that terminated the child, or nil if it exited
	// normally or never started.
	Signal os.Signal
	// Err reports a start failure, a cancellation-induced termination, or a
	// stream-drain timeout. It is nil for a child that ran and exited with any
	// status.
	Err error
	// Canceled reports that Err is (or wraps) the context's error.
	Canceled bool
	// Stopped reports that the child was terminated through [Process.Stop].
	Stopped bool
	// WaitDelay reports that Err is (or wraps) [ErrWaitDelay], that is, the
	// child's output pipes could not be drained within the timeout.
	WaitDelay bool

	// Stdout and Stderr hold a bounded prefix (up to 64 KiB each) of the
	// child's output. The standard streams are always captured through pipes so
	// they can be drained within the termination bound, whether or not the
	// caller also forwarded them to a writer.
	Stdout []byte
	Stderr []byte
}

// Started reports whether the child process started.
func (r Result) Started() bool { return r.PID != 0 }

// Signaled reports whether the child was terminated by a signal.
func (r Result) Signaled() bool { return r.Signal != nil }

// ExitStatus returns the child's exit code, or -1 if it was terminated by a
// signal or never started. It mirrors [os.ProcessState.ExitCode].
func (r Result) ExitStatus() int {
	if !r.Started() || r.Signaled() {
		return -1
	}
	return r.ExitCode
}

// Success reports whether the child started and exited normally with status 0.
func (r Result) Success() bool {
	return r.Started() && r.Err == nil && !r.Signaled() && r.ExitCode == 0
}

// String returns a compact description of the result.
func (r Result) String() string {
	switch {
	case !r.Started():
		if r.Err != nil {
			return "reexec: not started: " + r.Err.Error()
		}
		return "reexec: not started"
	case r.Signaled():
		return fmt.Sprintf("reexec: pid %d: signaled: %v", r.PID, r.Signal)
	default:
		return fmt.Sprintf("reexec: pid %d: exit status %d", r.PID, r.ExitCode)
	}
}

// Options configures [Start] and [Run].
type Options struct {
	// TerminationTimeout bounds the child's termination grace period and the
	// time spent draining its output after it exits. Zero means
	// [DefaultTerminationTimeout].
	TerminationTimeout time.Duration

	// Configure, if non-nil, is called with the command after it is built but
	// before it is started. It can set Dir, Env, and Stdin. The child's
	// standard output and error are always captured through pipes owned by this
	// package so their draining can be bounded; setting Stdout or Stderr is
	// therefore ignored (the output is retained on [Result] instead).
	// Cancellation behavior (Cancel and WaitDelay) is applied afterwards so
	// that it cannot be removed.
	Configure func(*exec.Cmd)
}

// Process is a started re-exec child. Its terminal [Result] is published
// exactly once, and [Process.Wait] may be called any number of times from any
// goroutine.
type Process struct {
	cmd     *exec.Cmd
	grace   time.Duration
	streams *streamForwarder
	ctx     context.Context

	done   chan struct{}
	result Result

	// stop is set by Stop before any signal is sent, so wait can record that
	// the termination was requested without racing Stop. Stop stores it before
	// waiting on done, and wait reads it only after the child has exited, so
	// the two accesses are ordered by the child's exit and the done close.
	stop atomic.Bool
	// killed is set when termination had to escalate to an unconditional kill
	// after the grace period expired.
	killed atomic.Bool

	stopOnce sync.Once
	stopErr  error
}

// Start launches a re-exec of the current binary using args (see [Command] and
// [CommandContext]) and begins collecting its terminal result.
//
// args[0] is resolved with [Resolve] before the child is started; a name that
// is not registered, or that does not reduce to a unique base name, is reported
// as an error wrapping [ErrNotRegistered] or [ErrInvalidName] without starting
// a process. This prevents a missing or ambiguous registration from silently
// running the wrong handler (or falling through to the parent's main).
//
// ctx controls cancellation: when it is done, the child is asked to terminate
// and is killed if it has not exited within the grace period. A canceled child
// is still waited for, so it is reaped and its output is drained, before the
// result is published.
func Start(ctx context.Context, opts Options, args ...string) (*Process, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("%w: no command arguments", ErrNotRegistered)
	}
	if _, err := Resolve(args[0]); err != nil {
		return nil, err
	}

	grace := opts.TerminationTimeout
	if grace == 0 {
		grace = DefaultTerminationTimeout
	}

	cmd := CommandContext(ctx, args...)
	if opts.Configure != nil {
		opts.Configure(cmd)
	}
	// Install the termination contract after Configure so a callback cannot
	// leave a child running past the context by clearing Cancel/WaitDelay.
	cmd.Cancel = func() error {
		return terminateProcess(cmd.Process)
	}
	cmd.WaitDelay = grace

	// The child writes into pipes owned here, so the parent can bound the
	// drain after the child exits (see stream.go). This replaces any writers
	// the caller set through Configure: output is still retained on the Result.
	streams, err := newStreamForwarder(cmd)
	if err != nil {
		return nil, err
	}

	stdin, err := stdinPipe(cmd)
	if err != nil {
		streams.closePipes()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		closeStdin(stdin)
		streams.closePipes()
		return nil, fmt.Errorf("reexec: starting %q: %w", args[0], err)
	}
	// The child now owns the write ends; close the parent's copies so EOF is
	// seen when the child (and any grandchild) releases them.
	streams.started()
	// Close our copy of the child's stdin once it has started: a re-exec
	// entrypoint does not normally read it, and leaving it open lets a child
	// that does read block forever on the parent's stdin. The child still sees
	// a valid, immediately-EOF stdin.
	closeStdin(stdin)

	p := &Process{
		cmd:     cmd,
		grace:   grace,
		done:    make(chan struct{}),
		streams: streams,
		ctx:     ctx,
	}
	go p.wait()
	return p, nil
}

// stdinPipe gives the child an empty stdin pipe when the caller did not set
// one, and closes it after the child has started. See Start.
func stdinPipe(cmd *exec.Cmd) (io.Closer, error) {
	if cmd.Stdin != nil {
		return nil, nil
	}
	w, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("reexec: stdin pipe: %w", err)
	}
	return w, nil
}

func closeStdin(w io.Closer) {
	if w != nil {
		_ = w.Close()
	}
}

// Run is [Start] followed by [Process.Wait]. It returns the child's terminal
// result; a child that ran and exited with a non-zero status or a signal is
// described by the result, not by an error.
func Run(ctx context.Context, opts Options, args ...string) Result {
	p, err := Start(ctx, opts, args...)
	if err != nil {
		return Result{Err: err}
	}
	return p.Wait()
}

// Pid returns the child's process ID, or 0 if it never started.
func (p *Process) Pid() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// Done returns a channel that is closed when the terminal result has been
// published. It is safe to select on while the child runs.
func (p *Process) Done() <-chan struct{} { return p.done }

// Wait blocks until the child has exited and its result has been published, then
// returns that result. Repeated and concurrent calls return the same value.
func (p *Process) Wait() Result {
	<-p.done
	return p.result
}

// Stop asks the child to terminate and returns after it has exited and its
// output has been drained, killing it if it does not exit within the
// termination grace period. It is safe to call more than once and after the
// child has already exited; in that case it is a no-op.
//
// Stop does not cancel the context passed to [Start]; it is intended for a
// parent that is shutting down. Killing the child also unblocks [Process.Wait].
func (p *Process) Stop() error {
	p.stopOnce.Do(func() {
		p.stop.Store(true)
		p.stopErr = p.terminate()
	})
	return p.stopErr
}

func (p *Process) terminate() error {
	select {
	case <-p.done:
		return nil
	default:
	}

	proc := p.cmd.Process
	if proc == nil {
		<-p.done
		return nil
	}

	err := terminateProcess(proc)
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		// A signal that cannot be delivered (for example SIGTERM on Windows)
		// falls back to an unconditional kill below.
		err = nil
	}

	if p.grace > 0 {
		timer := time.NewTimer(p.grace)
		defer timer.Stop()
		select {
		case <-p.done:
			return nil
		case <-timer.C:
		}
	}

	// The grace period expired without the child exiting; escalate to an
	// unconditional kill. Record the escalation only if the kill was actually
	// delivered, so a child that exited on its own at the boundary is not
	// reported as terminated.
	if killErr := proc.Kill(); killErr != nil {
		if !errors.Is(killErr, os.ErrProcessDone) {
			<-p.done
			return fmt.Errorf("reexec: killing pid %d: %w", proc.Pid, killErr)
		}
	} else {
		p.killed.Store(true)
	}
	<-p.done
	return err
}

// wait is the only goroutine that calls cmd.Wait; it publishes the result
// exactly once and then closes p.done, establishing the happens-before edge
// for every reader of p.result.
func (p *Process) wait() {
	waitErr := p.cmd.Wait()
	// Bound the drain: a grandchild that inherited the child's descriptors must
	// not be able to block publication of the result indefinitely. Do this
	// before publishing so a reader of the Result sees complete output.
	p.result = p.newResult(waitErr)
	if p.streams.drain(p.grace) {
		// The child's exit status is still reported through ExitCode/Signal;
		// only the API error reflects that its output could not be fully drained.
		p.result.WaitDelay = true
		if p.result.Err == nil {
			p.result.Err = ErrWaitDelay
		}
	}
	p.result.Stdout, p.result.Stderr = p.streams.output()
	close(p.done)
}

func (p *Process) newResult(waitErr error) Result {
	res := Result{}
	if ps := p.cmd.ProcessState; ps != nil {
		res.PID = ps.Pid()
		res.ExitCode = ps.ExitCode()
		if sig, ok := processSignal(ps); ok {
			res.Signal = sig
		}
	} else if p.cmd.Process != nil {
		res.PID = p.cmd.Process.Pid
	}

	// A child that dies of the termination signal reports a signal status, not
	// the context error, so cancellation is recorded from the context itself.
	if p.ctx != nil && p.ctx.Err() != nil {
		res.Canceled = true
	}

	switch {
	case waitErr == nil:
	case errors.Is(waitErr, exec.ErrWaitDelay), errors.Is(waitErr, ErrWaitDelay):
		res.Err = ErrWaitDelay
		res.WaitDelay = true
	case errors.Is(waitErr, context.Canceled), errors.Is(waitErr, context.DeadlineExceeded):
		res.Err = waitErr
	default:
		// A non-zero exit or a signal is not an API error: it is reported
		// through ExitCode and Signal so the real status is preserved.
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) {
			res.Err = waitErr
		}
	}

	// A canceled child was terminated by this package, so surface the context
	// error even when the kernel reports a signal status. When the child also
	// had to be killed after the grace period, both causes are reported so
	// errors.Is matches either.
	var causes []error
	if res.Err != nil {
		causes = append(causes, res.Err)
	}
	if res.Canceled && p.ctx != nil && p.ctx.Err() != nil {
		causes = append(causes, p.ctx.Err())
	}
	// Stopped records that the child's termination was requested through
	// Process.Stop, regardless of what the kernel finally reported.
	res.Stopped = p.stop.Load()
	if p.killed.Load() || (res.Canceled && res.Signaled() && isKillSignal(res.Signal)) {
		// The child ignored the friendly termination signal and had to be
		// killed; report that the grace period was exceeded. Cancellation is
		// escalated by exec's WaitDelay handling, so it is detected from the
		// kill signal rather than from the Stop path alone.
		causes = append(causes, ErrTerminated)
	}
	if len(causes) > 0 {
		res.Err = errors.Join(causes...)
	}
	return res
}
