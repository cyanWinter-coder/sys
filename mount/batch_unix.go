//go:build !windows

package mount

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// MountSpec describes a single mount operation as part of a batch.
// The fields have the same meaning as the arguments of Mount.
type MountSpec struct {
	// Source is the device or path to mount. As with Mount, an empty
	// string or "none" identifies a remount request, which is not
	// supported in a batch (see ValidateMounts).
	Source string
	// Target is the mount point. It must be an absolute path to an
	// existing file or directory in the current mount namespace.
	Target string
	// FSType is the filesystem type (may be empty for bind mounts,
	// exactly as for Mount).
	FSType string
	// Options are mount options in the same format as Mount accepts:
	// "opt1=val1,opt2=val2". See flags_unix.go for supported flags.
	Options string
}

// BatchError is returned by MountAll when the batch could not be committed.
// The batch is transactional: every mount that was already committed by the
// batch (and the partially-applied failing mount, if any) is unmounted in
// reverse order before MountAll returns, so after an error the system is
// left as if MountAll had never been called.
type BatchError struct {
	// Index is the position of the failing spec in the batch.
	Index int
	// Spec is the spec that failed.
	Spec MountSpec
	// Err is the error returned by the failing mount.
	Err error
	// Rollback holds the errors (if any) encountered while unmounting
	// the already-committed mounts, in rollback (reverse) order. A nil
	// or empty slice means the rollback completed cleanly.
	Rollback []error
}

func (e *BatchError) Error() string {
	out := fmt.Sprintf("batch mount: entry %d (%s on %s, type %q, options %q) failed: %s",
		e.Index, e.Spec.Source, e.Spec.Target, e.Spec.FSType, e.Spec.Options, e.Err)
	if len(e.Rollback) > 0 {
		msgs := make([]string, len(e.Rollback))
		for i, err := range e.Rollback {
			msgs[i] = err.Error()
		}
		out += "; rollback incomplete: " + strings.Join(msgs, "; ")
	}
	return out
}

// Unwrap returns the error of the failing mount (not the rollback errors).
func (e *BatchError) Unwrap() error {
	return e.Err
}

// batchMu serializes batch transactions against Unmount: a batch either
// commits fully or is rolled back entirely, and a concurrent Unmount
// observes only one of those two terminal states, never a partial batch.
var batchMu sync.RWMutex

// ptypeOptions is the set of mount options selecting a propagation type.
// Only one of them may appear in a single spec.
var ptypeOptions = map[string]bool{
	"shared":      true,
	"rshared":     true,
	"private":     true,
	"rprivate":    true,
	"slave":       true,
	"rslave":      true,
	"unbindable":  true,
	"runbindable": true,
}

// ValidateMounts checks a batch of mount specs without performing any
// mount. It reports the first problem found: an empty or non-absolute
// target, a missing target or bind source, conflicting propagation flags
// within one spec, a remount request (which a transaction cannot roll
// back safely), or a duplicate target within the batch. A nil return
// means the batch passed validation; it does not guarantee the mounts
// will succeed at commit time.
func ValidateMounts(specs []MountSpec) error {
	seen := make(map[string]int, len(specs))
	for i, spec := range specs {
		if err := validateSpec(spec); err != nil {
			return fmt.Errorf("batch mount: entry %d: %w", i, err)
		}
		target := filepath.Clean(spec.Target)
		if prev, ok := seen[target]; ok {
			return fmt.Errorf("batch mount: entry %d: target %s duplicates entry %d", i, target, prev)
		}
		seen[target] = i
	}
	return nil
}

func validateSpec(spec MountSpec) error {
	if spec.Target == "" {
		return fmt.Errorf("target is empty")
	}
	if !filepath.IsAbs(spec.Target) {
		return fmt.Errorf("target %q is not an absolute path", spec.Target)
	}
	if _, err := os.Stat(spec.Target); err != nil {
		return fmt.Errorf("target %s: %w", spec.Target, err)
	}

	var propagation, binds int
	if spec.Options != "" {
		for _, o := range strings.Split(spec.Options, ",") {
			switch {
			case o == "":
				return fmt.Errorf("options %q contain an empty element", spec.Options)
			case o == "remount":
				return fmt.Errorf("remount is not supported in a batch: it modifies a pre-existing mount which cannot be rolled back")
			case o == "bind" || o == "rbind":
				binds++
			case ptypeOptions[o]:
				propagation++
			}
		}
	}
	if propagation > 1 {
		return fmt.Errorf("options %q contain conflicting propagation flags", spec.Options)
	}

	// A source of "" or "none" is treated as a remount request by the
	// mounter (see isremount), unless this is a bind mount, in which
	// case the source is a real path that must exist.
	if spec.Source == "" || spec.Source == "none" {
		if binds > 0 {
			return fmt.Errorf("bind mount requires a source path")
		}
		return fmt.Errorf("source %q implies a remount, which is not supported in a batch", spec.Source)
	}
	if binds > 0 {
		if _, err := os.Stat(spec.Source); err != nil {
			return fmt.Errorf("bind source %s: %w", spec.Source, err)
		}
	}
	return nil
}

// MountAll mounts a batch of filesystems transactionally: either every
// spec is committed, or the batch is rolled back entirely.
//
// All specs are validated up front (see ValidateMounts) before anything
// is mounted. The mounts are then committed in order; if any of them
// fails, the mounts committed so far (including any partially-applied
// state of the failing one) are unmounted in reverse order, and a
// *BatchError is returned describing both the failure and any rollback
// errors. Because a failure leaves no trace behind, MountAll can simply
// be retried once the cause is fixed.
//
// MountAll is serialized against Unmount: a concurrent Unmount either
// runs entirely before or entirely after the batch, so it can never
// observe or disrupt a half-committed transaction.
//
// Propagation flags (shared, private, slave, unbindable and their
// recursive variants) are applied per Linux semantics: they are set on
// the new mount only, in the caller's current mount namespace, via a
// separate change-propagation call (the same two-step sequence Mount
// uses). Rollback uses lazy unmounts, which likewise affect only the
// current namespace; mounts that propagated to peer mounts of a shared
// subtree are detached by the kernel according to the usual propagation
// rules. Specs that would modify pre-existing mounts (remounts) are
// rejected by validation, since such changes cannot be rolled back.
func MountAll(specs []MountSpec) error {
	if err := ValidateMounts(specs); err != nil {
		return err
	}

	batchMu.Lock()
	defer batchMu.Unlock()

	committed := make([]string, 0, len(specs))
	for i, spec := range specs {
		flag, data := parseOptions(spec.Options)
		if err := mount(spec.Source, spec.Target, spec.FSType, uintptr(flag), data); err != nil {
			// The failing mount may have been partially applied
			// (e.g. the initial mount succeeded but applying the
			// propagation type failed), so unmount its target too.
			rollback := rollbackMounts(append(committed, spec.Target))
			return &BatchError{
				Index:    i,
				Spec:     spec,
				Err:      err,
				Rollback: rollback,
			}
		}
		committed = append(committed, spec.Target)
	}
	return nil
}

// rollbackMounts lazily unmounts targets in reverse order, aggregating
// any errors. It must be called with batchMu held (directly or via
// MountAll), so it uses the lock-free unmount.
func rollbackMounts(targets []string) []error {
	var errs []error
	for i := len(targets) - 1; i >= 0; i-- {
		if err := unmount(targets[i]); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}
