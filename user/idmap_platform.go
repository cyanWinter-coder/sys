package user

import "runtime"

// idMappingSupported reports whether the platform supports Linux
// user-namespace ID mappings (uid_map/gid_map). It is a variable so tests can
// exercise the unsupported-platform behavior on any host.
var idMappingSupported = runtime.GOOS == "linux"
