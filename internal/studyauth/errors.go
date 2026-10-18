package studyauth

import "lexinet/internal/model"

var authenticationSafeErrorMessages = map[model.StudyErrorCode]string{
	model.StudyErrorCodeSchemaInvalid:       "study authentication configuration is invalid",
	model.StudyErrorCodeUnauthorized:        "study authentication is required",
	model.StudyErrorCodeAuthorizationDenied: "study operation is not authorized",
	model.StudyErrorCodeServiceUnavailable:  "study authentication is temporarily unavailable",
	model.StudyErrorCodeInternal:            "study authentication failed",
}

func newAuthenticationError(code model.StudyErrorCode) *model.StudyError {
	message, ok := authenticationSafeErrorMessages[code]
	if !ok {
		code = model.StudyErrorCodeInternal
		message = authenticationSafeErrorMessages[code]
	}
	return &model.StudyError{
		Code:      code,
		Message:   message,
		Retryable: code == model.StudyErrorCodeServiceUnavailable,
	}
}

func newAuthenticationConfigurationError(field string) *model.StudyError {
	err := newAuthenticationError(model.StudyErrorCodeSchemaInvalid)
	err.Context = map[string]string{
		"operation": tailnetStudyAuthenticationConfigurationOperation,
		"field":     field,
	}
	return err
}

func newAuthenticationRequestError(code model.StudyErrorCode) *model.StudyError {
	err := newAuthenticationError(code)
	err.Context = map[string]string{
		"operation": tailnetStudyAuthenticationRequestOperation,
	}
	return err
}
