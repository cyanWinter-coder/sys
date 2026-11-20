package atomicwriter

import "errors"

// errExchangeUnsupported signals that the platform or filesystem cannot
// atomically exchange two directories and the journaled fallback must be
// used.
var errExchangeUnsupported = errors.New("atomic directory exchange is not supported")
