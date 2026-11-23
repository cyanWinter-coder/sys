//go:build !unix

package reexec_test

import "os"

// selfTerminateSignal returns nil on platforms without POSIX signals.
func selfTerminateSignal() os.Signal { return nil }

// signalSelf is unreachable because selfTerminateSignal returns nil there.
func signalSelf(os.Signal) error { return nil }

// ignoreTerminationSignals has no equivalent on platforms without POSIX
// signals; cancellation falls back to Kill.
func ignoreTerminationSignals() {}

// wantSelfTerminateSignal is unused on this platform; the signal-status tests
// are skipped.
func wantSelfTerminateSignal() os.Signal { return nil }

// wantKillSignal is unused on this platform; the signal-status tests are
// skipped.
func wantKillSignal() os.Signal { return nil }
