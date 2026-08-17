//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package studyauth

import "os"

const scopedTokenFileOwnershipSupported = false

// Unsupported ownership semantics fail closed instead of weakening the
// owner-only token-file invariant on an unreviewed platform.
func scopedTokenFileOwnedByEffectiveUser(_ os.FileInfo) bool {
	return false
}
