package studysession

import (
	"context"
	"errors"

	"lexinet/internal/model"
)

var studySafeErrorMessages = map[model.StudyErrorCode]string{
	model.StudyErrorCodeSchemaInvalid:        "study request is invalid",
	model.StudyErrorCodeUnauthorized:         "study authentication is required",
	model.StudyErrorCodeAuthorizationDenied:  "study operation is not authorized",
	model.StudyErrorCodeProfileNotFound:      "study learner profile was not found",
	model.StudyErrorCodeSessionNotFound:      "study session was not found",
	model.StudyErrorCodeIdempotencyConflict:  "study idempotency identity conflicts with prior input",
	model.StudyErrorCodeCycleNotExhausted:    "study predecessor cycle is not exhausted",
	model.StudyErrorCodeFlushReceiptRequired: "study predecessor flush receipt is required",
	model.StudyErrorCodeFlushReceiptStale:    "study predecessor flush receipt is stale",
	model.StudyErrorCodeStateStale:           "study state is stale",
	model.StudyErrorCodeStreamSequenceGap:    "study client stream sequence is not contiguous",
	model.StudyErrorCodeRequestTooLarge:      "study request exceeds a protocol bound",
	model.StudyErrorCodeRateLimited:          "study service admission is saturated",
	model.StudyErrorCodeServiceUnavailable:   "study service is temporarily unavailable",
	model.StudyErrorCodeInternal:             "study service failed",
}

// StudyPublicError is the complete closed error descriptor exposed to protocol
// adapters. It deliberately cannot carry Cause, Context, or repository text.
type StudyPublicError struct {
	Code      model.StudyErrorCode `json:"code"`
	Message   string               `json:"message"`
	Retryable bool                 `json:"retryable"`
}

// PublicStudyError converts any failure into one allowlisted public descriptor.
// Unknown and nil failures fail closed as INTERNAL.
func PublicStudyError(err error) StudyPublicError {
	sanitized := sanitizeStudySessionError(err)
	if sanitized == nil {
		sanitized = newStudySessionError(model.StudyErrorCodeInternal)
	}
	studyError, ok := sanitized.(*model.StudyError)
	if !ok || studyError == nil {
		studyError = newStudySessionError(model.StudyErrorCodeInternal)
	}
	return StudyPublicError{
		Code:      studyError.Code,
		Message:   studyError.Message,
		Retryable: studyError.Retryable,
	}
}

func newStudySessionError(code model.StudyErrorCode) *model.StudyError {
	message, ok := studySafeErrorMessages[code]
	if !ok {
		code = model.StudyErrorCodeInternal
		message = studySafeErrorMessages[code]
	}
	return &model.StudyError{
		Code:      code,
		Message:   message,
		Retryable: code == model.StudyErrorCodeRateLimited || code == model.StudyErrorCodeServiceUnavailable,
	}
}

func sanitizeStudySessionError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return newStudySessionError(model.StudyErrorCodeServiceUnavailable)
	}
	var studyError *model.StudyError
	if errors.As(err, &studyError) && studyError != nil {
		if _, ok := studySafeErrorMessages[studyError.Code]; ok {
			return newStudySessionError(studyError.Code)
		}
	}
	return newStudySessionError(model.StudyErrorCodeInternal)
}
