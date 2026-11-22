// Package sequential provides a set of functions for managing sequential
// files on Windows.
//
// The origin of these functions are the golang OS and windows packages,
// slightly modified to only cope with files, not directories due to the
// specific use case.
//
// The alteration is to allow a file on Windows to be opened with
// FILE_FLAG_SEQUENTIAL_SCAN (particular for docker load), to avoid eating
// the standby list, particularly when accessing large files such as layer.tar.
//
// For non-Windows platforms, the package provides wrappers for the equivalents
// in the os packages. They are passthrough on Unix platforms, and only relevant
// on Windows.
//
// # Versioned files
//
// Beyond the thin os wrappers, [NewFile] returns a [File]: a concurrency-safe
// wrapper that freezes the identity (version) of the opened file and chains
// resource ownership and write-barrier cleanup into a single state machine.
// A handle keeps serving the incarnation it opened even if the path is
// meanwhile replaced or removed, short writes are retried until the full
// buffer lands, write or sync failures poison later writes, and Close drives
// the underlying handle to a unique terminal state. The wrapper and its
// error shapes are identical on Windows and Unix.
package sequential
