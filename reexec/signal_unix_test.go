//go:build unix

package reexec_test

import (
	"os"
	"os/signal"
	"syscall"
)

// selfTerminateSignal is the signal the signal-test child sends to itself.
func selfTerminateSignal() os.Signal { return syscall.SIGTERM }

// signalSelf sends sig to the current process.
func signalSelf(sig os.Signal) error {
	return syscall.Kill(os.Getpid(), sig.(syscall.Signal))
}

// ignoreTerminationSignals makes SIGTERM unactionable, so termination must
// escalate to SIGKILL.
func ignoreTerminationSignals() {
	signal.Ignore(syscall.SIGTERM)
}

// wantSelfTerminateSignal is what a self-terminating child reports.
func wantSelfTerminateSignal() os.Signal { return syscall.SIGTERM }

// wantKillSignal is the escalation signal used by the implementation.
func wantKillSignal() os.Signal { return syscall.SIGKILL }
