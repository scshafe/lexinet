//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package studyauth

import (
	"os"
	"syscall"
	"testing"
	"time"
)

type scopedTokenOwnerFileInfo struct {
	ownerUID uint32
	mode     os.FileMode
}

func (info scopedTokenOwnerFileInfo) Name() string { return "scoped-token" }
func (info scopedTokenOwnerFileInfo) Size() int64  { return minimumScopedTokenBytes }
func (info scopedTokenOwnerFileInfo) Mode() os.FileMode {
	if info.mode == 0 {
		return 0o600
	}
	return info.mode
}
func (info scopedTokenOwnerFileInfo) ModTime() time.Time { return time.Time{} }
func (info scopedTokenOwnerFileInfo) IsDir() bool        { return false }
func (info scopedTokenOwnerFileInfo) Sys() any           { return &syscall.Stat_t{Uid: info.ownerUID} }

func TestScopedTokenFileOwnershipRequiresEffectiveUser(t *testing.T) {
	t.Parallel()
	effectiveUID := uint32(os.Geteuid())
	if !scopedTokenFileOwnedByEffectiveUser(scopedTokenOwnerFileInfo{ownerUID: effectiveUID}) {
		t.Fatal("effective-user-owned token file was rejected")
	}
	otherUID := effectiveUID + 1
	if otherUID == effectiveUID {
		otherUID--
	}
	if scopedTokenFileOwnedByEffectiveUser(scopedTokenOwnerFileInfo{ownerUID: otherUID}) {
		t.Fatal("differently owned token file was accepted")
	}
}

func TestScopedTokenFileInfoRejectsSpecialModeBits(t *testing.T) {
	t.Parallel()
	for _, specialMode := range []os.FileMode{os.ModeSetuid, os.ModeSetgid, os.ModeSticky} {
		if validScopedTokenFileInfo(scopedTokenOwnerFileInfo{
			ownerUID: uint32(os.Geteuid()),
			mode:     0o600 | specialMode,
		}) {
			t.Fatalf("special token-file mode %v was accepted", specialMode)
		}
	}
}
