package tailnet

import "os"

// validateStateDirectory verifies the pre-enrolled identity directory through
// both path and descriptor views before tsnet construction. Keeping persisted
// enrollment as the only identity source is what makes ambient enrollment
// rejection useful rather than merely cosmetic.
func validateStateDirectory(stateDirectory string) error {
	return validateStateDirectoryWithOwner(stateDirectory, stateDirectoryOwnedByEffectiveUser)
}

type stateDirectoryOwnerValidator func(os.FileInfo) bool

func validateStateDirectoryWithOwner(
	stateDirectory string,
	ownerValidator stateDirectoryOwnerValidator,
) (returnError error) {
	if ownerValidator == nil {
		return stateDirectoryError("owner_validation_unavailable", nil)
	}

	pathInfoBeforeOpen, err := os.Lstat(stateDirectory)
	if err != nil {
		return stateDirectoryFilesystemError(err)
	}
	if pathInfoBeforeOpen.Mode()&os.ModeSymlink != 0 {
		return stateDirectoryError("symlink", nil)
	}
	if !pathInfoBeforeOpen.IsDir() {
		return stateDirectoryError("not_directory", nil)
	}

	directory, err := os.Open(stateDirectory)
	if err != nil {
		return stateDirectoryFilesystemError(err)
	}
	defer func() {
		if closeError := directory.Close(); closeError != nil && returnError == nil {
			returnError = stateDirectoryError("unavailable", closeError)
		}
	}()

	descriptorInfo, err := directory.Stat()
	if err != nil {
		return stateDirectoryError("unavailable", err)
	}
	pathInfoAfterOpen, err := os.Lstat(stateDirectory)
	if err != nil {
		return stateDirectoryFilesystemError(err)
	}
	if pathInfoAfterOpen.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(pathInfoBeforeOpen, descriptorInfo) ||
		!os.SameFile(descriptorInfo, pathInfoAfterOpen) {
		return stateDirectoryError("changed_during_validation", nil)
	}
	if !descriptorInfo.IsDir() {
		return stateDirectoryError("not_directory", nil)
	}
	const stateDirectoryAllowedMode = os.FileMode(0o700)
	stateDirectoryMode := descriptorInfo.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
	if stateDirectoryMode != stateDirectoryAllowedMode {
		return stateDirectoryError("permissions_not_owner_only", nil)
	}
	if !ownerValidator(descriptorInfo) {
		return stateDirectoryError("owner_mismatch", nil)
	}
	return nil
}

func stateDirectoryFilesystemError(err error) *Error {
	reason := "unavailable"
	if os.IsNotExist(err) {
		reason = "not_found"
	}
	return stateDirectoryError(reason, err)
}

func stateDirectoryError(reason string, cause error) *Error {
	return newError(
		ErrorCodeStateDirectoryInvalid,
		"native Tailnet state directory is invalid",
		map[string]string{"reason": reason},
		cause,
	)
}
