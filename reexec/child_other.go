//go:build !unix

package reexec

import "os"

// terminateProcess asks p to exit. Platforms without POSIX signals (Windows)
// use Kill, which is the only interrupt available there.
func terminateProcess(p *os.Process) error {
	if p == nil {
		return os.ErrProcessDone
	}
	return p.Kill()
}

// isKillSignal reports whether sig is the unconditional kill signal. Platforms
// without POSIX signals terminate with Kill only.
func isKillSignal(os.Signal) bool { return true }

// processSignal reports the signal that terminated ps. On platforms without
// POSIX wait statuses there is no signal to report.
func processSignal(_ *os.ProcessState) (os.Signal, bool) {
	return nil, false
}
