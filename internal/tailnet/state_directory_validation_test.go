package tailnet

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestValidateStateDirectoryAcceptsOwnerOnlyDirectory(t *testing.T) {
	stateDirectory := t.TempDir()
	if err := os.Chmod(stateDirectory, 0o700); err != nil {
		t.Fatalf("chmod state directory: %v", err)
	}
	if err := validateStateDirectory(stateDirectory); err != nil {
		t.Fatalf("validate state directory: %v", err)
	}
}

func TestValidateStateDirectoryRejectsMissingPath(t *testing.T) {
	err := validateStateDirectory(filepath.Join(t.TempDir(), "missing"))
	assertStateDirectoryFailure(t, err, "not_found")
}

func TestValidateStateDirectoryRejectsRegularFile(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state-file")
	if err := os.WriteFile(stateFile, []byte("state-file-canary"), 0o600); err != nil {
		t.Fatalf("write state file: %v", err)
	}
	err := validateStateDirectory(stateFile)
	assertStateDirectoryFailure(t, err, "not_directory")
}

func TestValidateStateDirectoryRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ordinary Windows test users cannot reliably create symlinks")
	}
	targetDirectory := t.TempDir()
	symlinkPath := filepath.Join(t.TempDir(), "state-link")
	if err := os.Symlink(targetDirectory, symlinkPath); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	err := validateStateDirectory(symlinkPath)
	assertStateDirectoryFailure(t, err, "symlink")
}

func TestValidateStateDirectoryRejectsGroupOrWorldPermissions(t *testing.T) {
	stateDirectory := t.TempDir()
	if err := os.Chmod(stateDirectory, 0o750); err != nil {
		t.Fatalf("chmod state directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateDirectory, 0o700) })
	err := validateStateDirectory(stateDirectory)
	assertStateDirectoryFailure(t, err, "permissions_not_owner_only")
}

func TestValidateStateDirectoryRejectsOwnerWithoutFullDirectoryAccess(t *testing.T) {
	stateDirectory := t.TempDir()
	if err := os.Chmod(stateDirectory, 0o600); err != nil {
		t.Fatalf("chmod state directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateDirectory, 0o700) })
	err := validateStateDirectory(stateDirectory)
	assertStateDirectoryFailure(t, err, "permissions_not_owner_only")
}

func TestValidateStateDirectoryRejectsSpecialPermissionBits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix directory special permission bits")
	}
	stateDirectory := t.TempDir()
	if err := os.Chmod(stateDirectory, 0o700|os.ModeSticky); err != nil {
		t.Fatalf("chmod state directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateDirectory, 0o700) })
	info, err := os.Lstat(stateDirectory)
	if err != nil {
		t.Fatalf("stat state directory: %v", err)
	}
	if info.Mode()&os.ModeSticky == 0 {
		t.Skip("filesystem did not retain the sticky permission bit")
	}

	err = validateStateDirectory(stateDirectory)
	assertStateDirectoryFailure(t, err, "permissions_not_owner_only")
}

func TestValidateStateDirectoryRejectsEffectiveOwnerMismatch(t *testing.T) {
	stateDirectory := testStateDirectory(t)
	err := validateStateDirectoryWithOwner(stateDirectory, func(os.FileInfo) bool { return false })
	assertStateDirectoryFailure(t, err, "owner_mismatch")
}

func TestValidateStateDirectoryRejectsUnavailableOwnerValidation(t *testing.T) {
	err := validateStateDirectoryWithOwner(testStateDirectory(t), nil)
	assertStateDirectoryFailure(t, err, "owner_validation_unavailable")
}

func TestStateDirectoryFailureRedactsPathAndFilesystemCause(t *testing.T) {
	secretPathComponent := "private-state-path-secret-canary"
	stateDirectory := filepath.Join(t.TempDir(), secretPathComponent)
	err := validateStateDirectory(stateDirectory)
	encoded, marshalError := json.Marshal(err)
	if marshalError != nil {
		t.Fatalf("marshal error: %v", marshalError)
	}
	for _, rendering := range []string{err.Error(), string(encoded)} {
		if strings.Contains(rendering, secretPathComponent) {
			t.Fatalf("state path leaked in rendering: %q", rendering)
		}
	}
}

func assertStateDirectoryFailure(t *testing.T, err error, reason string) {
	t.Helper()
	if !errors.Is(err, &Error{Code: ErrorCodeStateDirectoryInvalid}) {
		t.Fatalf("error = %v, want %s", err, ErrorCodeStateDirectoryInvalid)
	}
	var tailnetError *Error
	if !errors.As(err, &tailnetError) {
		t.Fatalf("error type = %T, want *Error", err)
	}
	if tailnetError.Context["reason"] != reason {
		t.Fatalf("reason = %q, want %q", tailnetError.Context["reason"], reason)
	}
}
