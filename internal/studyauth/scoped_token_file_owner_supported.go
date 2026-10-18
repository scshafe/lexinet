//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package studyauth

import (
	"os"
	"syscall"
)

const scopedTokenFileOwnershipSupported = true

func scopedTokenFileOwnedByEffectiveUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat != nil && uint64(stat.Uid) == uint64(os.Geteuid())
}
