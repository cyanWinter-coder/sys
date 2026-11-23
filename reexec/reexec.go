// Package reexec implements a BusyBox-style “multi-call binary” re-execution
// pattern for Go programs.
//
// Callers register named entrypoints through [Register]. When the current
// process starts, [Init] compares filepath.Base(os.Args[0]) against the set of
// registered entrypoints. If it matches, Init runs that entrypoint and returns
// true.
//
// A matched entrypoint is analogous to main for that invocation mode: callers
// typically call Init at the start of main and return immediately when it
// reports a match. [RunEntrypoint] is the equivalent that also terminates the
// process with a meaningful status, which is what [Start] and [Run] report to
// the parent.
//
// Example uses:
//
//   - For multi-call binaries: multiple symlinks/hardlinks point at one binary,
//     and argv[0] (see [execve(2)]) selects the entrypoint.
//   - For programmatic re-exec: a parent launches the current binary (via
//     [Command] or [CommandContext]) with argv[0] set to a registered entrypoint
//     name to run a specific mode.
//
// The programmatic re-exec pattern is commonly used as a safe alternative to
// fork-without-exec in Go, since the runtime is not fork-safe once multiple
// threads exist (see [os.StartProcess] and https://go.dev/issue/27505).
//
// # Child lifecycle
//
// [Command] and [CommandContext] return a bare [*exec.Cmd]. For a re-exec that
// has a well-defined terminal state, use [Start] or [Run] instead: they reject
// an unregistered or ambiguous argv[0] before starting a process, bound the
// child's termination and output draining, and publish one immutable [Result]
// holding the child's real exit code or terminating signal. On the child side,
// [RunEntrypoint] terminates the process with a status that matches the
// entrypoint's outcome.
//
// Example (multi-call binary):
//
//	package main
//
//	import (
//		"fmt"
//
//		"github.com/moby/sys/reexec"
//	)
//
//	func init() {
//		reexec.Register("example-foo", func() {
//			fmt.Println("Hello from entrypoint example-foo")
//		})
//		reexec.Register("example-bar", entrypointBar)
//	}
//
//	func main() {
//		if reexec.Init() {
//			// Matched a reexec entrypoint; stop normal main execution.
//			return
//		}
//		fmt.Println("Hello main")
//	}
//
//	func entrypointBar() {
//		fmt.Println("Hello from entrypoint example-bar")
//	}
//
// To try it:
//
//	go build -o example .
//	ln -s example example-foo
//	ln -s example example-bar
//
//	./example
//	# Hello main
//
//	./example-foo
//	# Hello from entrypoint example-foo
//
//	./example-bar
//	# Hello from entrypoint example-bar
//
// [execve(2)]: https://man7.org/linux/man-pages/man2/execve.2.html
package reexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// Registration and dispatch errors. They are returned by [RegisterEntrypoint],
// [RegisterAll], [Resolve], and [ResolveInit]; the older [Register] and
// [RegisterContext] functions panic instead, for compatibility.
var (
	// ErrNotRegistered is returned when no entrypoint is registered under the
	// requested name.
	ErrNotRegistered = errors.New("reexec: entrypoint not registered")
	// ErrInvalidName is returned when an entrypoint name is empty or contains a
	// path component that cannot be reduced to a unique basename.
	ErrInvalidName = errors.New("reexec: invalid entrypoint name")
	// ErrAlreadyRegistered is returned when a name is registered more than once.
	ErrAlreadyRegistered = errors.New("reexec: entrypoint already registered")
	// ErrNilEntrypoint is returned when a nil entrypoint function is registered.
	ErrNilEntrypoint = errors.New("reexec: nil entrypoint")
)

var (
	// entrypointsMu guards entrypoints. Registration and lookup are safe for
	// concurrent use; actual registration is expected to happen during
	// initialization so that the set of entrypoints is stable at dispatch time.
	entrypointsMu sync.RWMutex
	entrypoints   = make(map[string]func(context.Context) error)
)

// Entry is a single name/entrypoint pair for [RegisterAll].
type Entry struct {
	// Name is the entrypoint name; it must not contain a path component.
	Name string
	// Entrypoint is the function to run. It must not be nil.
	Entrypoint func(context.Context) error
}

// Entrypoint is a uniquely resolved entrypoint. The zero value is invalid; use
// [Resolve] or [ResolveInit] to obtain one.
type Entrypoint struct {
	name string
	run  func(context.Context) error
}

// Name returns the registered name of the entrypoint.
func (e Entrypoint) Name() string { return e.name }

// Valid reports whether e refers to a registered entrypoint.
func (e Entrypoint) Valid() bool { return e.run != nil }

// Run executes the entrypoint with ctx and returns its result.
//
// Run on the zero Entrypoint returns an error wrapping [ErrNotRegistered].
func (e Entrypoint) Run(ctx context.Context) error {
	if e.run == nil {
		return fmt.Errorf("%w: zero Entrypoint", ErrNotRegistered)
	}
	return e.run(ctx)
}

// Register associates name with an entrypoint function to be executed when
// the current binary is invoked with argv[0] equal to name.
//
// Register is not safe for concurrent use; entrypoints must be registered
// during program initialization, before calling [Dispatch] or [Init].
// It panics if name is empty or contains a path component, if entrypoint is
// nil, or if name is already registered. New code should prefer
// [RegisterEntrypoint], which reports the same problems as errors.
func Register(name string, entrypoint func()) {
	mustRegister(name, func(context.Context) error {
		entrypoint()
		return nil
	})
}

// RegisterContext is like [Register] but the entrypoint receives a context and
// reports success or failure by returning an error.
//
// RegisterContext has the same concurrency and panic behavior as [Register].
// New code should prefer [RegisterEntrypoint].
func RegisterContext(name string, entrypoint func(context.Context) error) {
	mustRegister(name, entrypoint)
}

// RegisterEntrypoint registers entrypoint under name. Unlike [Register] and
// [RegisterContext], it returns an error instead of panicking:
//
//   - [ErrInvalidName] if name is empty or contains a path component;
//   - [ErrNilEntrypoint] if entrypoint is nil;
//   - [ErrAlreadyRegistered] if name is already registered.
//
// Registration is atomic: either the name is registered or the registry is
// left unchanged.
func RegisterEntrypoint(name string, entrypoint func(context.Context) error) error {
	return registerEntrypoint(name, entrypoint)
}

// RegisterAll registers a batch of entrypoints atomically. Every entry is
// validated (including duplicates within the batch) before any of them is
// installed, so on error the registry is left exactly as it was and the caller
// can retry with a corrected batch.
func RegisterAll(entries ...Entry) error {
	entrypointsMu.Lock()
	defer entrypointsMu.Unlock()

	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if err := validateEntrypoint(entry.Name, entry.Entrypoint); err != nil {
			return err
		}
		if _, ok := seen[entry.Name]; ok {
			return fmt.Errorf("%w: %q appears more than once in batch", ErrAlreadyRegistered, entry.Name)
		}
		if _, ok := entrypoints[entry.Name]; ok {
			return fmt.Errorf("%w: %q", ErrAlreadyRegistered, entry.Name)
		}
		seen[entry.Name] = struct{}{}
	}
	for _, entry := range entries {
		entrypoints[entry.Name] = entry.Entrypoint
	}
	return nil
}

// Resolve resolves name to its unique entrypoint.
//
// As with [Dispatch], only the base name of name is considered, so an argv[0]
// value such as "/usr/bin/reexec-worker" resolves the entrypoint registered as
// "reexec-worker". If name has no base name (it is empty or a root), or the
// base name is not registered, Resolve reports a non-nil error wrapping
// [ErrInvalidName] or [ErrNotRegistered].
func Resolve(name string) (Entrypoint, error) {
	base := filepath.Base(name)
	if base == "" || base == "." || base == string(filepath.Separator) {
		return Entrypoint{}, fmt.Errorf("%w: %q", ErrInvalidName, name)
	}

	entrypointsMu.RLock()
	entrypoint, ok := entrypoints[base]
	entrypointsMu.RUnlock()
	if !ok {
		return Entrypoint{}, fmt.Errorf("%w: %q", ErrNotRegistered, base)
	}
	return Entrypoint{name: base, run: entrypoint}, nil
}

// ResolveInit resolves the entrypoint selected by the current process's
// argv[0], the same value [Dispatch] and [Init] use.
//
// It differs from [Dispatch] in that a process that was not invoked as a
// registered entrypoint yields an error wrapping [ErrNotRegistered] rather than
// a silent no-match. If the process has no argv[0] at all (os.Args is empty),
// ResolveInit returns an error wrapping [ErrInvalidName] instead of panicking.
func ResolveInit() (Entrypoint, error) {
	if len(os.Args) == 0 {
		return Entrypoint{}, fmt.Errorf("%w: process has no argv[0]", ErrInvalidName)
	}
	return Resolve(os.Args[0])
}

// registerEntrypoint installs entrypoint under name, returning an error for an
// invalid or duplicate registration. It never mutates the registry on error.
func registerEntrypoint(name string, entrypoint func(context.Context) error) error {
	if err := validateEntrypoint(name, entrypoint); err != nil {
		return err
	}

	entrypointsMu.Lock()
	defer entrypointsMu.Unlock()
	if _, exists := entrypoints[name]; exists {
		return fmt.Errorf("%w: %q", ErrAlreadyRegistered, name)
	}
	entrypoints[name] = entrypoint
	return nil
}

func validateEntrypoint(name string, entrypoint func(context.Context) error) error {
	if name == "" || filepath.Base(name) != name {
		return fmt.Errorf("%w: %q", ErrInvalidName, name)
	}
	if entrypoint == nil {
		return fmt.Errorf("%w: %q", ErrNilEntrypoint, name)
	}
	return nil
}

// mustRegister is the legacy panic-based wrapper used by Register and
// RegisterContext.
func mustRegister(name string, entrypoint func(context.Context) error) {
	if err := registerEntrypoint(name, entrypoint); err != nil {
		switch {
		case errors.Is(err, ErrInvalidName):
			panic(fmt.Sprintf("reexec func does not expect a path component: %q", name))
		case errors.Is(err, ErrAlreadyRegistered):
			panic(fmt.Sprintf("reexec func already registered under name %q", name))
		default:
			panic(err)
		}
	}
}

// Dispatch checks whether the current process was invoked under a registered
// name (based on filepath.Base(os.Args[0])).
//
// If a matching entrypoint is found, it is executed and Dispatch reports a
// match with ok. If ok is false, err is always nil. If ok is true, err is the
// entrypoint's return value.
//
// In the ok=true case, the caller should stop normal main execution (typically
// by returning from main or calling os.Exit).
func Dispatch(ctx context.Context) (bool, error) {
	return dispatch(ctx)
}

// Init checks whether the current process was invoked under a registered name
// (based on filepath.Base(os.Args[0])).
//
// If a matching entrypoint is found, it is executed and Init returns true. In
// that case, the caller should stop normal main execution. If no match is found,
// Init returns false and normal execution should continue.
//
// Init is provided for compatibility with older callers. New code should
// prefer [Dispatch], which also return the entrypoint's error and allows
// the caller to decide how to report failures.
func Init() bool {
	ok, _ := dispatch(context.Background())
	return ok
}

// ExitCode conversion for a dispatched entrypoint's terminal state.
const (
	// ExitSuccess is the process status for an entrypoint that returned nil.
	ExitSuccess = 0
	// ExitFailure is the process status for an entrypoint that returned an
	// error and whose error did not carry an explicit exit code.
	ExitFailure = 1
)

// RunEntrypoint executes the entrypoint selected by the current process's
// argv[0] and terminates the process with a status that reflects the
// entrypoint's outcome. It reports whether an entrypoint was selected.
//
// It is the child-side counterpart of [Start] and [Run]: the child publishes a
// single terminal exit status, so the parent can tell success (0) from failure
// (non-zero) and can distinguish a normal exit from a signal (see
// [Result.Signaled]) without parsing output.
//
// If the entrypoint returns nil, the process exits with [ExitSuccess]. If it
// returns an error, RunEntrypoint prints it to stderr and exits with the code
// carried by a [*ExitError], or [ExitFailure]. If no entrypoint is selected,
// RunEntrypoint returns false and does not exit the process.
//
// Like [Dispatch], RunEntrypoint is meant to be called at the top of main; it
// does not return when it reports true.
func RunEntrypoint(ctx context.Context) bool {
	entrypoint, err := ResolveInit()
	if err != nil {
		return false
	}
	if runErr := entrypoint.Run(ctx); runErr != nil {
		// Match the message existing callers print when they handle Dispatch's
		// error themselves, so moving to RunEntrypoint does not change output.
		_, _ = fmt.Fprintln(os.Stderr, "entrypoint failed:", runErr)
		os.Exit(exitCodeFor(runErr))
	}
	os.Exit(ExitSuccess)
	return true
}

func init() {
	// Freeze the executable identity before anything else can run, so that a
	// later replacement of the on-disk binary cannot change the path used to
	// re-execute ourselves or to select an entrypoint.
	self.resolve()
}

func dispatch(ctx context.Context) (bool, error) {
	entrypoint, err := ResolveInit()
	if err != nil {
		// Not invoked as a registered entrypoint; this is a normal no-match.
		return false, nil
	}
	return true, entrypoint.Run(ctx)
}

// Command returns an [*exec.Cmd] configured to re-execute the current binary,
// using the path returned by [Self].
//
// The first element of args becomes argv[0] of the new process and is used by
// [Init] to select a registered entrypoint.
//
// On Linux, the Pdeathsig of [*exec.Cmd.SysProcAttr] is set to SIGTERM. This
// signal is sent to the child process when the OS thread that created it dies,
// which helps ensure the child does not outlive its parent unexpectedly. See
// [PR_SET_PDEATHSIG(2const)] and [go.dev/issue/27505] for details.
//
// It is the caller's responsibility to ensure that the creating thread is not
// terminated prematurely.
//
// [PR_SET_PDEATHSIG(2const)]: https://man7.org/linux/man-pages/man2/PR_SET_PDEATHSIG.2const.html
// [go.dev/issue/27505]: https://go.dev/issue/27505
func Command(args ...string) *exec.Cmd {
	return command(args...)
}

// CommandContext is like [Command] but includes a context. It uses
// [exec.CommandContext] under the hood.
//
// The provided context is used to interrupt the process
// (by calling cmd.Cancel or [os.Process.Kill])
// if the context becomes done before the command completes on its own.
//
// CommandContext sets the command's Cancel function to invoke the Kill method
// on its Process, and leaves its WaitDelay unset. The caller may change the
// cancellation behavior by modifying those fields before starting the command.
func CommandContext(ctx context.Context, args ...string) *exec.Cmd {
	return commandContext(ctx, args...)
}

// Self returns the executable path of the current process.
//
// On Linux, it returns "/proc/self/exe", which references the in-memory image
// of the running binary. This allows the on-disk binary (os.Args[0]) to be
// replaced or deleted without affecting re-execution.
//
// On other platforms, it returns the path captured at startup, with os.Args[0]
// resolved against the startup working directory and PATH and its symlink chain
// resolved once. Because the result does not change afterwards, a later chdir,
// PATH change, or replacement of the binary or one of its symlinks cannot
// redirect re-execution to a different file. [Executable] returns the same
// captured path on every platform.
func Self() string {
	return resolveSelf()
}

// naiveSelf is a separate function to allow testing in isolation on Linux. It
// resolves argv0 against the live process environment.
func naiveSelf(argv0 string) string {
	name := argv0
	if filepath.Base(name) == name {
		if lp, err := exec.LookPath(name); err == nil {
			return lp
		}
	}
	// handle conversion of relative paths to absolute
	if absName, err := filepath.Abs(name); err == nil {
		return absName
	}
	// if we couldn't get absolute name, return original
	// (NOTE: Go only errors on Abs() if os.Getwd fails)
	return name
}
