//go:build unix

package reexec

import (
	"os"
	"syscall"
)

// terminateProcess asks p to exit. SIGTERM lets the child run its own cleanup;
// callers that need an unconditional stop use [Process.Stop], which escalates
// to SIGKILL after the grace period.
func terminateProcess(p *os.Process) error {
	if p == nil {
		return os.ErrProcessDone
	}
	return p.Signal(syscall.SIGTERM)
}

// isKillSignal reports whether sig is the unconditional kill signal.
func isKillSignal(sig os.Signal) bool { return sig == syscall.SIGKILL }

// processSignal reports the signal that terminated ps, if any.
func processSignal(ps *os.ProcessState) (os.Signal, bool) {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return ws.Signal(), true
	}
	return nil, false
}
