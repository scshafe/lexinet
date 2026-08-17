//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package tailnet

import (
	"os"
	"syscall"
)

func stateDirectoryOwnedByEffectiveUser(info os.FileInfo) bool {
	status, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint64(status.Uid) == uint64(os.Geteuid())
}
