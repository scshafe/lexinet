package studyauth

import (
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
)

var errScopedTokenFileInvalid = errors.New("scoped token file is invalid")

func readScopedTokenDigest(path string) ([sha256.Size]byte, error) {
	var zeroDigest [sha256.Size]byte
	if path == "" || !filepath.IsAbs(path) {
		return zeroDigest, errScopedTokenFileInvalid
	}

	pathInfo, err := os.Lstat(path)
	if err != nil || !validScopedTokenFileInfo(pathInfo) {
		return zeroDigest, errScopedTokenFileInvalid
	}

	file, err := os.Open(path)
	if err != nil {
		return zeroDigest, errScopedTokenFileInvalid
	}
	fileInfo, statErr := file.Stat()
	if statErr != nil || !os.SameFile(pathInfo, fileInfo) || !validScopedTokenFileInfo(fileInfo) {
		_ = file.Close()
		return zeroDigest, errScopedTokenFileInvalid
	}

	tokenFileBytes, readErr := io.ReadAll(io.LimitReader(file, maximumScopedTokenFileBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		wipeScopedTokenBytes(tokenFileBytes)
		return zeroDigest, errScopedTokenFileInvalid
	}
	defer wipeScopedTokenBytes(tokenFileBytes)
	if len(tokenFileBytes) == 0 || len(tokenFileBytes) > maximumScopedTokenFileBytes {
		return zeroDigest, errScopedTokenFileInvalid
	}

	tokenBytes := tokenFileBytes
	if tokenBytes[len(tokenBytes)-1] == '\n' {
		tokenBytes = tokenBytes[:len(tokenBytes)-1]
	}
	if !validScopedTokenBytes(tokenBytes) {
		return zeroDigest, errScopedTokenFileInvalid
	}
	return sha256.Sum256(tokenBytes), nil
}

func validScopedTokenFileInfo(info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() || !scopedTokenFileOwnedByEffectiveUser(info) {
		return false
	}
	if info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return false
	}
	permissions := info.Mode().Perm()
	return permissions == 0o400 || permissions == 0o600
}

func validScopedTokenBytes(tokenBytes []byte) bool {
	if len(tokenBytes) < minimumScopedTokenBytes || len(tokenBytes) > maximumScopedTokenBytes {
		return false
	}
	for _, value := range tokenBytes {
		if !validScopedTokenByte(value) {
			return false
		}
	}
	return true
}

func validScopedTokenString(token string) bool {
	if len(token) < minimumScopedTokenBytes || len(token) > maximumScopedTokenBytes {
		return false
	}
	for index := 0; index < len(token); index++ {
		if !validScopedTokenByte(token[index]) {
			return false
		}
	}
	return true
}

func validScopedTokenByte(value byte) bool {
	return value >= 0x21 && value <= 0x7e
}

func wipeScopedTokenBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
