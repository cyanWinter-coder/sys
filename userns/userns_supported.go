package userns

import "runtime"

// Supported reports whether user namespaces can be probed on the current
// platform. It is true on Linux and false elsewhere.
//
// Code that needs to make decisions based on user-namespace membership should
// check Supported first: on unsupported platforms [RunningInUserNS] has no way
// to observe a namespace and would otherwise have to guess.
func Supported() bool {
	return runtime.GOOS == "linux"
}
