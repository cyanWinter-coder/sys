package reexec_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/sys/reexec"
)

// testEntrypointName returns a name unique to the calling (sub)test and run, so
// tests that register entrypoints cannot collide with each other, with the
// package-level entrypoints, or with a previous `go test -count=N` iteration
// (the registry is process-global and outlives a test function).
func testEntrypointName(t *testing.T, suffix string) string {
	t.Helper()
	return "test-" + strings.ReplaceAll(t.Name(), "/", "-") + "-" + strconv.FormatInt(registryRun.Add(1), 10) + "-" + suffix
}

// registryRun makes test entrypoint names unique across repeated test runs.
var registryRun atomic.Int64

func TestResolve(t *testing.T) {
	noop := func(context.Context) error { return nil }

	name := testEntrypointName(t, "resolve")
	if err := reexec.RegisterEntrypoint(name, noop); err != nil {
		t.Fatalf("RegisterEntrypoint: %v", err)
	}

	tests := []struct {
		doc   string
		argv0 string
		want  string // resolved name; empty means an error is expected
		err   error
	}{
		{doc: "basename", argv0: name, want: name},
		{doc: "path", argv0: filepath.Join("some", "dir", name), want: name},
		{doc: "absolute path", argv0: filepath.Join(string(filepath.Separator), "usr", "bin", name), want: name},
		{doc: "missing", argv0: "test-not-registered-at-all", err: reexec.ErrNotRegistered},
		{doc: "empty", argv0: "", err: reexec.ErrInvalidName},
	}

	for _, tc := range tests {
		t.Run(tc.doc, func(t *testing.T) {
			got, err := reexec.Resolve(tc.argv0)
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("got error %v, want %v", err, tc.err)
				}
				if got.Valid() {
					t.Errorf("expected invalid Entrypoint, got %v", got.Name())
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tc.argv0, err)
			}
			if got.Name() != tc.want {
				t.Errorf("got name %q, want %q", got.Name(), tc.want)
			}
			if !got.Valid() {
				t.Error("expected a valid Entrypoint")
			}
		})
	}
}

func TestResolveUniqueHandler(t *testing.T) {
	// Two distinct entrypoints must never resolve to each other, regardless of
	// the directory their argv[0] happens to be in.
	first := testEntrypointName(t, "first")
	second := testEntrypointName(t, "second")
	if err := reexec.RegisterAll(
		reexec.Entry{Name: first, Entrypoint: func(context.Context) error { return nil }},
		reexec.Entry{Name: second, Entrypoint: func(context.Context) error { return nil }},
	); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}

	for _, name := range []string{first, second} {
		for _, argv0 := range []string{name, "./" + name, "/bin/" + name} {
			got, err := reexec.Resolve(argv0)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", argv0, err)
			}
			if got.Name() != name {
				t.Errorf("Resolve(%q) = %q, want %q", argv0, got.Name(), name)
			}
		}
	}
}

func TestRegisterEntrypointErrors(t *testing.T) {
	noop := func(context.Context) error { return nil }

	existing := testEntrypointName(t, "existing")
	if err := reexec.RegisterEntrypoint(existing, noop); err != nil {
		t.Fatalf("RegisterEntrypoint: %v", err)
	}

	tests := []struct {
		doc         string
		name        string
		fn          func(context.Context) error
		expectedErr error
	}{
		{doc: "empty name", name: "", fn: noop, expectedErr: reexec.ErrInvalidName},
		{doc: "path name", name: filepath.Join("a", "b"), fn: noop, expectedErr: reexec.ErrInvalidName},
		{doc: "nil entrypoint", name: testEntrypointName(t, "nil"), fn: nil, expectedErr: reexec.ErrNilEntrypoint},
		{doc: "duplicate", name: existing, fn: noop, expectedErr: reexec.ErrAlreadyRegistered},
	}

	for _, tc := range tests {
		t.Run(tc.doc, func(t *testing.T) {
			err := reexec.RegisterEntrypoint(tc.name, tc.fn)
			if !errors.Is(err, tc.expectedErr) {
				t.Fatalf("got %v, want %v", err, tc.expectedErr)
			}
			// A failed registration must leave the registry unchanged.
			if tc.expectedErr == reexec.ErrAlreadyRegistered {
				got, err := reexec.Resolve(existing)
				if err != nil || !got.Valid() {
					t.Fatalf("existing entrypoint lost after failed registration: %v", err)
				}
			}
		})
	}
}

func TestRegisterAllAtomic(t *testing.T) {
	noop := func(context.Context) error { return nil }
	first := testEntrypointName(t, "batch-first")
	second := testEntrypointName(t, "batch-second")
	blocked := testEntrypointName(t, "batch-blocked")
	if err := reexec.RegisterEntrypoint(blocked, noop); err != nil {
		t.Fatalf("RegisterEntrypoint: %v", err)
	}

	// The batch fails on its last element, so nothing (including the valid
	// prefix) may be installed.
	err := reexec.RegisterAll(
		reexec.Entry{Name: first, Entrypoint: noop},
		reexec.Entry{Name: second, Entrypoint: noop},
		reexec.Entry{Name: blocked, Entrypoint: noop},
	)
	if !errors.Is(err, reexec.ErrAlreadyRegistered) {
		t.Fatalf("got %v, want ErrAlreadyRegistered", err)
	}
	for _, name := range []string{first, second} {
		if _, err := reexec.Resolve(name); !errors.Is(err, reexec.ErrNotRegistered) {
			t.Errorf("%q was installed despite a failed batch: %v", name, err)
		}
	}

	// A duplicate within the batch is rejected before any name is installed.
	dup := testEntrypointName(t, "batch-dup")
	err = reexec.RegisterAll(
		reexec.Entry{Name: dup, Entrypoint: noop},
		reexec.Entry{Name: dup, Entrypoint: noop},
	)
	if !errors.Is(err, reexec.ErrAlreadyRegistered) {
		t.Fatalf("got %v, want ErrAlreadyRegistered", err)
	}
	if _, err := reexec.Resolve(dup); !errors.Is(err, reexec.ErrNotRegistered) {
		t.Errorf("%q was installed despite a duplicate batch: %v", dup, err)
	}
}

func TestStartRejectsUnregistered(t *testing.T) {
	_, err := reexec.Start(t.Context(), reexec.Options{}, "test-definitely-not-registered")
	if !errors.Is(err, reexec.ErrNotRegistered) {
		t.Fatalf("got %v, want ErrNotRegistered", err)
	}
	_, err = reexec.Start(t.Context(), reexec.Options{})
	if !errors.Is(err, reexec.ErrNotRegistered) {
		t.Fatalf("got %v, want ErrNotRegistered for empty args", err)
	}
}

// requireUnix skips a test that relies on POSIX signals and wait statuses.
func requireUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" || runtime.GOOS == "plan9" {
		t.Skipf("%s: POSIX wait statuses are not available", runtime.GOOS)
	}
}

func TestRunExitStatus(t *testing.T) {
	res := reexec.Run(t.Context(), reexec.Options{}, testReExecExit)
	want := testReExecExitCode
	if res.Err != nil {
		t.Fatalf("unexpected error: %v (result: %v)", res.Err, res)
	}
	if !res.Started() {
		t.Fatal("child was not reported as started")
	}
	if res.Signaled() {
		t.Fatalf("child unexpectedly reported as signaled: %v", res)
	}
	if res.ExitCode != want || res.ExitStatus() != want {
		t.Errorf("got exit code %d / status %d, want %d", res.ExitCode, res.ExitStatus(), want)
	}
	if res.Success() {
		t.Error("Success() = true for a non-zero exit")
	}
}

func TestRunSignalStatus(t *testing.T) {
	requireUnix(t)

	res := reexec.Run(t.Context(), reexec.Options{TerminationTimeout: 5 * time.Second}, testReExecSignal)
	if !res.Started() {
		t.Fatalf("child was not reported as started: %v", res)
	}
	if !res.Signaled() {
		t.Fatalf("expected a signal result, got %v", res)
	}
	if got, want := res.Signal, wantSelfTerminateSignal(); got != want {
		t.Errorf("got signal %v, want %v", got, want)
	}
	if res.ExitCode != -1 || res.ExitStatus() != -1 {
		t.Errorf("got exit code %d, want -1 for a signaled child", res.ExitCode)
	}
	if res.Err != nil {
		t.Errorf("unexpected error for a self-signaled child: %v", res.Err)
	}
}

func TestRunPublishesOneResult(t *testing.T) {
	p, err := reexec.Start(t.Context(), reexec.Options{}, testReExecExit)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	first := p.Wait()
	select {
	case <-p.Done():
	default:
		t.Fatal("Done was not closed after Wait returned")
	}

	// A second Wait (and any concurrent waiter) must observe the same value,
	// not a re-wait that could fail with "Wait was already called".
	const readers = 8
	results := make(chan reexec.Result, readers)
	for range readers {
		go func() { results <- p.Wait() }()
	}
	for range readers {
		got := <-results
		// Result is comparable except for the captured output slices; compare
		// the status fields to prove every waiter observed the same terminal
		// state.
		if got.PID != first.PID || got.ExitCode != first.ExitCode || got.Signal != first.Signal ||
			got.Err != first.Err || got.Canceled != first.Canceled || got.Stopped != first.Stopped ||
			got.WaitDelay != first.WaitDelay {
			t.Errorf("concurrent Wait returned %v, want %v", got, first)
		}
	}
}

func TestRunCancelTerminatesChild(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	p, err := reexec.Start(ctx, reexec.Options{TerminationTimeout: 5 * time.Second}, testReExecWait)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Let the child start and install its signal handling, then cancel.
	time.Sleep(100 * time.Millisecond)
	cancel()

	res := p.Wait()
	if !res.Started() {
		t.Fatalf("child was not reported as started: %v", res)
	}
	if !res.Canceled {
		t.Errorf("Canceled = false, want true (result: %v)", res)
	}
	if !errors.Is(res.Err, context.Canceled) {
		t.Errorf("got Err %v, want context.Canceled", res.Err)
	}
}

func TestRunForceKillsIgnoredTermination(t *testing.T) {
	requireUnix(t)

	// The child ignores SIGTERM, so cancellation must fall back to SIGKILL
	// after the grace period rather than waiting forever.
	ctx, cancel := context.WithCancel(t.Context())
	p, err := reexec.Start(ctx, reexec.Options{TerminationTimeout: 200 * time.Millisecond}, testReExecIgnoreTerm)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	time.Sleep(200 * time.Millisecond)
	cancel()

	done := make(chan reexec.Result, 1)
	go func() { done <- p.Wait() }()
	select {
	case res := <-done:
		if !res.Started() {
			t.Fatalf("child not reported as started: %v", res)
		}
		if !res.Signaled() {
			t.Errorf("expected the child to be killed by a signal, got %v", res)
		}
		if want := wantKillSignal(); res.Signal != want {
			t.Errorf("got signal %v, want %v", res.Signal, want)
		}
		if !errors.Is(res.Err, reexec.ErrTerminated) {
			t.Errorf("got Err %v, want ErrTerminated after the grace period expired", res.Err)
		}
		if !res.Canceled {
			t.Errorf("Canceled = false, want true (result: %v)", res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("bounded termination did not complete")
	}
}

func TestRunDrainsHeldStream(t *testing.T) {
	requireUnix(t)

	// The child exits at once but leaves a grandchild holding its stdout (and,
	// with the "ignore" mode, its stderr), open. Without a bounded drain the
	// parent would block in Wait until the grandchild exits.
	start := time.Now()
	res := reexec.Run(t.Context(), reexec.Options{TerminationTimeout: 300 * time.Millisecond}, testReExecHoldStream, holdStreamUse)
	elapsed := time.Since(start)

	if !res.Started() {
		t.Fatalf("child was not reported as started: %v", res)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Run took %v, expected a bounded drain", elapsed)
	}
	// The child itself exited 0; only the drain timed out, which is reported
	// separately so the real exit status is not lost.
	if res.ExitCode != 0 || res.Signaled() {
		t.Errorf("child exit status lost: %v", res)
	}
	if res.Success() {
		t.Errorf("Success() = true despite an incomplete drain: %v", res)
	}
	if !strings.Contains(string(res.Stdout), "child-hold-stream") {
		t.Errorf("stdout not captured: %q", res.Stdout)
	}
	// The child exited normally, but the drain had to force-close the pipe.
	if !res.WaitDelay {
		t.Errorf("WaitDelay = false, want true for a stream held open: %v", res)
	}
	if !errors.Is(res.Err, exec.ErrWaitDelay) {
		t.Errorf("got Err %v, want exec.ErrWaitDelay", res.Err)
	}
}

func TestStopTerminatesChild(t *testing.T) {
	requireUnix(t)

	p, err := reexec.Start(t.Context(), reexec.Options{TerminationTimeout: 5 * time.Second}, testReExecWait)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	res := p.Wait()
	if !res.Started() {
		t.Fatalf("child not reported as started: %v", res)
	}
	if !res.Stopped {
		t.Errorf("Stopped = false, want true (result: %v)", res)
	}
	// Stop is not cancellation; the context is still live.
	if res.Canceled {
		t.Errorf("Canceled = true for a Stop-terminated child: %v", res)
	}
	if !res.Signaled() {
		t.Errorf("expected the child to die of a signal, got %v", res)
	}

	// Stop after the child is gone must be a no-op.
	if err := p.Stop(); err != nil {
		t.Errorf("second Stop: %v", err)
	}
}

func TestStopIsIdempotentAndConcurrent(t *testing.T) {
	p, err := reexec.Start(t.Context(), reexec.Options{TerminationTimeout: 5 * time.Second}, testReExecWait)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	errs := make(chan error, 4)
	for range 4 {
		go func() { errs <- p.Stop() }()
	}
	for range 4 {
		if err := <-errs; err != nil {
			t.Errorf("Stop: %v", err)
		}
	}
	if res := p.Wait(); !res.Started() {
		t.Fatalf("child not reported as started: %v", res)
	}
}

func TestExecutableSnapshot(t *testing.T) {
	exe := reexec.Executable()
	if exe == "" {
		t.Fatal("Executable returned an empty path")
	}
	if !filepath.IsAbs(exe) {
		t.Errorf("Executable returned a relative path: %q", exe)
	}
	// The snapshot must be stable across calls and unaffected by chdir.
	if again := reexec.Executable(); again != exe {
		t.Fatalf("Executable changed between calls: %q then %q", exe, again)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if reexec.Executable() != exe {
		t.Fatalf("Executable changed after chdir: %q then %q", exe, reexec.Executable())
	}
}

func TestRunEntrypointExitCodes(t *testing.T) {
	tests := []struct {
		doc      string
		name     string
		wantCode int
		wantOut  string
	}{
		{doc: "success", name: testReExecOK, wantCode: 0, wantOut: "child-ok"},
		{doc: "plain error", name: testReExec, wantCode: reexec.ExitFailure, wantOut: "entrypoint failed"},
		{doc: "coded error", name: testReExecExitError, wantCode: testReExecExitCode, wantOut: "entrypoint failed"},
	}

	for _, tc := range tests {
		t.Run(tc.doc, func(t *testing.T) {
			res := reexec.Run(t.Context(), reexec.Options{TerminationTimeout: 5 * time.Second}, tc.name)
			if res.Err != nil {
				t.Fatalf("unexpected error: %v (%v)", res.Err, res)
			}
			if got := res.ExitStatus(); got != tc.wantCode {
				t.Errorf("got exit status %d, want %d; stdout=%q stderr=%q", got, tc.wantCode, res.Stdout, res.Stderr)
			}
			if !strings.Contains(string(res.Stdout)+string(res.Stderr), tc.wantOut) {
				t.Errorf("output %q does not contain %q", string(res.Stdout)+string(res.Stderr), tc.wantOut)
			}
		})
	}
}

func TestWithExitCode(t *testing.T) {
	cause := errors.New("boom")
	err := reexec.WithExitCode(7, cause)
	var exitErr *reexec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("got %T, want *reexec.ExitError", err)
	}
	if exitErr.Code != 7 {
		t.Errorf("got code %d, want 7", exitErr.Code)
	}
	if !errors.Is(err, cause) {
		t.Errorf("WithExitCode did not preserve the cause: %v", err)
	}

	// Out-of-range codes are rejected so a failure cannot masquerade as a
	// signal death; the original error is returned unchanged.
	plain := errors.New("plain")
	if got := reexec.WithExitCode(0, plain); got != plain {
		t.Errorf("WithExitCode(0) = %v, want the original error", got)
	}
	if got := reexec.WithExitCode(200, plain); got != plain {
		t.Errorf("WithExitCode(200) = %v, want the original error", got)
	}
}

// TestSelfSnapshotIgnoresLaterChanges verifies that the non-Linux readability
// path resolves against the environment captured at startup: changing the
// working directory the process later uses must not change what Self reports,
// because the snapshot was taken before the change.
func TestSelfSnapshotIgnoresLaterChanges(t *testing.T) {
	// On Linux Self is /proc/self/exe, which is not path-dependent; the
	// snapshot logic under test is the argv[0]-based fallback.
	if runtime.GOOS == "linux" {
		t.Skip("Self uses /proc/self/exe on Linux")
	}

	exe := reexec.Executable()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	if got := reexec.Executable(); got != exe {
		t.Errorf("Executable changed after chdir: %q, want %q", got, exe)
	}
}
