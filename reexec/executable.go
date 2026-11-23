package reexec

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/moby/sys/reexec/internal/reexecoverride"
)

// selfSnapshot freezes the executable path at package initialization.
//
// Resolution depends on the startup working directory, PATH, and the on-disk
// symlink chain. Recomputing it on every call would let a later chdir, a PATH
// change, or a replacement of the binary or its symlink silently redirect
// re-execution to a different file. Capturing the result once means every
// re-exec and dispatch decision in the process uses the same executable
// identity, so a handler can never be resolved against a binary other than the
// one this process was started from.
type selfSnapshot struct {
	once sync.Once
	exe  string
}

var self selfSnapshot

func (s *selfSnapshot) resolve() {
	s.once.Do(func() {
		if runtime.GOOS == "linux" {
			// /proc/self/exe refers to the in-memory image, which is inherently
			// immune to replacement of the on-disk binary. Resolve the symlink so
			// the captured identity names the file the image came from, falling
			// back to the link target if it has since been deleted.
			if p, err := os.Readlink("/proc/self/exe"); err == nil && filepath.IsAbs(p) {
				if real, err := filepath.EvalSymlinks(p); err == nil {
					p = real
				}
				s.exe = filepath.Clean(p)
				return
			}
		}
		s.exe = evalNaive(os.Args[0])
	})
}

// evalNaive resolves argv0 against the live environment and then resolves the
// symlink chain once, so a later replacement of the link cannot change the
// result. If the chain cannot be resolved, the absolute path is returned.
func evalNaive(argv0 string) string {
	resolved := naiveSelf(argv0)
	if real, err := filepath.EvalSymlinks(resolved); err == nil {
		return real
	}
	return resolved
}

// Executable returns the path of the running executable as captured at package
// initialization, with symbolic links resolved.
//
// Unlike [Self], the returned value never changes after startup, so it can be
// compared across calls to detect that the on-disk binary was replaced or
// moved.
func Executable() string {
	self.resolve()
	return self.exe
}

// resolveSelf returns the executable path to use for re-execution, honoring a
// test override of argv0.
func resolveSelf() string {
	if argv0, ok := reexecoverride.Argv0(); ok {
		// Test override: resolve the requested argv0 directly so tests control
		// exactly which value is observed.
		return naiveSelf(argv0)
	}
	if runtime.GOOS == "linux" {
		return "/proc/self/exe"
	}
	return Executable()
}
