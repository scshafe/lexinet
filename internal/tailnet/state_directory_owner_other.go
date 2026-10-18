//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package tailnet

import "os"

// Platforms without a stable effective-UID/FileInfo contract fail closed.
func stateDirectoryOwnedByEffectiveUser(os.FileInfo) bool {
	return false
}
